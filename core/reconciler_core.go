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

// ReplayImported replays main from mergeBase, incorporating the provided imported commits.
//
// Imported commits are expected to be present in the local object store (e.g. via ImportBundle),
// so they can be cherry-picked by hash.
func (r *Reconciler) ReplayImported(ctx context.Context, mergeBase string, imported []Commit) error {
	if ctx == nil {
		ctx = context.Background()
	}

	head, err := r.db.GetLastCommit("main")
	if err != nil {
		return err
	}

	localCommits, err := r.getCommitsSince(ctx, r.db.sqldb, "main", mergeBase)
	if err != nil {
		return fmt.Errorf("failed to get local commits: %w", err)
	}

	allCommits := make([]commitWithMeta, 0, len(localCommits)+len(imported))
	for _, c := range localCommits {
		allCommits = append(allCommits, commitWithMeta{commit: c, sourceBranch: "main"})
	}
	for _, c := range imported {
		allCommits = append(allCommits, commitWithMeta{commit: c, sourceBranch: "import"})
	}

	allCommits = deduplicateByHLCDeterministic(allCommits)
	sortCommitsByHLC(allCommits)
	if len(allCommits) == 0 {
		return nil
	}

	return r.replayToTempBranchThenUpdateMain(ctx, mergeBase, head.Hash, allCommits)
}

func deduplicateByHLCDeterministic(commits []commitWithMeta) []commitWithMeta {
	byKey := make(map[string]commitWithMeta)

	for _, cwm := range commits {
		meta, err := ParseCommitMetadata(cwm.commit.Message)
		if err != nil || meta == nil {
			key := "hash:" + cwm.commit.Hash
			existing, ok := byKey[key]
			if !ok || cwm.commit.Hash < existing.commit.Hash {
				byKey[key] = cwm
			}
			continue
		}

		cwm.hlc = meta.HLC
		cwm.originPeerID = meta.HLC.PeerID
		key := "hlc:" + meta.HLC.String()

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

// replayToTempBranchThenUpdateMain deterministically rebuilds the post-mergeBase history on a temp branch,
// then updates `main` to match that temp branch if (and only if) `main` hasn't advanced concurrently.
//
// Important: this is a history-rewrite operation by design (linearization). The temp branch is based on
// mergeBase, commits are cherry-picked in HLC order, and then `main` is moved to the rebuilt tip.
func (r *Reconciler) replayToTempBranchThenUpdateMain(ctx context.Context, mergeBase string, mainHeadBefore string, allCommits []commitWithMeta) error {
	conn, err := r.db.sqldb.Conn(ctx)
	if err != nil {
		return fmt.Errorf("failed to get sql connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	_, err = conn.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-f', 'replay_tmp', '%s');", mergeBase))
	if err != nil {
		return fmt.Errorf("failed to create temp branch: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "CALL DOLT_CHECKOUT('main');")
		_, _ = conn.ExecContext(context.Background(), "CALL DOLT_BRANCH('-D', 'replay_tmp');")
	}()

	_, err = conn.ExecContext(ctx, "CALL DOLT_CHECKOUT('replay_tmp');")
	if err != nil {
		return fmt.Errorf("failed to checkout temp branch: %w", err)
	}

	for _, cwm := range allCommits {
		err = r.cherryPickWithOriginalMetadata(ctx, conn, cwm)
		if err != nil {
			if isConflictError(err) {
				r.log.Warnf("Conflict cherry-picking %s - dropping (FWW)", cwm.commit.Hash)
				_, _ = conn.ExecContext(ctx, "CALL DOLT_CHERRY_PICK('--abort');")

				meta, _ := ParseCommitMetadata(cwm.commit.Message)
				if meta != nil {
					ad := &CommitAd{
						PeerID:     meta.HLC.PeerID,
						HLC:        meta.HLC,
						CommitHash: cwm.commit.Hash,
					}
					if r.onCommitRejectedInternal != nil {
						r.onCommitRejectedInternal(ad, RejectedConflict)
					}
					if r.onCommitRejected != nil {
						r.onCommitRejected(ad, RejectedConflict)
					}
				}
				continue
			}
			return fmt.Errorf("failed to cherry-pick %s: %w", cwm.commit.Hash, err)
		}
	}

	_, err = conn.ExecContext(ctx, "CALL DOLT_CHECKOUT('main');")
	if err != nil {
		return fmt.Errorf("failed to checkout main: %w", err)
	}

	curHead, err := r.db.GetLastCommit("main")
	if err != nil {
		return fmt.Errorf("failed to read main head: %w", err)
	}
	if curHead.Hash != mainHeadBefore {
		return errMainAdvancedDuringReplay
	}

	// Atomically update `main` to the rebuilt history.
	//
	// This is intentionally performed as the last step, after `mainHeadBefore` validation, so crashes or
	// errors during replay do not corrupt `main`.
	_, err = conn.ExecContext(ctx, "CALL DOLT_RESET('--hard', 'replay_tmp');")
	if err != nil {
		return fmt.Errorf("failed to move main to replay_tmp: %w", err)
	}

	return nil
}

func (r *Reconciler) cherryPickWithOriginalMetadata(ctx context.Context, q sqlRunner, cwm commitWithMeta) error {
	isMerge, err := r.isMergeCommit(ctx, q, cwm.commit.Hash)
	if err != nil {
		return err
	}
	if isMerge {
		r.log.Warnf("Skipping merge commit %s during replay", cwm.commit.Hash)
		return nil
	}

	meta, err := ParseCommitMetadata(cwm.commit.Message)
	if err != nil {
		_, err = q.ExecContext(ctx, fmt.Sprintf("CALL DOLT_CHERRY_PICK('%s');", cwm.commit.Hash))
		if err != nil && isNoChangesError(err) {
			return nil
		}
		return err
	}

	_, err = q.ExecContext(ctx, fmt.Sprintf("CALL DOLT_CHERRY_PICK('%s');", cwm.commit.Hash))
	if err != nil {
		if isNoChangesError(err) {
			return nil
		}
		return err
	}

	_, err = q.ExecContext(ctx, fmt.Sprintf(
		"CALL DOLT_COMMIT('--amend', '-m', '%s', '--author', '%s <%s>', '--date', '%s');",
		escapeSQL(cwm.commit.Message),
		escapeSQL(meta.Author),
		escapeSQL(meta.Email),
		meta.Date.Format(time.RFC3339Nano),
	))
	return err
}

func isNoChangesError(err error) bool {
	errStr := err.Error()
	return strings.Contains(errStr, "no changes") ||
		strings.Contains(errStr, "nothing to commit")
}

func (r *Reconciler) isMergeCommit(ctx context.Context, q sqlRunner, hash string) (bool, error) {
	var parentCount int
	query := "SELECT COUNT(*) FROM dolt_commit_ancestors WHERE commit_hash = ? AND parent_index IS NOT NULL"
	row, err := q.QueryContext(ctx, query, hash)
	if err != nil {
		r.log.Debugf("Could not check if %s is merge commit: %v", hash, err)
		return false, nil
	}
	defer row.Close()
	if row.Next() {
		if err := row.Scan(&parentCount); err != nil {
			return false, nil
		}
	}
	return parentCount > 1, nil
}

type commitWithMeta struct {
	commit       Commit
	hlc          HLCTimestamp
	originPeerID string
	sourceBranch string
}

func (r *Reconciler) getCommitsSince(ctx context.Context, q sqlRunner, branch string, base string) ([]Commit, error) {
	query := fmt.Sprintf("SELECT commit_hash, committer, email, date, message FROM dolt_log('%s..%s');", base, branch)
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var commits []Commit
	for rows.Next() {
		var c Commit
		if err := rows.Scan(&c.Hash, &c.Committer, &c.Email, &c.Date, &c.Message); err != nil {
			return nil, err
		}
		commits = append(commits, c)
	}
	return commits, nil
}

func sortCommitsByHLC(commits []commitWithMeta) {
	for i := range commits {
		if commits[i].hlc.IsZero() {
			meta, err := ParseCommitMetadata(commits[i].commit.Message)
			if err != nil || meta == nil {
				commits[i].hlc = HLCTimestamp{
					Wall:    commits[i].commit.Date.UnixNano(),
					Logical: 0,
					PeerID:  commits[i].commit.Committer,
				}
			} else {
				commits[i].hlc = meta.HLC
			}
		}
	}
	sort.Slice(commits, func(i, j int) bool {
		return commits[i].hlc.Less(commits[j].hlc)
	})
}

func isConflictError(err error) bool {
	errStr := err.Error()
	return strings.Contains(errStr, "conflict") ||
		strings.Contains(errStr, "CONFLICT")
}

func isNotFastForwardError(err error) bool {
	errStr := err.Error()
	return strings.Contains(errStr, "fast-forward") ||
		strings.Contains(errStr, "Fast-forward")
}
