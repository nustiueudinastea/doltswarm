package core

import (
	"time"
)

// FinalizedBase represents the watermark position in local history.
// Commits at or before this point are considered finalized and immutable.
type FinalizedBase struct {
	HLC        HLCTimestamp // The watermark HLC
	CommitHash string       // Commit at watermark position
	UpdatedAt  time.Time    // When this was last updated
}

// IsZero returns true if the finalized base is not set
func (f FinalizedBase) IsZero() bool {
	return f.HLC.IsZero() && f.CommitHash == ""
}

// WatermarkConfig holds configuration for watermark computation
type WatermarkConfig struct {
	// MinPeersForWatermark is the minimum number of active peers required
	// to compute a watermark. Default: 2
	MinPeersForWatermark int

	// SlackDuration is the buffer subtracted from the checkpoint-verified HLC
	// to account for clock drift and network delays. Default: 30s
	SlackDuration time.Duration

	// ActivityTTL is how long before a peer is considered inactive.
	// Default: 5m
	ActivityTTL time.Duration
}

// DefaultWatermarkConfig returns the default watermark configuration
func DefaultWatermarkConfig() WatermarkConfig {
	return WatermarkConfig{
		MinPeersForWatermark: 2,
		SlackDuration:        30 * time.Second,
		ActivityTTL:          5 * time.Minute,
	}
}

// PeerActivity tracks when we last heard from a peer, their head HLC,
// and their checkpoints (for safe watermark computation).
type PeerActivity struct {
	PeerID      string
	LastSeenAt  time.Time
	HeadHLC     HLCTimestamp
	Checkpoints []Checkpoint // Recent checkpoints from this peer's digest
}

// WatermarkResult holds the computed watermark and the checkpoint to finalize.
type WatermarkResult struct {
	// Watermark is the HLC threshold for finalization (highest commonly-known - slack)
	Watermark HLCTimestamp
	// FinalizeCheckpoint is the highest commonly-known checkpoint with HLC <= Watermark.
	// This is the checkpoint that should become the finalized base.
	// SAFETY: Only finalize checkpoints, never arbitrary local commits.
	FinalizeCheckpoint Checkpoint
}

// ComputeWatermark computes the finalization watermark from peer activities.
// Returns the watermark HLC and true if computation was successful,
// or a zero HLC and false if there aren't enough active peers.
//
// SAFETY: The watermark is computed from checkpoints, not just head HLCs.
// A commit is considered "commonly known" only if it appears in checkpoints
// from at least minPeersForWatermark active peers. This ensures we don't
// finalize a prefix that some peer might be missing.
//
// Algorithm:
// 1. Filter to active peers only
// 2. Find checkpoints that appear in at least minPeersForWatermark peers
// 3. Watermark = highest commonly-known checkpoint HLC - slack
func ComputeWatermark(activities []PeerActivity, cfg WatermarkConfig) (HLCTimestamp, bool) {
	result, ok := ComputeWatermarkWithCheckpoint(activities, cfg)
	if !ok {
		return HLCTimestamp{}, false
	}
	return result.Watermark, true
}

// ComputeWatermarkWithCheckpoint computes the watermark and returns the checkpoint
// that should become the finalized base. This ensures we only finalize commonly-known
// checkpoints, never arbitrary local commits.
func ComputeWatermarkWithCheckpoint(activities []PeerActivity, cfg WatermarkConfig) (WatermarkResult, bool) {
	now := time.Now()

	// Filter to active peers only
	var activePeers []PeerActivity
	for _, a := range activities {
		if now.Sub(a.LastSeenAt) <= cfg.ActivityTTL {
			activePeers = append(activePeers, a)
		}
	}

	// Require minimum peers for safety
	if len(activePeers) < cfg.MinPeersForWatermark {
		return WatermarkResult{}, false
	}

	// Count how many peers have each checkpoint (by HLC string as key)
	// We use HLC as the identity since commit hashes can differ due to replays
	checkpointCounts := make(map[string]int)
	checkpointsByKey := make(map[string]Checkpoint)

	for _, peer := range activePeers {
		// Track which checkpoints this peer has (dedup by HLC)
		seenInPeer := make(map[string]bool)
		for _, cp := range peer.Checkpoints {
			key := cp.HLC.String()
			if !seenInPeer[key] {
				seenInPeer[key] = true
				checkpointCounts[key]++
				// Keep the checkpoint with a commit hash if available
				if existing, ok := checkpointsByKey[key]; !ok || (existing.CommitHash == "" && cp.CommitHash != "") {
					checkpointsByKey[key] = cp
				}
			}
		}
	}

	// Find the highest HLC that appears in at least minPeersForWatermark peers
	var highestCommonlyKnown Checkpoint
	for key, count := range checkpointCounts {
		if count >= cfg.MinPeersForWatermark {
			cp := checkpointsByKey[key]
			if highestCommonlyKnown.HLC.IsZero() || highestCommonlyKnown.HLC.Less(cp.HLC) {
				highestCommonlyKnown = cp
			}
		}
	}

	if highestCommonlyKnown.HLC.IsZero() {
		return WatermarkResult{}, false
	}

	// Apply slack: subtract SlackDuration from wall time
	slackNanos := cfg.SlackDuration.Nanoseconds()
	watermark := HLCTimestamp{
		Wall:    highestCommonlyKnown.HLC.Wall - slackNanos,
		Logical: 0,  // Reset logical to 0 at the slack boundary
		PeerID:  "", // Watermark has no peer ID
	}

	// Find the highest commonly-known checkpoint with HLC <= watermark
	// This is the checkpoint that should become the finalized base
	var finalizeCheckpoint Checkpoint
	for key, count := range checkpointCounts {
		if count >= cfg.MinPeersForWatermark {
			cp := checkpointsByKey[key]
			// Only consider checkpoints at or before watermark
			if cp.HLC.Less(watermark) || cp.HLC.Equal(watermark) {
				if finalizeCheckpoint.HLC.IsZero() || finalizeCheckpoint.HLC.Less(cp.HLC) {
					finalizeCheckpoint = cp
				}
			}
		}
	}

	return WatermarkResult{
		Watermark:          watermark,
		FinalizeCheckpoint: finalizeCheckpoint,
	}, true
}

// IsFinalized returns true if the given HLC is at or before the watermark
func IsFinalized(hlc, watermark HLCTimestamp) bool {
	if watermark.IsZero() {
		return false
	}
	return hlc.Less(watermark) || hlc.Equal(watermark)
}

// IsAfterFinalizedBase returns true if the HLC is strictly after the finalized base
func IsAfterFinalizedBase(hlc HLCTimestamp, base *FinalizedBase) bool {
	if base == nil || base.HLC.IsZero() {
		return true // No finalized base, everything is after
	}
	return base.HLC.Less(hlc)
}
