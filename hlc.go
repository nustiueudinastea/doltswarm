package doltswarm

import (
	"fmt"
	"sync"
	"time"
)

// MaxClockSkew is the maximum allowed clock skew for incoming HLC timestamps.
// Advertisements with HLC timestamps more than this ahead of local time are rejected.
const MaxClockSkew = 5 * time.Second

// HLCTimestamp represents a hybrid logical clock timestamp
type HLCTimestamp struct {
	Wall    int64  `json:"w"` // Wall time in nanoseconds
	Logical int32  `json:"l"` // Logical counter
	PeerID  string `json:"p"` // Peer ID for deterministic tie-breaking
}

// HLC is a hybrid logical clock
type HLC struct {
	mu        sync.Mutex
	timestamp HLCTimestamp
	peerID    string
}

// NewHLC creates a new HLC for the given peer
func NewHLC(peerID string) *HLC {
	return &HLC{
		peerID: peerID,
		timestamp: HLCTimestamp{
			Wall:    time.Now().UnixNano(),
			Logical: 0,
			PeerID:  peerID,
		},
	}
}

// Now returns a new timestamp for a local event
func (c *HLC) Now() HLCTimestamp {
	c.mu.Lock()
	defer c.mu.Unlock()

	physicalTime := time.Now().UnixNano()

	if c.timestamp.Wall >= physicalTime {
		c.timestamp.Logical++
	} else {
		c.timestamp.Wall = physicalTime
		c.timestamp.Logical = 0
	}

	return HLCTimestamp{
		Wall:    c.timestamp.Wall,
		Logical: c.timestamp.Logical,
		PeerID:  c.peerID,
	}
}

// Update updates the clock based on a received timestamp
func (c *HLC) Update(remote HLCTimestamp) {
	c.mu.Lock()
	defer c.mu.Unlock()

	physicalTime := time.Now().UnixNano()
	maxWall := max(c.timestamp.Wall, max(remote.Wall, physicalTime))

	if maxWall == c.timestamp.Wall && maxWall == remote.Wall {
		c.timestamp.Logical = max(c.timestamp.Logical, remote.Logical) + 1
	} else if maxWall == c.timestamp.Wall {
		c.timestamp.Logical++
	} else if maxWall == remote.Wall {
		c.timestamp.Wall = remote.Wall
		c.timestamp.Logical = remote.Logical + 1
	} else {
		c.timestamp.Wall = physicalTime
		c.timestamp.Logical = 0
	}
}

// Less returns true if h < other (total ordering)
func (h HLCTimestamp) Less(other HLCTimestamp) bool {
	if h.Wall != other.Wall {
		return h.Wall < other.Wall
	}
	if h.Logical != other.Logical {
		return h.Logical < other.Logical
	}
	return h.PeerID < other.PeerID
}

// Equal returns true if timestamps are equal
func (h HLCTimestamp) Equal(other HLCTimestamp) bool {
	return h.Wall == other.Wall && h.Logical == other.Logical && h.PeerID == other.PeerID
}

// IsZero returns true if this is a zero-value timestamp
func (h HLCTimestamp) IsZero() bool {
	return h.Wall == 0 && h.Logical == 0 && h.PeerID == ""
}

// String returns a unique string key for this timestamp
func (h HLCTimestamp) String() string {
	return fmt.Sprintf("%d:%d:%s", h.Wall, h.Logical, h.PeerID)
}

// IsFutureSkew checks if the HLC timestamp is too far in the future (clock skew guard)
func (h HLCTimestamp) IsFutureSkew() bool {
	now := time.Now().UnixNano()
	maxAllowed := now + int64(MaxClockSkew)
	return h.Wall > maxAllowed
}
