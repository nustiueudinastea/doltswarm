package core

import (
	"context"
	"time"
)

type CommitStatus int

const (
	CommitStatusUnknown CommitStatus = iota
	CommitStatusPending
	CommitStatusApplied
	CommitStatusRejected
)

type CommitIndexEntry struct {
	HLC        HLCTimestamp
	CommitHash string
	Status     CommitStatus

	NextRetryAt time.Time
	UpdatedAt   time.Time
}

// CommitIndex is a local, non-replicated index used by the gossip-first pipeline.
// Implementations MUST be deterministic with respect to HLC ordering.
type CommitIndex interface {
	Close() error

	// Upsert stores the canonical commit hash for an HLC (once known) and its status.
	Upsert(ctx context.Context, e CommitIndexEntry) error

	// Get fetches the entry for a given HLC, if present.
	Get(ctx context.Context, hlc HLCTimestamp) (CommitIndexEntry, bool, error)

	// ListAfter returns entries strictly after `after`, ordered by HLC.
	ListAfter(ctx context.Context, after HLCTimestamp, limit int) ([]CommitIndexEntry, error)

	// FinalizedBase returns the current finalized base.
	// Returns the base, true if set, or zero value, false if not set.
	FinalizedBase(ctx context.Context) (FinalizedBase, bool, error)

	// SetFinalizedBase updates the finalized base. The update is only applied
	// if the new base's HLC is strictly greater than the current base (monotonic).
	SetFinalizedBase(ctx context.Context, fb FinalizedBase) error
}
