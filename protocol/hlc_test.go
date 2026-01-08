package protocol

import (
	"testing"
	"time"
)

func TestHLCNow(t *testing.T) {
	hlc := NewHLC("peer1")

	ts1 := hlc.Now()
	ts2 := hlc.Now()

	// Second timestamp should be greater
	if !ts1.Less(ts2) {
		t.Errorf("Expected ts1 < ts2, got ts1=%v, ts2=%v", ts1, ts2)
	}

	// PeerID should be set
	if ts1.PeerID != "peer1" {
		t.Errorf("Expected PeerID='peer1', got '%s'", ts1.PeerID)
	}
}

func TestHLCUpdate(t *testing.T) {
	hlc := NewHLC("peer1")

	// Get initial timestamp
	ts1 := hlc.Now()

	// Simulate receiving a timestamp from the future
	remote := HLCTimestamp{
		Wall:    ts1.Wall + 1000000000, // 1 second in the future
		Logical: 5,
		PeerID:  "peer2",
	}

	hlc.Update(remote)

	// Next local timestamp should be after the remote
	ts2 := hlc.Now()
	if !remote.Less(ts2) {
		t.Errorf("Expected remote < ts2 after update, got remote=%v, ts2=%v", remote, ts2)
	}
}

func TestHLCTimestampLess(t *testing.T) {
	tests := []struct {
		name     string
		a, b     HLCTimestamp
		expected bool
	}{
		{
			name:     "different wall time",
			a:        HLCTimestamp{Wall: 100, Logical: 5, PeerID: "z"},
			b:        HLCTimestamp{Wall: 200, Logical: 0, PeerID: "a"},
			expected: true,
		},
		{
			name:     "same wall, different logical",
			a:        HLCTimestamp{Wall: 100, Logical: 1, PeerID: "z"},
			b:        HLCTimestamp{Wall: 100, Logical: 2, PeerID: "a"},
			expected: true,
		},
		{
			name:     "same wall and logical, different peer",
			a:        HLCTimestamp{Wall: 100, Logical: 1, PeerID: "a"},
			b:        HLCTimestamp{Wall: 100, Logical: 1, PeerID: "b"},
			expected: true,
		},
		{
			name:     "equal timestamps",
			a:        HLCTimestamp{Wall: 100, Logical: 1, PeerID: "a"},
			b:        HLCTimestamp{Wall: 100, Logical: 1, PeerID: "a"},
			expected: false,
		},
		{
			name:     "greater wall time",
			a:        HLCTimestamp{Wall: 200, Logical: 0, PeerID: "a"},
			b:        HLCTimestamp{Wall: 100, Logical: 5, PeerID: "z"},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.a.Less(tt.b)
			if result != tt.expected {
				t.Errorf("Expected %v.Less(%v) = %v, got %v", tt.a, tt.b, tt.expected, result)
			}
		})
	}
}

func TestHLCTimestampEqual(t *testing.T) {
	ts1 := HLCTimestamp{Wall: 100, Logical: 1, PeerID: "peer1"}
	ts2 := HLCTimestamp{Wall: 100, Logical: 1, PeerID: "peer1"}
	ts3 := HLCTimestamp{Wall: 100, Logical: 2, PeerID: "peer1"}

	if !ts1.Equal(ts2) {
		t.Errorf("Expected ts1.Equal(ts2) = true")
	}

	if ts1.Equal(ts3) {
		t.Errorf("Expected ts1.Equal(ts3) = false")
	}
}

func TestHLCTimestampString(t *testing.T) {
	ts := HLCTimestamp{Wall: 100, Logical: 5, PeerID: "peer1"}
	expected := "100:5:peer1"
	if ts.String() != expected {
		t.Errorf("Expected String()='%s', got '%s'", expected, ts.String())
	}
}

func TestHLCTimestampIsZero(t *testing.T) {
	zero := HLCTimestamp{}
	nonZero := HLCTimestamp{Wall: 100, Logical: 0, PeerID: "peer1"}

	if !zero.IsZero() {
		t.Errorf("Expected IsZero() = true for zero value")
	}

	if nonZero.IsZero() {
		t.Errorf("Expected IsZero() = false for non-zero value")
	}
}

func TestHLCTimestampIsFutureSkew(t *testing.T) {
	now := time.Now().UnixNano()

	// Normal timestamp (not skewed)
	normal := HLCTimestamp{Wall: now, Logical: 0, PeerID: "peer1"}
	if normal.IsFutureSkew() {
		t.Errorf("Expected IsFutureSkew() = false for normal timestamp")
	}

	// Future timestamp within allowed skew
	withinSkew := HLCTimestamp{Wall: now + int64(MaxClockSkew/2), Logical: 0, PeerID: "peer1"}
	if withinSkew.IsFutureSkew() {
		t.Errorf("Expected IsFutureSkew() = false for timestamp within skew")
	}

	// Future timestamp beyond allowed skew
	beyondSkew := HLCTimestamp{Wall: now + int64(MaxClockSkew*2), Logical: 0, PeerID: "peer1"}
	if !beyondSkew.IsFutureSkew() {
		t.Errorf("Expected IsFutureSkew() = true for timestamp beyond skew")
	}
}

func TestHLCConcurrency(t *testing.T) {
	hlc := NewHLC("peer1")

	// Generate many timestamps concurrently
	done := make(chan HLCTimestamp, 100)
	for i := 0; i < 100; i++ {
		go func() {
			done <- hlc.Now()
		}()
	}

	// Collect all timestamps
	timestamps := make([]HLCTimestamp, 100)
	for i := 0; i < 100; i++ {
		timestamps[i] = <-done
	}

	// All timestamps should be unique
	seen := make(map[string]bool)
	for _, ts := range timestamps {
		key := ts.String()
		if seen[key] {
			t.Errorf("Duplicate timestamp found: %s", key)
		}
		seen[key] = true
	}
}
