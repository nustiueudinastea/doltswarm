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

// GetPending returns a copy of all pending commits (already sorted)
func (q *CommitQueue) GetPending() []*CommitAd {
	q.mu.RLock()
	defer q.mu.RUnlock()

	result := make([]*CommitAd, len(q.pending))
	copy(result, q.pending)
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
// This is critical to prevent livelock - rejected commits must also be marked as applied
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

// GetAppliedKeys returns all applied commit keys
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

// AppliedCount returns the count of applied commits
func (q *CommitQueue) AppliedCount() int {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return len(q.applied)
}
