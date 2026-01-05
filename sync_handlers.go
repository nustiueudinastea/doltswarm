package doltswarm

import (
	"context"
	"fmt"
)

// HandleAdvertiseCommit ingests a commit advertisement received over an external transport.
// The transport is responsible for authenticating the caller and providing fromPeerID.
func (db *DB) HandleAdvertiseCommit(ctx context.Context, fromPeerID string, ad *CommitAd) (accepted bool, err error) {
	if ad == nil {
		return false, fmt.Errorf("nil commit advertisement")
	}

	// Reject if HLC is too far in the future (clock skew guard).
	if ad.HLC.IsFutureSkew() {
		db.log.Warnf("Rejecting commit from %s due to clock skew: HLC %v", fromPeerID, ad.HLC)
		db.reconciler.GetQueue().MarkApplied(ad) // avoid retries
		return false, nil
	}

	// Verify metadata signature before enqueue.
	if !db.VerifyMetadataSignature(ad) {
		db.log.Warnf("Rejecting commit from %s due to invalid signature", fromPeerID)
		db.reconciler.GetQueue().MarkApplied(ad) // avoid retries
		if db.reconciler.onCommitRejected != nil {
			db.reconciler.onCommitRejected(ad, RejectedInvalidSignature)
		}
		return false, nil
	}

	// Add to reconciler queue.
	db.reconciler.OnRemoteCommit(ad)

	// Handle the commit (pull or reorder as needed).
	go func() {
		_ = ctx
		if err := db.reconciler.HandleIncomingCommit(ad); err != nil {
			db.log.Warnf("Failed to handle incoming commit from %s: %v", fromPeerID, err)
		}
	}()

	return true, nil
}

// HandleRequestCommitsSince returns commit advertisements after the provided HLC (used to repair missed adverts).
func (db *DB) HandleRequestCommitsSince(ctx context.Context, since HLCTimestamp) ([]*CommitAd, error) {
	_ = ctx

	commits, err := db.GetAllCommits()
	if err != nil {
		return nil, err
	}

	result := make([]*CommitAd, 0, len(commits))
	for _, c := range commits {
		meta, err := ParseCommitMetadata(c.Message)
		if err != nil {
			continue // Skip old-style commits
		}
		if since.Less(meta.HLC) {
			result = append(result, &CommitAd{
				PeerID:      meta.HLC.PeerID,
				HLC:         meta.HLC,
				ContentHash: meta.ContentHash,
				CommitHash:  c.Hash,
				Message:     meta.Message,
				Author:      meta.Author,
				Email:       meta.Email,
				Date:        meta.Date,
				Signature:   meta.Signature,
			})
		}
	}

	return result, nil
}
