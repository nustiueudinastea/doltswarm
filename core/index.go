package core

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

	// FinalizedBase returns the current finalized base (watermark position).
	// Returns the base, true if set, or zero value, false if not set.
	FinalizedBase(ctx context.Context) (FinalizedBase, bool, error)

	// SetFinalizedBase updates the finalized base. The update is only applied
	// if the new base's HLC is strictly greater than the current base (monotonic).
	SetFinalizedBase(ctx context.Context, fb FinalizedBase) error

	// GetOriginEventIDMapping checks if an origin_event_id has been resubmitted.
	// Returns the resubmission's HLC and true if found, or zero and false if not.
	GetOriginEventIDMapping(ctx context.Context, originEventID string) (resubmissionHLC HLCTimestamp, found bool, err error)

	// SetOriginEventIDMapping records that originEventID was resubmitted with newHLC.
	// This is used for idempotency to prevent duplicate resubmissions.
	SetOriginEventIDMapping(ctx context.Context, originEventID string, newHLC HLCTimestamp) error
}
