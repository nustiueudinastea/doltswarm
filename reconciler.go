package doltswarm

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// RejectionReason indicates why a commit was dropped
type RejectionReason int

const (
	NotRejected RejectionReason = iota
	RejectedConflict
	RejectedInvalidSignature
	RejectedFetchFailed
	RejectedClockSkew
)

// errMainAdvanced indicates that main branch advanced during reorder operation
var errMainAdvanced = fmt.Errorf("main branch advanced during reorder")

type sqlRunner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// CommitRejectedCallback is called when a commit is dropped
type CommitRejectedCallback func(ad *CommitAd, reason RejectionReason)

// Reconciler handles deterministic commit ordering using HLC
type Reconciler struct {
	mu  sync.Mutex
	db  *DB
	hlc *HLC

	queue *CommitQueue
	log   *logrus.Entry

	onCommitRejected CommitRejectedCallback

	// Debounce for reorder operations
	debounceTimer   *time.Timer
	debounceMu      sync.Mutex
	debounceDelay   time.Duration
	debounceMaxWait time.Duration
	debounceStart   time.Time
	pendingReorders map[string]*CommitAd

	// Cooldown after successful reorder to prevent cascading
	reorderInProgress bool
	lastReorderTime   time.Time
	reorderCooldown   time.Duration

	ctx    context.Context
	cancel context.CancelFunc
}

// NewReconciler creates a new reconciler
func NewReconciler(db *DB, peerID string, log *logrus.Entry) *Reconciler {
	ctx, cancel := context.WithCancel(context.Background())

	r := &Reconciler{
		db:              db,
		hlc:             NewHLC(peerID),
		queue:           NewCommitQueue(),
		log:             log.WithField("component", "reconciler"),
		debounceDelay:   100 * time.Millisecond,
		debounceMaxWait: 500 * time.Millisecond,
		pendingReorders: make(map[string]*CommitAd),
		reorderCooldown: 5 * time.Second, // Wait after successful reorder before allowing new ones
		ctx:             ctx,
		cancel:          cancel,
	}

	// Start periodic sync for missing ads recovery
	go r.periodicSyncLoop()

	return r
}

// SetCommitRejectedCallback sets the rejection callback
func (r *Reconciler) SetCommitRejectedCallback(cb CommitRejectedCallback) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onCommitRejected = cb
}

// Stop stops the reconciler
func (r *Reconciler) Stop() {
	r.cancel()
	r.debounceMu.Lock()
	if r.debounceTimer != nil {
		r.debounceTimer.Stop()
	}
	r.debounceMu.Unlock()
}

// GetHLC returns the HLC
func (r *Reconciler) GetHLC() *HLC {
	return r.hlc
}

// GetQueue returns the queue
func (r *Reconciler) GetQueue() *CommitQueue {
	return r.queue
}

// OnRemoteCommit handles an incoming commit advertisement
func (r *Reconciler) OnRemoteCommit(ad *CommitAd) {
	// Update HLC from remote
	r.hlc.Update(ad.HLC)

	// Add to queue (sorted by HLC)
	if r.queue.Add(ad) {
		r.log.Debugf("Queued commit from %s (HLC: %s)", ad.PeerID, ad.HLC.String())
	}
}

// PullEarlierCommits pulls commits with HLC earlier than the given timestamp
func (r *Reconciler) PullEarlierCommits(beforeHLC HLCTimestamp) error {
	earlier := r.queue.GetPendingBefore(beforeHLC)
	if len(earlier) == 0 {
		return nil
	}

	r.log.Infof("Pulling %d earlier commits before local commit", len(earlier))

	for _, ad := range earlier {
		if err := r.pullAndApply(ad); err != nil {
			r.log.Warnf("Failed to pull commit from %s: %v", ad.PeerID, err)
			// Continue with others
		}
	}

	return nil
}

// pullAndApply fetches and fast-forwards a single commit
func (r *Reconciler) pullAndApply(ad *CommitAd) error {
	ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
	defer cancel()

	// Ensure remote exists before fetch (handles race with peer add/remove)
	if _, err := r.db.ensureRemoteExists(ctx, ad.PeerID); err != nil {
		r.queue.MarkApplied(ad) // Mark as applied to prevent retry loop
		r.queue.Remove(ad)
		if r.onCommitRejected != nil {
			r.onCommitRejected(ad, RejectedFetchFailed)
		}
		return fmt.Errorf("failed to ensure remote exists for %s: %w", ad.PeerID, err)
	}

	// Fetch from peer
	_, err := r.db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_FETCH('%s', 'main');", ad.PeerID))
	if err != nil {
		r.queue.MarkApplied(ad) // Mark as applied to prevent retry loop
		r.queue.Remove(ad)
		if r.onCommitRejected != nil {
			r.onCommitRejected(ad, RejectedFetchFailed)
		}
		return fmt.Errorf("failed to fetch from %s: %w", ad.PeerID, err)
	}

	// Fast-forward merge (should not create merge commit if we're behind)
	_, err = r.db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_MERGE('--ff-only', '%s/main');", ad.PeerID))
	if err != nil {
		// If not fast-forward, fall back to reorder path to avoid creating merge commits
		if strings.Contains(err.Error(), "fast-forward") {
			r.log.Warnf("Merge from %s was not a fast-forward, scheduling reorder", ad.PeerID)
			r.scheduleReorder(ad)
			return nil
		}
		return fmt.Errorf("failed to merge from %s: %w", ad.PeerID, err)
	}

	r.queue.Remove(ad)
	r.queue.MarkApplied(ad)
	r.log.Debugf("Applied commit from %s (HLC: %s)", ad.PeerID, ad.HLC.String())

	return nil
}

// HandleIncomingCommit processes an incoming commit that may need reordering
func (r *Reconciler) HandleIncomingCommit(ad *CommitAd) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Get our current HEAD's HLC
	myHead, err := r.db.GetLastCommit("main")
	if err != nil {
		return fmt.Errorf("failed to get HEAD: %w", err)
	}

	myHeadMeta, err := ParseCommitMetadata(myHead.Message)
	if err != nil {
		// Old-style commit without metadata, just pull and merge
		return r.pullAndApply(ad)
	}

	if ad.HLC.Less(myHeadMeta.HLC) {
		// Incoming commit is EARLIER than our HEAD
		// We need to reorder: use debounce to batch multiple reorders
		r.log.Infof("Incoming commit from %s is earlier than HEAD, scheduling reorder", ad.PeerID)
		r.scheduleReorder(ad)
		return nil
	}
	// Incoming commit is LATER than our HEAD
	// Just pull and fast-forward
	return r.pullAndApply(ad)
}

// scheduleReorder adds a commit to pending reorders and starts/resets debounce timer
func (r *Reconciler) scheduleReorder(ad *CommitAd) {
	r.debounceMu.Lock()
	defer r.debounceMu.Unlock()

	// Always add to pending - it will be processed when cooldown expires
	r.pendingReorders[ad.Key()] = ad

	now := time.Now()

	// If reorder is in progress, just queue the ad - it will be processed after current reorder
	if r.reorderInProgress {
		r.log.Debugf("Reorder in progress, queueing commit from %s", ad.PeerID)
		return
	}

	// If we're in cooldown after a successful reorder, schedule for after cooldown
	if !r.lastReorderTime.IsZero() {
		elapsed := now.Sub(r.lastReorderTime)
		if elapsed < r.reorderCooldown {
			remaining := r.reorderCooldown - elapsed
			r.log.Debugf("In cooldown, scheduling reorder in %v", remaining)
			if r.debounceTimer != nil {
				r.debounceTimer.Stop()
			}
			r.debounceTimer = time.AfterFunc(remaining, func() {
				r.executeReorder()
			})
			return
		}
	}

	if r.debounceStart.IsZero() {
		r.debounceStart = now
	}

	// Check if we've hit the max wait time
	if now.Sub(r.debounceStart) >= r.debounceMaxWait {
		// Execute immediately
		go r.executeReorder()
		return
	}

	// Reset or start debounce timer
	if r.debounceTimer != nil {
		r.debounceTimer.Stop()
	}
	r.debounceTimer = time.AfterFunc(r.debounceDelay, func() {
		r.executeReorder()
	})
}

// executeReorder performs the actual reorder after debounce
func (r *Reconciler) executeReorder() {
	r.debounceMu.Lock()
	// Check if already in progress (shouldn't happen but be safe)
	if r.reorderInProgress {
		r.debounceMu.Unlock()
		return
	}
	r.reorderInProgress = true
	pending := r.pendingReorders
	r.pendingReorders = make(map[string]*CommitAd)
	r.debounceStart = time.Time{}
	r.debounceMu.Unlock()

	// Ensure we clear in-progress flag when done
	defer func() {
		r.debounceMu.Lock()
		r.reorderInProgress = false
		r.debounceMu.Unlock()
	}()

	if len(pending) == 0 {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.log.Infof("Executing batched reorder for %d commits", len(pending))

	// Collect peer IDs deterministically.
	//
	// IMPORTANT: Using only the peers present in `pending` makes the replay set (and merge-base)
	// depend on timing / arrival order, which can lead to different peers replaying different
	// ranges and producing different commit hashes.
	//
	// Instead, include all currently known peers and iterate in a stable order.
	peerIDs := r.collectPeerIDsForReorder(pending)

	// Use a long timeout - we'll keep retrying until success or context cancellation
	// The 120s timeout is for the overall reorder operation, not per-attempt
	ctx, cancel := context.WithTimeout(r.ctx, 120*time.Second)
	defer cancel()

	// Infinite retry loop with capped exponential backoff
	// Per PLAN.md: "If FF fails because main advanced, restart replay from new merge base"
	// We never fall back to merge commits - that would violate the linear history requirement
	baseDelay := 100 * time.Millisecond
	maxDelay := 2 * time.Second
	attempt := 0

	for {
		// Check context cancellation
		select {
		case <-ctx.Done():
			r.log.Warnf("Reorder timed out after %d attempts, commits will be retried later", attempt)
			// Don't mark as applied - they'll be retried on next trigger
			return
		default:
		}

		if attempt > 0 {
			// Exponential backoff with jitter, capped at maxDelay
			// Jitter breaks synchronization between peers to prevent livelock
			shift := attempt - 1
			if shift > 10 {
				shift = 10
			}
			baseWait := baseDelay * time.Duration(1<<uint(shift))
			if baseWait > maxDelay {
				baseWait = maxDelay
			}
			// Add 0-100% jitter to break synchronization between peers
			jitter := time.Duration(rand.Int63n(int64(baseWait)))
			delay := baseWait + jitter
			r.log.Infof("Retrying reorder (attempt %d) after %v", attempt+1, delay)

			// Sleep with context cancellation check
			select {
			case <-ctx.Done():
				r.log.Warnf("Reorder cancelled during backoff")
				return
			case <-time.After(delay):
			}
		}
		attempt++

		// Fetch from all peers (re-fetch on retry to get latest)
		for _, peerID := range peerIDs {
			// Ensure remote exists before fetch
			if _, err := r.db.ensureRemoteExists(ctx, peerID); err != nil {
				r.log.Warnf("Failed to ensure remote exists for %s: %v", peerID, err)
				continue
			}
			_, err := r.db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_FETCH('%s', 'main');", peerID))
			if err != nil {
				r.log.Warnf("Failed to fetch from %s: %v", peerID, err)
			}
		}

		// Find earliest merge base across all peers
		mergeBase, err := r.findEarliestMergeBase(ctx, peerIDs)
		if err != nil {
			r.log.Errorf("Failed to find merge base: %v", err)
			return
		}

		// Snapshot current main head to detect concurrent advances
		head, err := r.db.GetLastCommit("main")
		if err != nil {
			r.log.Errorf("Failed to read main head: %v", err)
			return
		}

		// Do reorder on temp branch
		err = r.cherryPickReorderSafe(ctx, peerIDs, mergeBase, head.Hash)
		if err == errMainAdvanced {
			r.log.Warnf("Main advanced during reorder, will retry (attempt %d)", attempt)
			continue
		}
		if err != nil {
			r.log.Errorf("Failed to reorder: %v", err)
			return
		}

		// Success - mark all as applied and set cooldown
		for _, ad := range pending {
			r.queue.Remove(ad)
			r.queue.MarkApplied(ad)
		}

		// Set cooldown to prevent cascading reorders
		r.debounceMu.Lock()
		r.lastReorderTime = time.Now()
		r.debounceMu.Unlock()

		r.log.Infof("Reorder completed successfully after %d attempts, cooldown for %v", attempt, r.reorderCooldown)
		return
	}
}

func (r *Reconciler) collectPeerIDsForReorder(pending map[string]*CommitAd) []string {
	unique := make(map[string]struct{})
	for _, ad := range pending {
		if ad.PeerID != "" {
			unique[ad.PeerID] = struct{}{}
		}
	}
	for peerID := range r.db.GetPeers() {
		if peerID != "" {
			unique[peerID] = struct{}{}
		}
	}

	peerIDs := make([]string, 0, len(unique))
	for peerID := range unique {
		peerIDs = append(peerIDs, peerID)
	}
	sort.Strings(peerIDs)
	return peerIDs
}

// findEarliestMergeBase finds the earliest common ancestor across all peer branches
func (r *Reconciler) findEarliestMergeBase(ctx context.Context, peerIDs []string) (string, error) {
	// Deterministic selection: order merge bases by (metadata HLC if present else commit date),
	// with a stable tie-breaker on the merge-base hash.
	type candidate struct {
		hash    string
		orderTS HLCTimestamp
	}

	var best *candidate
	for _, peerID := range peerIDs {
		var mergeBase string
		if err := r.db.sqldb.QueryRowContext(ctx,
			fmt.Sprintf("SELECT DOLT_MERGE_BASE('%s/main', 'main');", peerID),
		).Scan(&mergeBase); err != nil {
			continue
		}

		var msg string
		var date time.Time
		if err := r.db.sqldb.QueryRowContext(ctx,
			fmt.Sprintf("SELECT message, date FROM dolt_log('%s', '-n', '1');", mergeBase),
		).Scan(&msg, &date); err != nil {
			continue
		}

		orderTS := HLCTimestamp{Wall: date.UnixNano(), Logical: 0, PeerID: mergeBase}
		if meta, err := ParseCommitMetadata(msg); err == nil && meta != nil {
			orderTS = meta.HLC
		}

		c := candidate{hash: mergeBase, orderTS: orderTS}
		if best == nil {
			best = &c
			continue
		}
		if c.orderTS.Less(best.orderTS) || (c.orderTS.Equal(best.orderTS) && c.hash < best.hash) {
			*best = c
		}
	}

	if best == nil || best.hash == "" {
		return "", fmt.Errorf("could not find merge base")
	}
	return best.hash, nil
}

// cherryPickReorderSafe reorders commits using temp branch for crash safety.
// expectedHead is the hash of main when the reorder attempt started; if main moves,
// we abort with errMainAdvanced to let the caller retry from the new base.
// Returns errMainAdvanced if main advanced during the operation (caller should retry)
func (r *Reconciler) cherryPickReorderSafe(ctx context.Context, peerIDs []string, mergeBase string, expectedHead string) error {
	// IMPORTANT: Dolt session state (checked-out branch) is connection-scoped.
	// Use a dedicated connection for the entire reorder so DOLT_CHECKOUT / cherry-pick
	// operations don't leak session state into the pool and so all statements operate
	// on the intended branch.
	conn, err := r.db.sqldb.Conn(ctx)
	if err != nil {
		return fmt.Errorf("failed to get sql connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Get commits from main since merge base
	localCommits, err := r.getCommitsSince(ctx, conn, "main", mergeBase)
	if err != nil {
		return fmt.Errorf("failed to get local commits: %w", err)
	}

	// Get commits from all peers since merge base
	var allCommits []commitWithMeta
	for _, c := range localCommits {
		allCommits = append(allCommits, commitWithMeta{commit: c, sourceBranch: "main"})
	}

	for _, peerID := range peerIDs {
		branch := fmt.Sprintf("%s/main", peerID)
		peerCommits, err := r.getCommitsSince(ctx, conn, branch, mergeBase)
		if err != nil {
			r.log.Warnf("Failed to get commits from %s: %v", peerID, err)
			continue
		}
		for _, c := range peerCommits {
			allCommits = append(allCommits, commitWithMeta{commit: c, sourceBranch: branch})
		}
	}

	// Deduplicate by stable commit identity (HLC), picking a deterministic representative.
	allCommits = deduplicateByHLC(allCommits, r.db.signer.GetID())

	// Sort by HLC
	sortCommitsByHLC(allCommits)

	if len(allCommits) == 0 {
		return nil
	}

	// Create temp branch at merge base (crash safety)
	_, err = conn.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-f', 'replay_tmp', '%s');", mergeBase))
	if err != nil {
		return fmt.Errorf("failed to create temp branch: %w", err)
	}

	// Ensure we clean up the temp branch
	defer func() {
		// Switch back to main first (using the same conn that performed checkout)
		_, _ = conn.ExecContext(context.Background(), "CALL DOLT_CHECKOUT('main');")
		_, _ = conn.ExecContext(context.Background(), "CALL DOLT_BRANCH('-D', 'replay_tmp');")
	}()

	// Checkout temp branch
	_, err = conn.ExecContext(ctx, "CALL DOLT_CHECKOUT('replay_tmp');")
	if err != nil {
		return fmt.Errorf("failed to checkout temp branch: %w", err)
	}

	// Cherry-pick in HLC order with original metadata
	for _, cwm := range allCommits {
		err = r.cherryPickWithOriginalMetadata(ctx, conn, cwm)
		if err != nil {
			if isConflictError(err) {
				r.log.Warnf("Conflict cherry-picking %s - dropping (FWW)", cwm.commit.Hash)
				_, _ = conn.ExecContext(ctx, "CALL DOLT_CHERRY_PICK('--abort');")

				// CRITICAL: Mark rejected commit as applied to prevent livelock
				meta, _ := ParseCommitMetadata(cwm.commit.Message)
				if meta != nil {
					ad := &CommitAd{
						PeerID:     meta.HLC.PeerID,
						HLC:        meta.HLC,
						CommitHash: cwm.commit.Hash,
					}
					r.queue.MarkApplied(ad)
					r.queue.Remove(ad)

					// Notify via callback
					if r.onCommitRejected != nil {
						r.onCommitRejected(ad, RejectedConflict)
					}
				}
				continue
			}
			// Non-conflict error, abort
			return fmt.Errorf("failed to cherry-pick %s: %w", cwm.commit.Hash, err)
		}
	}

	// Fast-forward main to replay_tmp (atomic)
	_, err = conn.ExecContext(ctx, "CALL DOLT_CHECKOUT('main');")
	if err != nil {
		return fmt.Errorf("failed to checkout main: %w", err)
	}

	// Ensure main didn't move while we were replaying
	curHead, err := r.db.GetLastCommit("main")
	if err != nil {
		return fmt.Errorf("failed to read main head: %w", err)
	}
	if curHead.Hash != expectedHead {
		return errMainAdvanced
	}

	// Not necessarily a fast-forward if we inserted earlier commits.
	_, err = conn.ExecContext(ctx, "CALL DOLT_RESET('--hard', 'replay_tmp');")
	if err != nil {
		return fmt.Errorf("failed to move main to replay_tmp: %w", err)
	}

	return nil
}

// cherryPickWithOriginalMetadata cherry-picks using original author/date for hash stability
func (r *Reconciler) cherryPickWithOriginalMetadata(ctx context.Context, q sqlRunner, cwm commitWithMeta) error {
	// Always skip merge commits (Dolt cannot cherry-pick them)
	isMerge, err := r.isMergeCommit(ctx, q, cwm.commit.Hash)
	if err != nil {
		return err
	}
	if isMerge {
		r.log.Warnf("Skipping merge commit %s during replay", cwm.commit.Hash)
		return nil
	}

	// Parse metadata for original author/date
	meta, err := ParseCommitMetadata(cwm.commit.Message)
	if err != nil {
		// Old commit without metadata, use regular cherry-pick
		_, err = q.ExecContext(ctx, fmt.Sprintf("CALL DOLT_CHERRY_PICK('%s');", cwm.commit.Hash))
		if err != nil && isNoChangesError(err) {
			r.log.Debugf("Skipping %s - no changes (already applied)", cwm.commit.Hash)
			return nil
		}
		return err
	}

	// Cherry-pick normally (creates a commit)
	_, err = q.ExecContext(ctx, fmt.Sprintf("CALL DOLT_CHERRY_PICK('%s');", cwm.commit.Hash))
	if err != nil {
		// If no changes, the content is already present - skip
		if isNoChangesError(err) {
			r.log.Debugf("Skipping %s - no changes (already applied)", cwm.commit.Hash)
			return nil
		}
		return err
	}

	// Amend with original metadata for hash stability
	_, err = q.ExecContext(ctx, fmt.Sprintf(
		"CALL DOLT_COMMIT('--amend', '-m', '%s', '--author', '%s <%s>', '--date', '%s');",
		escapeSQL(cwm.commit.Message),
		escapeSQL(meta.Author),
		escapeSQL(meta.Email),
		meta.Date.Format(time.RFC3339Nano),
	))
	return err
}

// isNoChangesError checks if error is due to no changes to commit
func isNoChangesError(err error) bool {
	errStr := err.Error()
	return strings.Contains(errStr, "no changes") ||
		strings.Contains(errStr, "nothing to commit")
}

// isMergeCommit returns true if a commit has more than one parent
func (r *Reconciler) isMergeCommit(ctx context.Context, q sqlRunner, hash string) (bool, error) {
	var parentCount int
	// Use dolt_commit_ancestors table to count parents for this commit
	query := "SELECT COUNT(*) FROM dolt_commit_ancestors WHERE commit_hash = ? AND parent_index IS NOT NULL"
	row, err := q.QueryContext(ctx, query, hash)
	if err != nil {
		// If the table doesn't exist or query fails, assume not a merge commit
		// to allow cherry-pick to proceed (it will fail naturally if it is a merge)
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

// getCommitsSince returns commits since a given base (excluding the base)
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

// deduplicateByHLC removes duplicate commits by HLC.
//
// NOTE: Using content_hash for dedup can be nondeterministic (it is not the commit identity) and
// selection order can vary across peers. HLC is the stable identity in the linear pipeline.
func deduplicateByHLC(commits []commitWithMeta, localPeerID string) []commitWithMeta {
	byKey := make(map[string]commitWithMeta)

	for _, cwm := range commits {
		meta, err := ParseCommitMetadata(cwm.commit.Message)
		if err != nil || meta == nil {
			// Old commit without metadata: dedup by hash (best effort).
			key := "hash:" + cwm.commit.Hash
			if _, ok := byKey[key]; !ok {
				byKey[key] = cwm
			}
			continue
		}

		cwm.hlc = meta.HLC
		cwm.originPeerID = meta.HLC.PeerID
		key := "hlc:" + meta.HLC.String()

		existing, ok := byKey[key]
		if !ok {
			byKey[key] = cwm
			continue
		}

		if preferReplaySource(cwm, existing, localPeerID) {
			byKey[key] = cwm
		}
	}

	result := make([]commitWithMeta, 0, len(byKey))
	for _, v := range byKey {
		result = append(result, v)
	}
	return result
}

func preferReplaySource(candidate commitWithMeta, current commitWithMeta, localPeerID string) bool {
	candidateCanonical := isCanonicalReplaySource(candidate, localPeerID)
	currentCanonical := isCanonicalReplaySource(current, localPeerID)
	if candidateCanonical != currentCanonical {
		return candidateCanonical
	}
	if candidate.sourceBranch != current.sourceBranch {
		return candidate.sourceBranch < current.sourceBranch
	}
	return candidate.commit.Hash < current.commit.Hash
}

func isCanonicalReplaySource(cwm commitWithMeta, localPeerID string) bool {
	// Prefer using the origin peer's branch as the source for the cherry-pick, so the delta
	// we apply is the origin's commit delta (not a replayed copy from another peer).
	if cwm.originPeerID == "" {
		return false
	}
	if cwm.originPeerID == localPeerID && cwm.sourceBranch == "main" {
		return true
	}
	return cwm.sourceBranch == fmt.Sprintf("%s/main", cwm.originPeerID)
}

// sortCommitsByHLC sorts commits by their HLC timestamp
func sortCommitsByHLC(commits []commitWithMeta) {
	// Parse HLC from each commit's metadata if not already set
	for i := range commits {
		if commits[i].hlc.Wall == 0 {
			meta, err := ParseCommitMetadata(commits[i].commit.Message)
			if err != nil {
				// Old commit without metadata, use date as fallback
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

	// Sort by HLC
	sort.Slice(commits, func(i, j int) bool {
		return commits[i].hlc.Less(commits[j].hlc)
	})
}

// isConflictError checks if error is a merge conflict
func isConflictError(err error) bool {
	errStr := err.Error()
	return strings.Contains(errStr, "conflict") ||
		strings.Contains(errStr, "CONFLICT")
}

// isNotFastForwardError checks if error is due to fast-forward not possible
func isNotFastForwardError(err error) bool {
	errStr := err.Error()
	return strings.Contains(errStr, "fast-forward") ||
		strings.Contains(errStr, "Fast-forward")
}

// periodicSyncLoop runs periodic sync to recover missing advertisements
func (r *Reconciler) periodicSyncLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.requestMissingCommits()
		}
	}
}

// requestMissingCommits requests commits from peers since our last known HLC
func (r *Reconciler) requestMissingCommits() {
	// NOTE: Using HEAD's HLC as the baseline cannot repair missed adverts whose HLC is <= HEAD.HLC,
	// which can happen under concurrent writes or transient disconnects. Instead, periodically
	// backfill a window of recent commits by HLC.
	const repairBackfillCommits = 512

	commits, err := r.db.GetAllCommits()
	if err != nil {
		return
	}

	hlcs := make([]HLCTimestamp, 0, len(commits))
	for _, c := range commits {
		meta, err := ParseCommitMetadata(c.Message)
		if err != nil || meta == nil {
			continue
		}
		hlcs = append(hlcs, meta.HLC)
	}
	if len(hlcs) == 0 {
		return
	}

	sort.Slice(hlcs, func(i, j int) bool { return hlcs[i].Less(hlcs[j]) })

	var since HLCTimestamp
	if len(hlcs) > repairBackfillCommits {
		since = hlcs[len(hlcs)-repairBackfillCommits-1]
	}

	peers := r.db.GetPeers()
	for _, peer := range peers {
		go func(p Peer) {
			ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
			defer cancel()

			// ensure remote exists before fetch
			_, _ = r.db.ensureRemoteExists(ctx, p.ID())

			resp, err := p.Sync().RequestCommitsSince(ctx, since)
			if err != nil {
				// Silence error if the peer doesn't implement this RPC yet
				if !strings.Contains(err.Error(), "Unimplemented") {
					r.log.Debugf("Failed to request commits from %s: %v", p.ID(), err)
				}
				return
			}

			for _, ad := range resp {
				r.OnRemoteCommit(ad)
			}
		}(peer)
	}
}

// escapeSQL escapes single quotes for SQL strings
func escapeSQL(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// getContentHash gets Prolly Tree root hash for the working set
func (db *DB) getContentHash() (string, error) {
	var hash string
	err := db.sqldb.QueryRow("SELECT dolt_hashof_db('WORKING')").Scan(&hash)
	return hash, err
}

// VerifyMetadataSignature verifies the metadata signature of a CommitAd
func (db *DB) VerifyMetadataSignature(ad *CommitAd) bool {
	// If this is our own commit, we can verify using our public key.
	if ad.HLC.PeerID == db.signer.GetID() {
		meta := ad.ToMetadata()
		return meta.Verify(db.signer, db.signer.PublicKey()) == nil
	}

	// TODO: implement peer public-key lookup and verification for remote commits.
	// Until key distribution is wired, accept remote commits to avoid false rejections.
	return true
}

// CreateCommitMetadata creates and signs commit metadata for a new commit
func (db *DB) CreateCommitMetadata(msg string, hlc HLCTimestamp) (*CommitMetadata, error) {
	// Get content hash
	contentHash, err := db.getContentHash()
	if err != nil {
		return nil, fmt.Errorf("failed to get content hash: %w", err)
	}

	author := db.signer.GetID()
	email := fmt.Sprintf("%s@doltswarm", author)
	commitDate := time.Now()

	metadata := NewCommitMetadata(msg, hlc, contentHash, author, email, commitDate)
	if err := metadata.Sign(db.signer); err != nil {
		return nil, fmt.Errorf("failed to sign: %w", err)
	}

	return metadata, nil
}

// doCommitWithMetadata creates a commit with HLC metadata
func doCommitWithMetadata(tx *sql.Tx, metadata *CommitMetadata, signer Signer) (string, error) {
	if signer == nil {
		return "", fmt.Errorf("no signer available")
	}

	metadataJSON, err := metadata.Marshal()
	if err != nil {
		return "", fmt.Errorf("failed to marshal metadata: %w", err)
	}

	// Commit with original metadata
	var commitHash string
	err = tx.QueryRow(fmt.Sprintf(
		"CALL DOLT_COMMIT('-A', '-m', '%s', '--author', '%s <%s>', '--date', '%s');",
		escapeSQL(metadataJSON),
		metadata.Author,
		metadata.Email,
		metadata.Date.Format(time.RFC3339Nano),
	)).Scan(&commitHash)
	if err != nil {
		return "", fmt.Errorf("failed to run commit procedure: %w", err)
	}

	// Create commit signature and add it to a tag
	signature, err := signer.Sign(commitHash)
	if err != nil {
		return "", fmt.Errorf("failed to sign commit '%s': %w", commitHash, err)
	}
	tagcmd := fmt.Sprintf("CALL DOLT_TAG('-m', '%s', '--author', '%s <%s>', '%s', '%s');",
		signer.PublicKey(), metadata.Author, metadata.Email, signature, commitHash)
	_, err = tx.Exec(tagcmd)
	if err != nil {
		return "", fmt.Errorf("failed to create signature tag (%s): %w", signature, err)
	}

	return commitHash, nil
}
