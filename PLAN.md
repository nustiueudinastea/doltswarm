# Linear History with Pull-First Ordering

## Overview

This plan describes deterministic commit ordering using Hybrid Logical Clocks (HLC) to achieve identical commit history across all peers without merge commits.

### Goals
- Deterministic commit ordering without centralized coordination
- Identical commit history on all peers (same commit hashes)
- Zero merge commits (linear history)
- Immediate commit visibility (no batching delays)
- Minimize cherry-picks through pull-first optimization

### Scope
- Target cluster size: 2-20 nodes
- Direct gRPC communication
- Hard cutover: no legacy compatibility

### Non-Goals
- GossipSub integration
- Split-brain detection/majority tracking
- Clusters > 20 nodes
- Backward compatibility with old commit format
- Preserve legacy code paths (remove them; breaking changes are acceptable)

---

## Architecture

### Current Flow (to be replaced)
```
Commit → AdvertiseHead(hash) → Pull → Merge → Merge Commits → Divergence
```

### New Flow
```
Check queue → Pull earlier commits first → Commit to main → AdvertiseCommit → Cherry-pick only if needed
```

### Cleanup Directive
Remove all legacy head-based paths (AdvertiseHead/RequestHead RPCs, head event handling), old merge helpers, unused proto messages, and associated tests/fixtures. The codebase should only contain the new linear-history pipeline; breaking removals are acceptable.

### Key Principles

1. **Commit directly to main**: No staging branches, commits go straight to main
2. **Pull-first optimization**: Before committing, pull any known earlier commits to avoid reordering
3. **HLC ordering**: When reordering is needed, use HLC for deterministic order
4. **Cherry-pick fallback**: Only cherry-pick when commits cross in transit
5. **First Write Wins**: Conflicts (as defined by Dolt) cause commit rejection
6. **Crash safety**: All reordering happens on temp branch, then fast-forward main

---

## Design Decisions

### Commit Identity
**Stable identity tuple**: (PeerID, HLC)
- PeerID: Which peer created the commit
- HLC: Hybrid Logical Clock timestamp (wall time + logical counter + peer ID)

The commit hash may change after cherry-pick (different parent), but identity is stable.

### Hash Stability
**Problem**: If we use `time.Now()` during cherry-pick replay, each peer gets different commit hashes.

**Solution**: When replaying a commit via cherry-pick:
1. Use `DOLT_CHERRY_PICK('--no-commit', hash)` to stage changes without committing
2. Ensure the parent is the previously replayed tip (HEAD must be that tip; no `--amend/--allow-empty`)
3. Use `DOLT_COMMIT` with original author, email, date, and message from metadata
4. All peers replay with identical metadata + parents → identical commit hashes

### Conflict Resolution
**Reject conflicting commits**:
- Conflicts are defined by Dolt (cherry-pick fails with conflict error)
- Conflicting commits are dropped entirely
- **Critical**: Dropped commits must be marked as "applied" (even though rejected) to prevent livelock
- Dropped commit's peer receives notification via callback
- All peers drop the same commits (deterministic due to HLC ordering)
- When a commit is already present locally, mark it applied in the queue to stay idempotent
- After a conflict abort during replay, continue replay from the current `replay_tmp` tip (do not leave HEAD detached)

### Commit Visibility
- Commits are visible immediately after being committed to main
- `AdvertiseCommit` is sent after commit is on main
- Other peers can pull immediately

### Signature Scheme
**Sign full metadata** (not just commit hash):
- Metadata: version, message, HLC, content_hash, author, email, date
- Signature = `sign(serialize(metadata_without_sig))`
- **Metadata signature is authoritative** - it proves the original author created this content
- Tag signature (on commit hash) may become invalid after cherry-pick changes the hash
- Verification should use metadata signature, not tag signature
- Incoming adverts must have metadata signature verified before enqueue; invalid adverts are dropped and marked applied with "invalid signature"

### Crash Safety
**Problem**: `DOLT_RESET --hard mergeBase` mid-replay can corrupt main if process crashes.

**Solution**: Replay on temporary branch, then fast-forward main:
1. Create `replay_tmp` branch at merge base
2. Cherry-pick all commits onto `replay_tmp`
3. Fast-forward main to `replay_tmp` (atomic). If FF fails because main advanced, restart replay from new merge base; never force-reset main.
4. Delete `replay_tmp` on both success and failure

If crash occurs during steps 1-2, main is untouched. Step 3 is atomic.

### Debounce
**Problem**: Under burst of N concurrent commits, each "earlier" advertisement triggers a full reorder, causing O(N²) work.

**Solution**: Debounce before reordering:
1. When receiving an advertisement that needs reorder, start a timer (e.g., 100ms)
2. If more advertisements arrive, reset the timer
3. Only trigger reorder after timer expires (hard cap e.g., 200–500ms so throughput stays high)
4. This batches multiple "earlier" commits into a single reorder operation

### Missing Advertisements Recovery
**Problem**: Dropped UDP packets or temporary disconnects can cause permanent gaps.

**Solution**: Periodic sync via `RequestCommitsSince` RPC:
1. Each peer periodically (e.g., every 30s) requests commits since its max applied HLC
2. Peers respond with any commits they have after that HLC, batched (e.g., 100–500 per response)
3. This fills gaps from missed advertisements without O(N²) chatter
4. Responses are verified and enqueued like normal adverts

---

## Pull-First Optimization

### The Idea
If you know about commits with earlier HLC before making your own commit, pull them first so your commit naturally comes after (no cherry-pick needed).

### Best Case: Zero Cherry-Picks
```
Timeline:
  t=0    A commits C_A (HLC=100), advertises
  t=10   B receives advertisement, adds to queue
  t=20   B wants to commit
         → B checks queue: "C_A has HLC=100, earlier than my HLC=200"
         → B pulls C_A first
         → B commits C_B with parent=C_A
         → B's main: base → C_A → C_B (natural order, no cherry-pick!)
  t=30   A pulls C_B, fast-forwards
```

### Fallback: Cherry-Pick When Commits Cross
```
Timeline:
  t=0    A commits C_A (HLC=100)
  t=0    B commits C_B (HLC=200)  ← Concurrent, didn't see C_A yet
  t=10   Both advertise
  t=20   Both receive other's advertisement
         → Both have: own commit on main, need to integrate other
         → Both cherry-pick to get: base → C_A → C_B' (HLC order)
         → C_B' has same content as C_B but different parent
         → Using original metadata, C_B' hash is identical on both peers
```

---

## Implementation Components

### 1. Hybrid Logical Clock

**File: `hlc.go`**

```go
package doltswarm

import (
    "fmt"
    "sync"
    "time"
)

// HLCTimestamp represents a hybrid logical clock timestamp
type HLCTimestamp struct {
    Wall    int64  `json:"w"` // Wall time in nanoseconds
    Logical int32  `json:"l"` // Logical counter
    PeerID  string `json:"p"` // Peer ID for deterministic tie-breaking
}

// HLC is a hybrid logical clock
type HLC struct {
    mu        sync.Mutex
    timestamp HLCTimestamp
    peerID    string
}

// NewHLC creates a new HLC for the given peer
func NewHLC(peerID string) *HLC {
    return &HLC{
        peerID: peerID,
        timestamp: HLCTimestamp{
            Wall:    time.Now().UnixNano(),
            Logical: 0,
            PeerID:  peerID,
        },
    }
}

// Now returns a new timestamp for a local event
func (c *HLC) Now() HLCTimestamp {
    c.mu.Lock()
    defer c.mu.Unlock()

    physicalTime := time.Now().UnixNano()

    if c.timestamp.Wall >= physicalTime {
        c.timestamp.Logical++
    } else {
        c.timestamp.Wall = physicalTime
        c.timestamp.Logical = 0
    }

    return HLCTimestamp{
        Wall:    c.timestamp.Wall,
        Logical: c.timestamp.Logical,
        PeerID:  c.peerID,
    }
}

// Update updates the clock based on a received timestamp
func (c *HLC) Update(remote HLCTimestamp) {
    c.mu.Lock()
    defer c.mu.Unlock()

    physicalTime := time.Now().UnixNano()
    maxWall := max(c.timestamp.Wall, max(remote.Wall, physicalTime))

    if maxWall == c.timestamp.Wall && maxWall == remote.Wall {
        c.timestamp.Logical = max(c.timestamp.Logical, remote.Logical) + 1
    } else if maxWall == c.timestamp.Wall {
        c.timestamp.Logical++
    } else if maxWall == remote.Wall {
        c.timestamp.Wall = remote.Wall
        c.timestamp.Logical = remote.Logical + 1
    } else {
        c.timestamp.Wall = physicalTime
        c.timestamp.Logical = 0
    }
}

// Less returns true if h < other (total ordering)
func (h HLCTimestamp) Less(other HLCTimestamp) bool {
    if h.Wall != other.Wall {
        return h.Wall < other.Wall
    }
    if h.Logical != other.Logical {
        return h.Logical < other.Logical
    }
    return h.PeerID < other.PeerID
}

// Equal returns true if timestamps are equal
func (h HLCTimestamp) Equal(other HLCTimestamp) bool {
    return h.Wall == other.Wall && h.Logical == other.Logical && h.PeerID == other.PeerID
}

// String returns a unique string key for this timestamp
func (h HLCTimestamp) String() string {
    return fmt.Sprintf("%d:%d:%s", h.Wall, h.Logical, h.PeerID)
}
```
**Skew guard**: On receive, if `HLC.Wall` is more than a configurable `MaxClockSkew` (e.g., 5s) ahead of local `time.Now()`, reject the advert and mark it applied as "hlc_invalid" to avoid skew-induced reorders.

---

### 2. Commit Metadata

**File: `metadata.go`**

```go
package doltswarm

import (
    "encoding/json"
    "fmt"
    "time"
)

const CommitMetadataVersion = 1

// CommitMetadata is stored in commit messages as JSON
type CommitMetadata struct {
    Version     int          `json:"v"`
    Message     string       `json:"msg"`
    HLC         HLCTimestamp `json:"hlc"`
    ContentHash string       `json:"content_hash"`
    Author      string       `json:"author"`
    Email       string       `json:"email"`
    Date        time.Time    `json:"date"`
    Signature   string       `json:"sig"`
}

// CommitMetadataForSigning excludes the Signature field
type CommitMetadataForSigning struct {
    Version     int          `json:"v"`
    Message     string       `json:"msg"`
    HLC         HLCTimestamp `json:"hlc"`
    ContentHash string       `json:"content_hash"`
    Author      string       `json:"author"`
    Email       string       `json:"email"`
    Date        time.Time    `json:"date"`
}

// NewCommitMetadata creates new metadata
func NewCommitMetadata(msg string, hlc HLCTimestamp, contentHash string, author string, email string, date time.Time) *CommitMetadata {
    return &CommitMetadata{
        Version:     CommitMetadataVersion,
        Message:     msg,
        HLC:         hlc,
        ContentHash: contentHash,
        Author:      author,
        Email:       email,
        Date:        date,
    }
}

// SignableData returns bytes to sign
func (m *CommitMetadata) SignableData() ([]byte, error) {
    forSigning := CommitMetadataForSigning{
        Version:     m.Version,
        Message:     m.Message,
        HLC:         m.HLC,
        ContentHash: m.ContentHash,
        Author:      m.Author,
        Email:       m.Email,
        Date:        m.Date,
    }
    return json.Marshal(forSigning)
}

// Sign signs the metadata
func (m *CommitMetadata) Sign(signer Signer) error {
    data, err := m.SignableData()
    if err != nil {
        return fmt.Errorf("failed to get signable data: %w", err)
    }

    signature, err := signer.Sign(string(data))
    if err != nil {
        return fmt.Errorf("failed to sign: %w", err)
    }

    m.Signature = signature
    return nil
}

// Marshal serializes to JSON string
func (m *CommitMetadata) Marshal() (string, error) {
    data, err := json.Marshal(m)
    if err != nil {
        return "", err
    }
    return string(data), nil
}

// ParseCommitMetadata parses JSON commit message
func ParseCommitMetadata(commitMessage string) (*CommitMetadata, error) {
    var m CommitMetadata
    if err := json.Unmarshal([]byte(commitMessage), &m); err != nil {
        return nil, err
    }
    if m.Version == 0 {
        return nil, fmt.Errorf("invalid metadata: missing version")
    }
    return &m, nil
}
```

---

### 3. Commit Advertisement

**File: `commit_ad.go`**

```go
package doltswarm

import "time"

// CommitAd represents an advertised commit
type CommitAd struct {
    // Stable identity (never changes)
    PeerID      string
    HLC         HLCTimestamp
    ContentHash string

    // For fetching
    CommitHash string // Hash on originating peer's main branch

    // Original commit metadata (for replay with identical hash)
    Message string
    Author  string
    Email   string
    Date    time.Time

    // Signature over metadata (authoritative)
    Signature string
}

// Key returns unique identifier for this commit
func (ad *CommitAd) Key() string {
    return ad.HLC.String()
}
```

---

### 4. Commit Queue

**File: `queue.go`**

```go
package doltswarm

import (
    "sort"
    "sync"
)

// CommitQueue maintains pending commits ordered by HLC
type CommitQueue struct {
    mu      sync.RWMutex
    pending []*CommitAd
    applied map[string]bool // Key: HLC.String()
}

// NewCommitQueue creates a new queue
func NewCommitQueue() *CommitQueue {
    return &CommitQueue{
        pending: make([]*CommitAd, 0),
        applied: make(map[string]bool),
    }
}

// Add inserts a commit in HLC order, returns true if added
func (q *CommitQueue) Add(ad *CommitAd) bool {
    q.mu.Lock()
    defer q.mu.Unlock()

    key := ad.Key()

    // Skip if already applied or pending
    if q.applied[key] {
        return false
    }
    for _, p := range q.pending {
        if p.Key() == key {
            return false
        }
    }

    // Insert in sorted position (by HLC)
    idx := sort.Search(len(q.pending), func(i int) bool {
        return ad.HLC.Less(q.pending[i].HLC)
    })

    q.pending = append(q.pending, nil)
    copy(q.pending[idx+1:], q.pending[idx:])
    q.pending[idx] = ad

    return true
}

// GetPendingBefore returns commits with HLC less than the given timestamp
func (q *CommitQueue) GetPendingBefore(hlc HLCTimestamp) []*CommitAd {
    q.mu.RLock()
    defer q.mu.RUnlock()

    var result []*CommitAd
    for _, ad := range q.pending {
        if ad.HLC.Less(hlc) {
            result = append(result, ad)
        }
    }
    return result
}

// GetPendingAfter returns commits with HLC greater than the given timestamp
func (q *CommitQueue) GetPendingAfter(hlc HLCTimestamp) []*CommitAd {
    q.mu.RLock()
    defer q.mu.RUnlock()

    var result []*CommitAd
    for _, ad := range q.pending {
        if hlc.Less(ad.HLC) {
            result = append(result, ad)
        }
    }
    return result
}

// TakePending returns and clears all pending commits (already sorted)
func (q *CommitQueue) TakePending() []*CommitAd {
    q.mu.Lock()
    defer q.mu.Unlock()

    result := q.pending
    q.pending = make([]*CommitAd, 0)
    return result
}

// Remove removes a specific commit from pending
func (q *CommitQueue) Remove(ad *CommitAd) {
    q.mu.Lock()
    defer q.mu.Unlock()

    key := ad.Key()
    for i, p := range q.pending {
        if p.Key() == key {
            q.pending = append(q.pending[:i], q.pending[i+1:]...)
            return
        }
    }
}

// MarkApplied marks a commit as applied (including rejected commits!)
func (q *CommitQueue) MarkApplied(ad *CommitAd) {
    q.mu.Lock()
    defer q.mu.Unlock()
    q.applied[ad.Key()] = true
}

// IsApplied checks if a commit has been applied
func (q *CommitQueue) IsApplied(hlcKey string) bool {
    q.mu.RLock()
    defer q.mu.RUnlock()
    return q.applied[hlcKey]
}

// HasPending returns true if there are pending commits
func (q *CommitQueue) HasPending() bool {
    q.mu.RLock()
    defer q.mu.RUnlock()
    return len(q.pending) > 0
}

// PendingCount returns count of pending commits
func (q *CommitQueue) PendingCount() int {
    q.mu.RLock()
    defer q.mu.RUnlock()
    return len(q.pending)
}

// GetCommitsSince returns all applied commits with HLC greater than the given timestamp
// Used for RequestCommitsSince recovery
func (q *CommitQueue) GetAppliedKeys() []string {
    q.mu.RLock()
    defer q.mu.RUnlock()

    keys := make([]string, 0, len(q.applied))
    for k := range q.applied {
        keys = append(keys, k)
    }
    return keys
}
```

---

### 5. Reconciler

**File: `reconciler.go`**

```go
package doltswarm

import (
    "context"
    "fmt"
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
)

// CommitRejectedCallback is called when a commit is dropped
type CommitRejectedCallback func(ad *CommitAd, reason RejectionReason)

// Reconciler handles deterministic commit ordering
type Reconciler struct {
    mu     sync.Mutex
    db     *DB
    hlc    *HLC
    queue  *CommitQueue
    log    *logrus.Entry

    onCommitRejected CommitRejectedCallback

    // Debounce for reorder operations
    debounceTimer   *time.Timer
    debounceMu      sync.Mutex
    debounceDelay   time.Duration
    pendingReorders map[string]*CommitAd // Commits waiting for debounce

    ctx    context.Context
    cancel context.CancelFunc
}

// NewReconciler creates a new reconciler
func NewReconciler(db *DB, log *logrus.Entry) *Reconciler {
    ctx, cancel := context.WithCancel(context.Background())

    r := &Reconciler{
        db:              db,
        hlc:             NewHLC(db.signer.GetID()),
        queue:           NewCommitQueue(),
        log:             log.WithField("component", "reconciler"),
        debounceDelay:   100 * time.Millisecond,
        pendingReorders: make(map[string]*CommitAd),
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
// Returns the HLC of the latest commit pulled (for chaining)
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
    _, err = r.db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_MERGE('%s/main');", ad.PeerID))
    if err != nil {
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
    } else {
        // Incoming commit is LATER than our HEAD
        // Just pull and fast-forward
        return r.pullAndApply(ad)
    }
}

// scheduleReorder adds a commit to pending reorders and starts/resets debounce timer
func (r *Reconciler) scheduleReorder(ad *CommitAd) {
    r.debounceMu.Lock()
    defer r.debounceMu.Unlock()

    r.pendingReorders[ad.Key()] = ad

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
    pending := r.pendingReorders
    r.pendingReorders = make(map[string]*CommitAd)
    r.debounceMu.Unlock()

    if len(pending) == 0 {
        return
    }

    r.mu.Lock()
    defer r.mu.Unlock()

    r.log.Infof("Executing batched reorder for %d commits", len(pending))

    // Collect all unique peer IDs we need to fetch from
    peerIDs := make(map[string]bool)
    for _, ad := range pending {
        peerIDs[ad.PeerID] = true
    }

    ctx, cancel := context.WithTimeout(r.ctx, 120*time.Second)
    defer cancel()

    // Fetch from all peers
    for peerID := range peerIDs {
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

    // Do reorder on temp branch
    err = r.cherryPickReorderSafe(ctx, peerIDs, mergeBase)
    if err != nil {
        r.log.Errorf("Failed to reorder: %v", err)
        return
    }

    // Mark all as applied
    for _, ad := range pending {
        r.queue.Remove(ad)
        r.queue.MarkApplied(ad)
    }
}

// findEarliestMergeBase finds the earliest common ancestor across all peer branches
func (r *Reconciler) findEarliestMergeBase(ctx context.Context, peerIDs map[string]bool) (string, error) {
    var earliestBase string
    var earliestDate time.Time

    for peerID := range peerIDs {
        var mergeBase string
        err := r.db.sqldb.QueryRowContext(ctx,
            fmt.Sprintf("SELECT DOLT_MERGE_BASE('%s/main', 'main');", peerID)).Scan(&mergeBase)
        if err != nil {
            continue
        }

        // Get date of merge base
        var date time.Time
        err = r.db.sqldb.QueryRowContext(ctx,
            fmt.Sprintf("SELECT date FROM dolt_log('%s', '-n', '1');", mergeBase)).Scan(&date)
        if err != nil {
            continue
        }

        if earliestBase == "" || date.Before(earliestDate) {
            earliestBase = mergeBase
            earliestDate = date
        }
    }

    if earliestBase == "" {
        return "", fmt.Errorf("could not find merge base")
    }
    return earliestBase, nil
}

// cherryPickReorderSafe reorders commits using temp branch for crash safety
func (r *Reconciler) cherryPickReorderSafe(ctx context.Context, peerIDs map[string]bool, mergeBase string) error {
    // Get commits from main since merge base
    localCommits, err := r.getCommitsSince(ctx, "main", mergeBase)
    if err != nil {
        return fmt.Errorf("failed to get local commits: %w", err)
    }

    // Get commits from all peers since merge base
    var allCommits []commitWithMeta
    for _, c := range localCommits {
        allCommits = append(allCommits, commitWithMeta{commit: c})
    }

    for peerID := range peerIDs {
        peerCommits, err := r.getCommitsSince(ctx, fmt.Sprintf("%s/main", peerID), mergeBase)
        if err != nil {
            r.log.Warnf("Failed to get commits from %s: %v", peerID, err)
            continue
        }
        for _, c := range peerCommits {
            allCommits = append(allCommits, commitWithMeta{commit: c})
        }
    }

    // Deduplicate by content hash
    allCommits = deduplicateByContent(allCommits)

    // Sort by HLC
    sortCommitsByHLC(allCommits)

    if len(allCommits) == 0 {
        return nil
    }

    // Create temp branch at merge base (crash safety)
    _, err = r.db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-f', 'replay_tmp', '%s');", mergeBase))
    if err != nil {
        return fmt.Errorf("failed to create temp branch: %w", err)
    }

    // Checkout temp branch
    _, err = r.db.ExecContext(ctx, "CALL DOLT_CHECKOUT('replay_tmp');")
    if err != nil {
        return fmt.Errorf("failed to checkout temp branch: %w", err)
    }

    // Cherry-pick in HLC order with original metadata
    for _, cwm := range allCommits {
        err = r.cherryPickWithOriginalMetadata(ctx, cwm)
        if err != nil {
            if isConflictError(err) {
                r.log.Warnf("Conflict cherry-picking %s - dropping (FWW)", cwm.commit.Hash)
                _, _ = r.db.ExecContext(ctx, "CALL DOLT_CHERRY_PICK('--abort');")

                // CRITICAL: Mark rejected commit as applied to prevent livelock
                meta, _ := ParseCommitMetadata(cwm.commit.Message)
                if meta != nil {
                    ad := &CommitAd{
                        PeerID:     cwm.commit.Committer,
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
            _, _ = r.db.ExecContext(ctx, "CALL DOLT_CHECKOUT('main');")
            _, _ = r.db.ExecContext(ctx, "CALL DOLT_BRANCH('-D', 'replay_tmp');")
            return fmt.Errorf("failed to cherry-pick %s: %w", cwm.commit.Hash, err)
        }
    }

    // Fast-forward main to replay_tmp (atomic)
    _, err = r.db.ExecContext(ctx, "CALL DOLT_CHECKOUT('main');")
    if err != nil {
        return fmt.Errorf("failed to checkout main: %w", err)
    }

    _, err = r.db.ExecContext(ctx, "CALL DOLT_MERGE('--ff-only', 'replay_tmp');")
    if err != nil {
        return fmt.Errorf("failed to fast-forward main: %w", err)
    }

    // Clean up temp branch
    _, err = r.db.ExecContext(ctx, "CALL DOLT_BRANCH('-D', 'replay_tmp');")
    if err != nil {
        r.log.Warnf("Failed to delete temp branch: %v", err)
    }

    return nil
}

// cherryPickWithOriginalMetadata cherry-picks using original author/date for hash stability
func (r *Reconciler) cherryPickWithOriginalMetadata(ctx context.Context, cwm commitWithMeta) error {
    // Parse metadata for original author/date
    meta, err := ParseCommitMetadata(cwm.commit.Message)
    if err != nil {
        // Old commit without metadata, use regular cherry-pick
        _, err = r.db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_CHERRY_PICK('%s');", cwm.commit.Hash))
        return err
    }

    // Use --no-commit to stage changes without committing
    _, err = r.db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_CHERRY_PICK('--no-commit', '%s');", cwm.commit.Hash))
    if err != nil {
        return err
    }

    // Commit with original metadata for hash stability
    _, err = r.db.ExecContext(ctx, fmt.Sprintf(
        "CALL DOLT_COMMIT('-A', '-m', '%s', '--author', '%s <%s>', '--date', '%s');",
        escapeSQL(cwm.commit.Message),
        meta.Author,
        meta.Email,
        meta.Date.Format(time.RFC3339Nano),
    ))
    return err
}

type commitWithMeta struct {
    commit Commit
    hlc    HLCTimestamp
}

// getCommitsSince returns commits since a given base (excluding the base)
func (r *Reconciler) getCommitsSince(ctx context.Context, branch string, base string) ([]Commit, error) {
    query := fmt.Sprintf("SELECT commit_hash, committer, email, date, message FROM dolt_log('%s..%s');", base, branch)
    rows, err := r.db.QueryContext(ctx, query)
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

// deduplicateByContent removes duplicate commits by content hash
func deduplicateByContent(commits []commitWithMeta) []commitWithMeta {
    seen := make(map[string]bool)
    var result []commitWithMeta

    for _, cwm := range commits {
        meta, err := ParseCommitMetadata(cwm.commit.Message)
        if err != nil {
            // No metadata, use commit hash as key
            if !seen[cwm.commit.Hash] {
                seen[cwm.commit.Hash] = true
                result = append(result, cwm)
            }
            continue
        }

        // Use content hash as dedup key
        if !seen[meta.ContentHash] {
            seen[meta.ContentHash] = true
            cwm.hlc = meta.HLC
            result = append(result, cwm)
        }
    }
    return result
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
    // Get our HEAD's HLC as baseline
    myHead, err := r.db.GetLastCommit("main")
    if err != nil {
        return
    }

    meta, err := ParseCommitMetadata(myHead.Message)
    if err != nil {
        return // Old-style commits, skip
    }

    clients := r.db.GetClients()
    for _, client := range clients {
        go func(c *DBClient) {
            ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
            defer cancel()

            resp, err := c.RequestCommitsSince(ctx, &proto.RequestCommitsSinceRequest{
                SinceHlc: &proto.HLCTimestamp{
                    Wall:    meta.HLC.Wall,
                    Logical: meta.HLC.Logical,
                    PeerId:  meta.HLC.PeerID,
                },
            })
            if err != nil {
                return
            }

            for _, commit := range resp.Commits {
                ad := &CommitAd{
                    PeerID: commit.PeerId,
                    HLC: HLCTimestamp{
                        Wall:    commit.Hlc.Wall,
                        Logical: commit.Hlc.Logical,
                        PeerID:  commit.Hlc.PeerId,
                    },
                    ContentHash: commit.ContentHash,
                    CommitHash:  commit.CommitHash,
                    Message:     commit.Message,
                    Author:      commit.Author,
                    Email:       commit.Email,
                    Signature:   commit.Signature,
                }
                if commit.Date != nil {
                    ad.Date = commit.Date.AsTime()
                }
                r.OnRemoteCommit(ad)
            }
        }(client)
    }
}
```

---

### 6. Protocol Buffer Updates

**File: `proto/dbsyncer.proto`**

```protobuf
syntax = "proto3";

package proto;

option go_package = "github.com/nustiueudinastea/doltswarm/proto";

import "google/protobuf/timestamp.proto";

service DBSyncer {
    // Legacy - keep for transition
    rpc AdvertiseHead(AdvertiseHeadRequest) returns (AdvertiseHeadResponse);
    rpc RequestHead(RequestHeadRequest) returns (RequestHeadResponse);

    // New: Commit advertisement with HLC
    rpc AdvertiseCommit(AdvertiseCommitRequest) returns (AdvertiseCommitResponse);

    // New: Request commits since a given HLC (for recovery)
    rpc RequestCommitsSince(RequestCommitsSinceRequest) returns (RequestCommitsSinceResponse);
}

// Legacy messages
message AdvertiseHeadRequest {
    string head = 1;
}

message AdvertiseHeadResponse {}

message RequestHeadRequest {}

message RequestHeadResponse {
    string head = 1;
}

// HLC timestamp
message HLCTimestamp {
    int64 wall = 1;
    int32 logical = 2;
    string peer_id = 3;
}

// Commit advertisement with HLC identity
message AdvertiseCommitRequest {
    string peer_id = 1;
    HLCTimestamp hlc = 2;
    string content_hash = 3;
    string commit_hash = 4;  // Hash on sender's main
    string message = 5;
    string author = 6;
    string email = 7;
    google.protobuf.Timestamp date = 8;
    string signature = 9;    // Signature over metadata (authoritative)
}

message AdvertiseCommitResponse {
    bool accepted = 1;
}

// Request commits since a given HLC (for missing ads recovery)
message RequestCommitsSinceRequest {
    HLCTimestamp since_hlc = 1;
}

message RequestCommitsSinceResponse {
    repeated AdvertiseCommitRequest commits = 1;
}
```

---

### 7. Modified Commit Flow

**File: `sql.go`** (modified)

```go
// getContentHash gets Prolly Tree root hash
func (db *DB) getContentHash() (string, error) {
    var hash string
    err := db.sqldb.QueryRow("SELECT dolt_hashof_db('WORKING')").Scan(&hash)
    return hash, err
}

// Commit creates a commit with HLC ordering
func (db *DB) Commit(commitMsg string) (string, error) {
    // Lock to prevent concurrent commits
    db.reconciler.mu.Lock()
    defer db.reconciler.mu.Unlock()

    // Get HLC timestamp for our commit
    hlc := db.reconciler.GetHLC().Now()

    // Pull-first: apply any known earlier commits first
    err := db.reconciler.PullEarlierCommits(hlc)
    if err != nil {
        db.log.Warnf("Failed to pull earlier commits: %v", err)
        // Continue anyway
    }

    // Get content hash
    contentHash, err := db.getContentHash()
    if err != nil {
        return "", fmt.Errorf("failed to get content hash: %w", err)
    }

    // Capture original metadata at commit time
    author := db.signer.GetID()
    email := fmt.Sprintf("%s@doltswarm", author)
    commitDate := time.Now()

    // Create metadata with original author/date
    metadata := NewCommitMetadata(commitMsg, hlc, contentHash, author, email, commitDate)
    if err := metadata.Sign(db.signer); err != nil {
        return "", fmt.Errorf("failed to sign: %w", err)
    }

    metadataJSON, err := metadata.Marshal()
    if err != nil {
        return "", fmt.Errorf("failed to marshal metadata: %w", err)
    }

    // Commit to main with metadata as message
    tx, err := db.BeginTx(db.ctx, nil)
    if err != nil {
        return "", fmt.Errorf("failed to begin tx: %w", err)
    }
    defer tx.Rollback()

    var commitHash string
    err = tx.QueryRow(fmt.Sprintf(
        "CALL DOLT_COMMIT('-A', '-m', '%s', '--author', '%s <%s>', '--date', '%s');",
        escapeSQL(metadataJSON),
        author,
        email,
        commitDate.Format(time.RFC3339Nano),
    )).Scan(&commitHash)
    if err != nil {
        return "", fmt.Errorf("failed to commit: %w", err)
    }

    // Create signature tag (note: this may become invalid after cherry-pick)
    signature, err := db.signer.Sign(commitHash)
    if err != nil {
        return "", fmt.Errorf("failed to sign commit: %w", err)
    }
    _, err = tx.Exec(fmt.Sprintf(
        "CALL DOLT_TAG('-m', '%s', '--author', '%s <%s>', '%s', '%s');",
        db.signer.PublicKey(), author, email, signature, commitHash,
    ))
    if err != nil {
        return "", fmt.Errorf("failed to create tag: %w", err)
    }

    if err := tx.Commit(); err != nil {
        return "", fmt.Errorf("failed to commit tx: %w", err)
    }

    // Mark our own commit as applied
    db.reconciler.GetQueue().MarkApplied(&CommitAd{HLC: hlc})

    // Advertise to peers with full metadata
    db.AdvertiseCommit(&CommitAd{
        PeerID:      author,
        HLC:         hlc,
        ContentHash: contentHash,
        CommitHash:  commitHash,
        Message:     commitMsg,
        Author:      author,
        Email:       email,
        Date:        commitDate,
        Signature:   metadata.Signature,
    })

    return commitHash, nil
}

// ExecAndCommit executes SQL and commits with HLC ordering
func (db *DB) ExecAndCommit(execFunc ExecFunc, commitMsg string) (string, error) {
    // Lock to prevent concurrent commits
    db.reconciler.mu.Lock()
    defer db.reconciler.mu.Unlock()

    // Get HLC timestamp
    hlc := db.reconciler.GetHLC().Now()

    // Pull-first: apply any known earlier commits first
    err := db.reconciler.PullEarlierCommits(hlc)
    if err != nil {
        db.log.Warnf("Failed to pull earlier commits: %v", err)
    }

    tx, err := db.BeginTx(db.ctx, nil)
    if err != nil {
        return "", fmt.Errorf("failed to begin tx: %w", err)
    }
    defer tx.Rollback()

    _, err = tx.Exec("CALL DOLT_CHECKOUT('main');")
    if err != nil {
        return "", fmt.Errorf("failed to checkout main: %w", err)
    }

    // Execute user function
    if err := execFunc(tx); err != nil {
        return "", fmt.Errorf("failed to exec function: %w", err)
    }

    // Get content hash
    var contentHash string
    if err := tx.QueryRow("SELECT dolt_hashof_db('WORKING')").Scan(&contentHash); err != nil {
        return "", fmt.Errorf("failed to get content hash: %w", err)
    }

    // Capture original metadata at commit time
    author := db.signer.GetID()
    email := fmt.Sprintf("%s@doltswarm", author)
    commitDate := time.Now()

    // Create metadata with original author/date
    metadata := NewCommitMetadata(commitMsg, hlc, contentHash, author, email, commitDate)
    if err := metadata.Sign(db.signer); err != nil {
        return "", fmt.Errorf("failed to sign: %w", err)
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
        author,
        email,
        commitDate.Format(time.RFC3339Nano),
    )).Scan(&commitHash)
    if err != nil {
        return "", fmt.Errorf("failed to commit: %w", err)
    }

    // Create signature tag
    signature, err := db.signer.Sign(commitHash)
    if err != nil {
        return "", fmt.Errorf("failed to sign commit: %w", err)
    }
    _, err = tx.Exec(fmt.Sprintf(
        "CALL DOLT_TAG('-m', '%s', '--author', '%s <%s>', '%s', '%s');",
        db.signer.PublicKey(), author, email, signature, commitHash,
    ))
    if err != nil {
        return "", fmt.Errorf("failed to create tag: %w", err)
    }

    // Get changed tables
    changedTables, err := getChangedTables(tx, "WORKING", "HEAD")
    if err != nil {
        return "", err
    }

    if err := tx.Commit(); err != nil {
        return "", fmt.Errorf("failed to commit tx: %w", err)
    }

    // Trigger callbacks
    for _, table := range changedTables {
        db.triggerTableChangeCallbacks(table)
    }

    // Mark applied
    db.reconciler.GetQueue().MarkApplied(&CommitAd{HLC: hlc})

    // Advertise with full metadata
    db.AdvertiseCommit(&CommitAd{
        PeerID:      author,
        HLC:         hlc,
        ContentHash: contentHash,
        CommitHash:  commitHash,
        Message:     commitMsg,
        Author:      author,
        Email:       email,
        Date:        commitDate,
        Signature:   metadata.Signature,
    })

    return commitHash, nil
}

func escapeSQL(s string) string {
    return strings.ReplaceAll(s, "'", "''")
}
```

---

### 8. Syncer Updates

**File: `syncer.go`** (additions)

```go
// AdvertiseCommit handles incoming commit advertisements
func (s *ServerSyncer) AdvertiseCommit(ctx context.Context, req *proto.AdvertiseCommitRequest) (*proto.AdvertiseCommitResponse, error) {
    peer, ok := p2pgrpc.RemotePeerFromContext(ctx)
    if !ok {
        return nil, errors.New("no peer in context")
    }

    ad := &CommitAd{
        PeerID: req.PeerId,
        HLC: HLCTimestamp{
            Wall:    req.Hlc.Wall,
            Logical: req.Hlc.Logical,
            PeerID:  req.Hlc.PeerId,
        },
        ContentHash: req.ContentHash,
        CommitHash:  req.CommitHash,
        Message:     req.Message,
        Author:      req.Author,
        Email:       req.Email,
        Signature:   req.Signature,
    }
    if req.Date != nil {
        ad.Date = req.Date.AsTime()
    }

    // Reject if HLC is too far in the future (clock skew guard)
    if isFutureSkew(ad.HLC) {
        return &proto.AdvertiseCommitResponse{Accepted: false}, nil
    }

    // Verify metadata signature before enqueue
    if ok := verifyMetadataSignature(ad); !ok {
        s.db.reconciler.GetQueue().MarkApplied(ad) // avoid retries
        return &proto.AdvertiseCommitResponse{Accepted: false}, nil
    }

    // Add to reconciler queue
    s.db.reconciler.OnRemoteCommit(ad)

    // Handle the commit (pull or reorder as needed)
    go func() {
        err := s.db.reconciler.HandleIncomingCommit(ad)
        if err != nil {
            s.log.Warnf("Failed to handle incoming commit from %s: %v", peer.String(), err)
        }
    }()

    return &proto.AdvertiseCommitResponse{Accepted: true}, nil
}

// Helpers: isFutureSkew enforces MaxClockSkew; verifyMetadataSignature checks the metadata signature with the sender's public key.

// RequestCommitsSince returns commits since a given HLC (for recovery)
func (s *ServerSyncer) RequestCommitsSince(ctx context.Context, req *proto.RequestCommitsSinceRequest) (*proto.RequestCommitsSinceResponse, error) {
    sinceHLC := HLCTimestamp{
        Wall:    req.SinceHlc.Wall,
        Logical: req.SinceHlc.Logical,
        PeerID:  req.SinceHlc.PeerId,
    }

    // Get commits from main branch that are after the requested HLC
    commits, err := s.db.GetAllCommits()
    if err != nil {
        return nil, fmt.Errorf("failed to get commits: %w", err)
    }

    var result []*proto.AdvertiseCommitRequest
    for _, c := range commits {
        meta, err := ParseCommitMetadata(c.Message)
        if err != nil {
            continue // Skip old-style commits
        }

        if sinceHLC.Less(meta.HLC) {
            result = append(result, &proto.AdvertiseCommitRequest{
                PeerId: meta.HLC.PeerID,
                Hlc: &proto.HLCTimestamp{
                    Wall:    meta.HLC.Wall,
                    Logical: meta.HLC.Logical,
                    PeerId:  meta.HLC.PeerID,
                },
                ContentHash: meta.ContentHash,
                CommitHash:  c.Hash,
                Message:     meta.Message,
                Author:      meta.Author,
                Email:       meta.Email,
                Date:        timestamppb.New(meta.Date),
                Signature:   meta.Signature,
            })
        }
    }

    return &proto.RequestCommitsSinceResponse{Commits: result}, nil
}
```

---

### 9. Distributed Updates

**File: `distributed.go`** (additions)

```go
// AdvertiseCommit broadcasts commit to all peers
func (db *DB) AdvertiseCommit(ad *CommitAd) {
    go func() {
        clients := db.GetClients()
        if len(clients) == 0 {
            return
        }

        db.log.Debugf("Advertising commit %s to %d peers", ad.CommitHash, len(clients))

        req := &proto.AdvertiseCommitRequest{
            PeerId: ad.PeerID,
            Hlc: &proto.HLCTimestamp{
                Wall:    ad.HLC.Wall,
                Logical: ad.HLC.Logical,
                PeerId:  ad.HLC.PeerID,
            },
            ContentHash: ad.ContentHash,
            CommitHash:  ad.CommitHash,
            Message:     ad.Message,
            Author:      ad.Author,
            Email:       ad.Email,
            Date:        timestamppb.New(ad.Date),
            Signature:   ad.Signature,
        }

        var wg sync.WaitGroup
        for _, client := range clients {
            wg.Add(1)
            go func(c *DBClient) {
                defer wg.Done()
                ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
                defer cancel()

                _, err := c.AdvertiseCommit(ctx, req, grpc.WaitForReady(true))
                if err != nil {
                    db.log.Warnf("Failed to advertise commit to %s: %v", c.GetID(), err)
                }
            }(client)
        }
        wg.Wait()
    }()
}
```

---

### 10. DB Updates

**File: `db.go`** (additions)

```go
type DB struct {
    // ... existing fields ...

    reconciler *Reconciler
}

func Open(dir string, name string, logger *logrus.Entry, signer Signer) (*DB, error) {
    // ... existing code ...

    // Initialize reconciler
    db.reconciler = NewReconciler(db, logger)

    return db, nil
}

func (db *DB) Close() error {
    if db.reconciler != nil {
        db.reconciler.Stop()
    }
    // ... existing close code ...
}

func (db *DB) GetReconciler() *Reconciler {
    return db.reconciler
}

func (db *DB) SetCommitRejectedCallback(cb CommitRejectedCallback) {
    db.reconciler.SetCommitRejectedCallback(cb)
}
```

---

## Data Flow Summary

```
1. LOCAL COMMIT (with pull-first optimization)
   ┌─────────────────────────────────────────────────┐
   │ User calls db.Commit("message")                 │
   │ → Lock reconciler                               │
   │ → Get HLC timestamp                             │
   │ → Check queue for earlier commits               │
   │ → Pull and apply any earlier commits            │
   │ → Get content hash                              │
   │ → Create signed metadata (with author/date)     │
   │ → Commit to main with original metadata         │
   │ → Mark as applied                               │
   │ → Advertise to peers (full metadata)            │
   │ → Unlock                                        │
   └─────────────────────────────────────────────────┘

2. RECEIVE ADVERTISEMENT
   ┌─────────────────────────────────────────────────┐
   │ Peer receives AdvertiseCommit RPC               │
   │ → Update local HLC                              │
   │ → Add to queue (sorted by HLC)                  │
   │ → Compare incoming HLC with HEAD's HLC          │
   │   → If incoming is LATER: pull and fast-forward │
   │   → If incoming is EARLIER: schedule reorder    │
   │     (debounce to batch multiple)                │
   └─────────────────────────────────────────────────┘

3. REORDER (after debounce, on temp branch)
   ┌─────────────────────────────────────────────────┐
   │ Find common ancestor                            │
   │ → Create replay_tmp branch at ancestor          │
   │ → Collect commits from all branches             │
   │ → Deduplicate by content hash                   │
   │ → Sort all by HLC                               │
   │ → Cherry-pick in order:                         │
   │   → Use --no-commit + recommit with orig meta   │
   │   → On conflict: drop, mark applied (FWW)       │
   │ → Fast-forward main to replay_tmp (atomic)      │
   │ → Delete replay_tmp                             │
   └─────────────────────────────────────────────────┘

4. PERIODIC SYNC (every 30s)
   ┌─────────────────────────────────────────────────┐
   │ Request commits since our HEAD's HLC            │
   │ → Fills gaps from missed advertisements         │
   │ → Peers respond with any newer commits          │
   └─────────────────────────────────────────────────┘
```

---

## Why This Works

1. **Pull-first reduces reordering**: Most commits won't need cherry-pick
2. **Same HLC ordering**: When reordering is needed, all peers use same order
3. **Same conflict resolution**: FWW drops same commits on all peers (marked applied)
4. **Hash stability**: Original metadata ensures identical hashes after replay
5. **Crash safety**: Temp branch protects main from corruption
6. **Debounce**: Batches reorders under burst, reduces O(N²) to O(N)
7. **Recovery**: Periodic sync fills gaps from missed advertisements
8. **Same result**: All peers converge to identical main branch

---

## Integration Test Impact

### Library Interface: UNCHANGED

| Method | Signature | Status |
|--------|-----------|--------|
| `ExecAndCommit` | `(ExecFunc, string) → (string, error)` | Same |
| `Commit` | `(string) → (string, error)` | Same |
| `GetAllCommits` | `() → ([]Commit, error)` | Same |
| `GetLastCommit` | `(string) → (Commit, error)` | Same |

### Required Test Changes

1. **Mock ServerSyncer**: Update to implement `AdvertiseCommit` and `RequestCommitsSince` RPCs
2. **Proto regeneration**: Run `protoc` after updating .proto file
3. **Convergence tests**: Should work unchanged (check HEAD and history)

---

## Testing Plan

### Unit Tests
- [ ] HLC timestamp comparison and ordering
- [ ] HLC update from remote timestamps
- [ ] HLC future-skew rejection
- [ ] Commit metadata serialization/deserialization (with author/date)
- [ ] Metadata signature verification/rejection path
- [ ] Queue insertion maintains HLC order
- [ ] Queue GetPendingBefore/After
- [ ] Queue marks rejected commits as applied
- [ ] Debounce batches multiple reorders

### Integration Tests
- [ ] Single peer commit cycle
- [ ] Two peer synchronization (no conflicts)
- [ ] Pull-first optimization works (no cherry-pick needed)
- [ ] Concurrent commits converge (cherry-pick fallback)
- [ ] Conflict resolution (FWW) - rejected commits marked applied
- [ ] All peers have identical main branch (same commit hashes)
- [ ] Crash recovery (main untouched if crash during replay)
- [ ] Missing ads recovery via RequestCommitsSince

---

## Implementation Order

1. **Create `hlc.go`** - HLC implementation with tests
2. **Create `metadata.go`** - CommitMetadata with author/date and signing
3. **Create `commit_ad.go`** - CommitAd struct with full metadata
4. **Create `queue.go`** - CommitQueue with tests (including rejected marking)
5. **Update `proto/dbsyncer.proto`** - Add AdvertiseCommit, RequestCommitsSince; remove reliance on AdvertiseHead/RequestHead
6. **Regenerate protobuf** - `protoc` command
7. **Create `reconciler.go`** - Reconciler with debounce, temp branch, recovery
8. **Update `syncer.go`** - Add AdvertiseCommit, RequestCommitsSince handlers
9. **Update `distributed.go`** - Add AdvertiseCommit broadcast
10. **Update `sql.go`** - Modify Commit/ExecAndCommit with original metadata
11. **Update `db.go`** - Integrate reconciler
12. **Remove old merge logic** - Clean up Merge() function and disable AdvertiseHead/RequestHead pipeline
13. **Update integration tests** - Mock for new RPCs
14. **Run convergence tests** - Verify identical hashes across peers
15. **Purge legacy/unused code** - Delete old head-event handlers, unused proto RPCs/messages, unused event types, legacy merge helpers, and related tests/fixtures so the codebase has no dead paths
