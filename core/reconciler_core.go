package core

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// RejectionReason indicates why a commit was dropped.
type RejectionReason int

const (
	NotRejected RejectionReason = iota
	RejectedConflict
	RejectedInvalidSignature
	RejectedFetchFailed
	RejectedClockSkew
)

// errMainAdvancedDuringReplay indicates that main branch advanced while we were building a replay result.
// Callers should restart from the new merge base (never reset main back in time mid-replay).
var errMainAdvancedDuringReplay = fmt.Errorf("main branch advanced during replay")

type sqlRunner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// CommitRejectedCallback is called when a commit is dropped.
type CommitRejectedCallback func(ad *CommitAd, reason RejectionReason)

// Reconciler is a local deterministic linearizer. It does not perform any networking.
type Reconciler struct {
	mu  sync.Mutex
	db  *DB
	hlc *HLC

	log *logrus.Entry

	onCommitRejectedInternal CommitRejectedCallback
	onCommitRejected         CommitRejectedCallback
}

func NewReconciler(db *DB, peerID string, log *logrus.Entry) *Reconciler {
	return &Reconciler{
		db:  db,
		hlc: NewHLC(peerID),
		log: log.WithField("component", "reconciler"),
	}
}

// setCommitRejectedInternalHook registers an internal callback invoked for rejected commits.
// It is intended for the Node to keep its local CommitIndex consistent (e.g. mark conflict drops as handled).
func (r *Reconciler) setCommitRejectedInternalHook(cb CommitRejectedCallback) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onCommitRejectedInternal = cb
}

func (r *Reconciler) SetCommitRejectedCallback(cb CommitRejectedCallback) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onCommitRejected = cb
}

func (r *Reconciler) Stop() {}

func (r *Reconciler) GetHLC() *HLC { return r.hlc }

// ReplayImportedCommits replays main from mergeBase by performing deterministic merges.
//
// Imported commits are expected to be present in the local object store (via Dolt fetch).
// Only user commits are merged; merge commits are treated as derived and ignored.
//
// If finalizedBase is non-nil, commits at or before the finalized HLC are filtered out.
// SAFETY: When finalizedBase is set, the effective base for replay is the finalized base
// commit hash, not the merge base. This ensures we never rewrite finalized history.
func (r *Reconciler) ReplayImportedCommits(ctx context.Context, mergeBase string, imported []Commit, finalizedBase *FinalizedBase) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	head, err := r.db.GetLastCommit("main")
	if err != nil {
		return 0, err
	}

	// Determine effective base:
	// - If finalizedBase is set, use it as the base (never replay finalized commits)
	// - Otherwise, use the merge base
	effectiveBase := mergeBase
	if finalizedBase != nil && finalizedBase.CommitHash != "" {
		effectiveBase = finalizedBase.CommitHash
		r.log.Debugf("Using finalized base %s (HLC=%s) as effective base instead of merge base %s",
			finalizedBase.CommitHash[:8], finalizedBase.HLC, mergeBase[:8])
	}

	localCommits, err := r.getCommitsSince(ctx, r.db.sqldb, "main", effectiveBase)
	if err != nil {
		return 0, fmt.Errorf("failed to get local commits: %w", err)
	}

	userCommits := r.collectUserCommits(localCommits, imported, finalizedBase)
	if len(userCommits) == 0 {
		return 0, nil
	}

	return r.replayToTempBranchThenUpdateMain(ctx, effectiveBase, head.Hash, userCommits)
}

func deduplicateByHLCDeterministic(commits []commitWithMeta) []commitWithMeta {
	byKey := make(map[string]commitWithMeta)

	for _, cwm := range commits {
		if cwm.hlc.IsZero() {
			key := "hash:" + cwm.commit.Hash
			existing, ok := byKey[key]
			if !ok || cwm.commit.Hash < existing.commit.Hash {
				byKey[key] = cwm
			}
			continue
		}

		key := "hlc:" + cwm.hlc.String()

		existing, ok := byKey[key]
		if !ok || cwm.commit.Hash < existing.commit.Hash {
			byKey[key] = cwm
		}
	}

	result := make([]commitWithMeta, 0, len(byKey))
	for _, v := range byKey {
		result = append(result, v)
	}
	return result
}

func (r *Reconciler) collectUserCommits(local []Commit, imported []Commit, finalizedBase *FinalizedBase) []commitWithMeta {
	allCommits := make([]commitWithMeta, 0, len(local)+len(imported))
	for _, c := range local {
		meta, err := ParseCommitMetadata(c.Message)
		if err != nil || meta == nil || meta.Kind != CommitKindUser {
			continue
		}
		if finalizedBase != nil && !finalizedBase.HLC.IsZero() && !finalizedBase.HLC.Less(meta.HLC) {
			continue
		}
		allCommits = append(allCommits, commitWithMeta{commit: c, hlc: meta.HLC})
	}
	for _, c := range imported {
		meta, err := ParseCommitMetadata(c.Message)
		if err != nil || meta == nil || meta.Kind != CommitKindUser {
			continue
		}
		if finalizedBase != nil && !finalizedBase.HLC.IsZero() && !finalizedBase.HLC.Less(meta.HLC) {
			continue
		}
		allCommits = append(allCommits, commitWithMeta{commit: c, hlc: meta.HLC})
	}

	allCommits = deduplicateByHLCDeterministic(allCommits)
	sortCommitsByHLC(allCommits)
	return allCommits
}

// replayToTempBranchThenUpdateMain deterministically rebuilds history by sorting commits
// by HLC and merging one at a time, then updates main if it hasn't advanced concurrently.
func (r *Reconciler) replayToTempBranchThenUpdateMain(ctx context.Context, base string, mainHeadBefore string, commits []commitWithMeta) (int, error) {
	conn, err := r.db.sqldb.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get sql connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	_, err = conn.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-f', 'replay_tmp', '%s');", base))
	if err != nil {
		return 0, fmt.Errorf("failed to create temp branch: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "CALL DOLT_CHECKOUT('main');")
		_, _ = conn.ExecContext(context.Background(), "CALL DOLT_BRANCH('-D', 'replay_tmp');")
	}()

	_, err = conn.ExecContext(ctx, "CALL DOLT_CHECKOUT('replay_tmp');")
	if err != nil {
		return 0, fmt.Errorf("failed to checkout temp branch: %w", err)
	}

	headHash := base
	mergeCommits := 0

	// Sort by HLC and merge one at a time (matching spec's deterministic approach)
	for _, cwm := range commits {
		// Try fast-forward first
		parent, err := r.getFirstParentHash(ctx, conn, cwm.commit.Hash)
		if err == nil && headHash == parent {
			if err := r.fastForwardToCommit(ctx, conn, cwm.commit.Hash); err != nil {
				return 0, err
			}
			headHash, err = r.getHeadHash(ctx, conn)
			if err != nil {
				return 0, err
			}
			continue
		}

		// Merge
		_, mergeErr := conn.ExecContext(ctx, fmt.Sprintf("CALL DOLT_MERGE('%s');", cwm.commit.Hash))
		if mergeErr != nil {
			if isConflictError(mergeErr) {
				r.log.Warnf("Conflict merging %s - dropping", cwm.commit.Hash)
				_, _ = conn.ExecContext(ctx, "CALL DOLT_MERGE('--abort');")
				ad := &CommitAd{
					PeerID:     cwm.hlc.PeerID,
					HLC:        cwm.hlc,
					CommitHash: cwm.commit.Hash,
				}
				if r.onCommitRejectedInternal != nil {
					r.onCommitRejectedInternal(ad, RejectedConflict)
				}
				if r.onCommitRejected != nil {
					r.onCommitRejected(ad, RejectedConflict)
				}
				continue
			}
			return 0, fmt.Errorf("failed to merge %s: %w", cwm.commit.Hash, mergeErr)
		}

		// Amend merge commit message with metadata
		author := "doltswarm"
		email := "doltswarm@local"
		mergeDate := time.Now()
		meta := NewCommitMetadata("merge", cwm.hlc, "", author, email, mergeDate)
		metaJSON, err := meta.Marshal()
		if err != nil {
			return 0, err
		}

		_, err = conn.ExecContext(ctx, fmt.Sprintf(
			"CALL DOLT_COMMIT('--amend', '-m', '%s', '--author', '%s <%s>', '--date', '%s');",
			escapeSQL(metaJSON),
			escapeSQL(author),
			escapeSQL(email),
			mergeDate.Format(time.RFC3339Nano),
		))
		if err != nil {
			return 0, err
		}

		mergeCommits++
		headHash, err = r.getHeadHash(ctx, conn)
		if err != nil {
			return 0, err
		}
	}

	_, err = conn.ExecContext(ctx, "CALL DOLT_CHECKOUT('main');")
	if err != nil {
		return 0, fmt.Errorf("failed to checkout main: %w", err)
	}

	curHead, err := r.db.GetLastCommit("main")
	if err != nil {
		return 0, fmt.Errorf("failed to read main head: %w", err)
	}
	if curHead.Hash != mainHeadBefore {
		return 0, errMainAdvancedDuringReplay
	}

	_, err = conn.ExecContext(ctx, "CALL DOLT_RESET('--hard', 'replay_tmp');")
	if err != nil {
		return 0, fmt.Errorf("failed to move main to replay_tmp: %w", err)
	}

	return mergeCommits, nil
}

func (r *Reconciler) fastForwardToCommit(ctx context.Context, q sqlRunner, commitHash string) error {
	_, err := q.ExecContext(ctx, fmt.Sprintf("CALL DOLT_RESET('--hard', '%s');", commitHash))
	return err
}

func (r *Reconciler) getFirstParentHash(ctx context.Context, q sqlRunner, commitHash string) (string, error) {
	row, err := q.QueryContext(ctx, "SELECT parent_hash FROM dolt_commit_ancestors WHERE commit_hash = ? AND parent_index = 0 LIMIT 1", commitHash)
	if err != nil {
		return "", err
	}
	defer row.Close()
	if row.Next() {
		var parent sql.NullString
		if err := row.Scan(&parent); err != nil {
			return "", err
		}
		if parent.Valid {
			return parent.String, nil
		}
		return "", nil
	}
	return "", fmt.Errorf("no parent found for commit %s", commitHash)
}

func (r *Reconciler) getHeadHash(ctx context.Context, q sqlRunner) (string, error) {
	row := q.QueryRowContext(ctx, "SELECT HASHOF('HEAD');")
	var hash string
	if err := row.Scan(&hash); err != nil {
		return "", err
	}
	return hash, nil
}

func (r *Reconciler) getBranchHeadHash(ctx context.Context, q sqlRunner, branch string) (string, error) {
	row := q.QueryRowContext(ctx, fmt.Sprintf("SELECT HASHOF('%s');", escapeSQL(branch)))
	var hash string
	if err := row.Scan(&hash); err == nil {
		return hash, nil
	} else if err != sql.ErrNoRows && !strings.Contains(err.Error(), "no rows") {
		return "", err
	}

	// Fallback for remotes or older Dolt versions where HASHOF doesn't resolve remote refs.
	var out sql.NullString
	err := q.QueryRowContext(ctx, "SELECT hash FROM dolt_branches WHERE name = ? LIMIT 1;", branch).Scan(&out)
	if err == nil {
		if out.Valid {
			return out.String, nil
		}
		return "", nil
	}
	if err != sql.ErrNoRows && !strings.Contains(err.Error(), "no rows") {
		return "", err
	}

	out = sql.NullString{}
	err = q.QueryRowContext(ctx, "SELECT hash FROM dolt_remote_branches WHERE name = ? LIMIT 1;", branch).Scan(&out)
	if err == nil {
		if out.Valid {
			return out.String, nil
		}
		return "", nil
	}
	if err == sql.ErrNoRows || strings.Contains(err.Error(), "no rows") {
		return "", nil
	}
	return "", err
}

func (r *Reconciler) getCommitByHash(ctx context.Context, q sqlRunner, commitHash string) (Commit, error) {
	var c Commit
	row := q.QueryRowContext(ctx,
		"SELECT commit_hash, committer, email, date, message FROM dolt_commits WHERE commit_hash = ? LIMIT 1;",
		commitHash,
	)
	if err := row.Scan(&c.Hash, &c.Committer, &c.Email, &c.Date, &c.Message); err == nil {
		return c, nil
	} else if err != sql.ErrNoRows && !strings.Contains(strings.ToLower(err.Error()), "no rows") &&
		!strings.Contains(strings.ToLower(err.Error()), "unknown table") &&
		!strings.Contains(strings.ToLower(err.Error()), "doesn't exist") &&
		!strings.Contains(strings.ToLower(err.Error()), "dolt_commits") {
		return Commit{}, err
	}

	// Fallback to dolt_log for older versions or missing dolt_commits.
	query := fmt.Sprintf(
		"SELECT commit_hash, committer, email, date, message FROM dolt_log('%s') LIMIT 1;",
		escapeSQL(commitHash),
	)
	row = q.QueryRowContext(ctx, query)
	if err := row.Scan(&c.Hash, &c.Committer, &c.Email, &c.Date, &c.Message); err != nil {
		return Commit{}, err
	}
	return c, nil
}

func (r *Reconciler) getParentHashes(ctx context.Context, q sqlRunner, commitHash string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT parent_hash FROM dolt_commit_ancestors WHERE commit_hash = ? ORDER BY parent_index ASC", commitHash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var parents []string
	for rows.Next() {
		var parent sql.NullString
		if err := rows.Scan(&parent); err != nil {
			return nil, err
		}
		if parent.Valid && parent.String != "" {
			parents = append(parents, parent.String)
		}
	}
	return parents, nil
}

type commitWithMeta struct {
	commit Commit
	hlc    HLCTimestamp
}

func (r *Reconciler) getCommitsSince(ctx context.Context, q sqlRunner, branch string, base string) ([]Commit, error) {
	head, err := r.getBranchHeadHash(ctx, q, branch)
	if err != nil {
		return nil, err
	}
	if head == "" || head == base {
		return nil, nil
	}

	seen := make(map[string]bool)
	queue := []string{head}
	var commits []Commit

	for len(queue) > 0 {
		hash := queue[0]
		queue = queue[1:]
		if hash == "" || hash == base || seen[hash] {
			continue
		}
		seen[hash] = true

		c, err := r.getCommitByHash(ctx, q, hash)
		if err != nil {
			return nil, err
		}
		commits = append(commits, c)

		parents, err := r.getParentHashes(ctx, q, hash)
		if err != nil {
			return nil, err
		}
		for _, parent := range parents {
			if parent == "" || parent == base {
				continue
			}
			if !seen[parent] {
				queue = append(queue, parent)
			}
		}
	}

	return commits, nil
}

func sortCommitsByHLC(commits []commitWithMeta) {
	sort.Slice(commits, func(i, j int) bool {
		hi, hj := commits[i].hlc, commits[j].hlc
		if hi.IsZero() && hj.IsZero() {
			return commits[i].commit.Hash < commits[j].commit.Hash
		}
		if hi.IsZero() {
			return false
		}
		if hj.IsZero() {
			return true
		}
		if hi.Equal(hj) {
			return commits[i].commit.Hash < commits[j].commit.Hash
		}
		return hi.Less(hj)
	})
}

func isConflictError(err error) bool {
	errStr := err.Error()
	return strings.Contains(errStr, "conflict") ||
		strings.Contains(errStr, "CONFLICT")
}
