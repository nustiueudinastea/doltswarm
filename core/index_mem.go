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

	// Finalization state
	finalizedBase    *FinalizedBase
	originEventIDMap map[string]HLCTimestamp // origin_event_id -> resubmission HLC
}

func NewMemoryCommitIndex() *MemoryCommitIndex {
	return &MemoryCommitIndex{
		entries:          make(map[string]CommitIndexEntry),
		originEventIDMap: make(map[string]HLCTimestamp),
	}
}

func (m *MemoryCommitIndex) Close() error { return nil }

func (m *MemoryCommitIndex) resetForRebuild() {
	m.entries = make(map[string]CommitIndexEntry)
	m.headSet = false
	m.headHLC = HLCTimestamp{}
	m.headHash = ""
	m.checkpoints = nil
	// Note: finalizedBase and originEventIDMap are intentionally preserved across rebuilds
	// as they represent durable finalization state that should not be lost.
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

// GetOriginEventIDMapping checks if an origin_event_id has been resubmitted
func (m *MemoryCommitIndex) GetOriginEventIDMapping(_ context.Context, originEventID string) (HLCTimestamp, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	hlc, ok := m.originEventIDMap[originEventID]
	return hlc, ok, nil
}

// SetOriginEventIDMapping records a resubmission for idempotency
func (m *MemoryCommitIndex) SetOriginEventIDMapping(_ context.Context, originEventID string, newHLC HLCTimestamp) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.originEventIDMap == nil {
		m.originEventIDMap = make(map[string]HLCTimestamp)
	}
	m.originEventIDMap[originEventID] = newHLC
	return nil
}
