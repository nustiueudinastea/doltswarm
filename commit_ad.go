package doltswarm

import "time"

// CommitAd represents an advertised commit
type CommitAd struct {
	// Stable identity (never changes even after cherry-pick)
	PeerID      string
	HLC         HLCTimestamp
	ContentHash string

	// For fetching (hash on originating peer's main branch)
	CommitHash string

	// Original commit metadata (for replay with identical hash)
	Message string
	Author  string
	Email   string
	Date    time.Time

	// Signature over metadata (authoritative - proves original author created this content)
	Signature string
}

// Key returns unique identifier for this commit based on HLC
func (ad *CommitAd) Key() string {
	return ad.HLC.String()
}

// ToMetadata converts a CommitAd to CommitMetadata
func (ad *CommitAd) ToMetadata() *CommitMetadata {
	return &CommitMetadata{
		Version:     CommitMetadataVersion,
		Message:     ad.Message,
		HLC:         ad.HLC,
		ContentHash: ad.ContentHash,
		Author:      ad.Author,
		Email:       ad.Email,
		Date:        ad.Date,
		Signature:   ad.Signature,
	}
}

// CommitAdFromMetadata creates a CommitAd from CommitMetadata
func CommitAdFromMetadata(meta *CommitMetadata, commitHash string) *CommitAd {
	return &CommitAd{
		PeerID:      meta.HLC.PeerID,
		HLC:         meta.HLC,
		ContentHash: meta.ContentHash,
		CommitHash:  commitHash,
		Message:     meta.Message,
		Author:      meta.Author,
		Email:       meta.Email,
		Date:        meta.Date,
		Signature:   meta.Signature,
	}
}
