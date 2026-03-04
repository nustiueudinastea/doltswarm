package core

import (
	"context"
	"sort"
	"sync"
	"time"
)

type MemoryCommitIndex struct {
	mu sync.RWMutex

	entries map[string]CommitIndexEntry

	// Finalization state
	finalizedBase *FinalizedBase
}

func NewMemoryCommitIndex() *MemoryCommitIndex {
	return &MemoryCommitIndex{
		entries: make(map[string]CommitIndexEntry),
	}
}

func (m *MemoryCommitIndex) Close() error { return nil }

func (m *MemoryCommitIndex) resetForRebuild() {
	m.entries = make(map[string]CommitIndexEntry)
	// Note: finalizedBase is intentionally preserved across rebuilds
	// as it represents durable finalization state that should not be lost.
}

func (m *MemoryCommitIndex) Upsert(_ context.Context, e CommitIndexEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if e.UpdatedAt.IsZero() {
		e.UpdatedAt = time.Now()
	}
	m.entries[e.HLC.String()] = e
	return nil
}

func (m *MemoryCommitIndex) Get(_ context.Context, hlc HLCTimestamp) (CommitIndexEntry, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.entries[hlc.String()]
	return e, ok, nil
}

func (m *MemoryCommitIndex) ListAfter(_ context.Context, after HLCTimestamp, limit int) ([]CommitIndexEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]CommitIndexEntry, 0)
	for _, e := range m.entries {
		if after.Less(e.HLC) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].HLC.Less(out[j].HLC)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// FinalizedBase returns the current finalized base
func (m *MemoryCommitIndex) FinalizedBase(_ context.Context) (FinalizedBase, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.finalizedBase == nil {
		return FinalizedBase{}, false, nil
	}
	return *m.finalizedBase, true, nil
}

// SetFinalizedBase updates the finalized base with monotonic advancement
func (m *MemoryCommitIndex) SetFinalizedBase(_ context.Context, fb FinalizedBase) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Only advance if the new base is strictly after the current one
	if m.finalizedBase != nil {
		if fb.HLC.Less(m.finalizedBase.HLC) || fb.HLC.Equal(m.finalizedBase.HLC) {
			return nil // Ignore non-advancing updates
		}
	}

	if fb.UpdatedAt.IsZero() {
		fb.UpdatedAt = time.Now()
	}
	m.finalizedBase = &fb
	return nil
}
