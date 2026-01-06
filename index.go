package doltswarm

import (
	"context"
	"time"
)

type CommitStatus int

const (
	CommitStatusUnknown CommitStatus = iota
	CommitStatusPending
	CommitStatusAvailable
	CommitStatusApplied
	CommitStatusRejected
	CommitStatusDeferred
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

	// SetHead records the node's current canonical head (for digest publishing).
	SetHead(ctx context.Context, headHLC HLCTimestamp, headHash string) error
	Head(ctx context.Context) (headHLC HLCTimestamp, headHash string, ok bool, err error)

	// RecentCheckpoints returns newest->oldest checkpoints for digest negotiation.
	RecentCheckpoints(ctx context.Context, limit int) ([]Checkpoint, error)
	AppendCheckpoint(ctx context.Context, cp Checkpoint) error
}
