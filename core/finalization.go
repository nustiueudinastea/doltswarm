package core

import (
	"time"
)

// FinalizedBase represents the finalization position in local history.
// Commits at or before this point are considered finalized and immutable.
type FinalizedBase struct {
	HLC        HLCTimestamp // The stable HLC at finalization
	CommitHash string       // Commit at finalization position
	UpdatedAt  time.Time    // When this was last updated
}

// IsZero returns true if the finalized base is not set
func (f FinalizedBase) IsZero() bool {
	return f.HLC.IsZero() && f.CommitHash == ""
}

// IsFinalized returns true if the given HLC is at or before the stable HLC
func IsFinalized(hlc, stableHLC HLCTimestamp) bool {
	if stableHLC.IsZero() {
		return false
	}
	return hlc.Less(stableHLC) || hlc.Equal(stableHLC)
}

// IsAfterFinalizedBase returns true if the HLC is strictly after the finalized base
func IsAfterFinalizedBase(hlc HLCTimestamp, base *FinalizedBase) bool {
	if base == nil || base.HLC.IsZero() {
		return true // No finalized base, everything is after
	}
	return base.HLC.Less(hlc)
}
