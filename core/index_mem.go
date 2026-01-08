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

	headSet  bool
	headHLC  HLCTimestamp
	headHash string

	checkpoints []Checkpoint // newest->oldest
}

func NewMemoryCommitIndex() *MemoryCommitIndex {
	return &MemoryCommitIndex{
		entries: make(map[string]CommitIndexEntry),
	}
}

func (m *MemoryCommitIndex) Close() error { return nil }

func (m *MemoryCommitIndex) resetForRebuild() {
	m.entries = make(map[string]CommitIndexEntry)
	m.headSet = false
	m.headHLC = HLCTimestamp{}
	m.headHash = ""
	m.checkpoints = nil
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

func (m *MemoryCommitIndex) SetHead(_ context.Context, headHLC HLCTimestamp, headHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.headSet = true
	m.headHLC = headHLC
	m.headHash = headHash
	return nil
}

func (m *MemoryCommitIndex) Head(_ context.Context) (HLCTimestamp, string, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.headHLC, m.headHash, m.headSet, nil
}

func (m *MemoryCommitIndex) RecentCheckpoints(_ context.Context, limit int) ([]Checkpoint, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if limit <= 0 || len(m.checkpoints) <= limit {
		out := make([]Checkpoint, len(m.checkpoints))
		copy(out, m.checkpoints)
		return out, nil
	}
	out := make([]Checkpoint, limit)
	copy(out, m.checkpoints[:limit])
	return out, nil
}

func (m *MemoryCommitIndex) AppendCheckpoint(_ context.Context, cp Checkpoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Prepend newest; keep simple and allow duplicates for now.
	m.checkpoints = append([]Checkpoint{cp}, m.checkpoints...)
	return nil
}
