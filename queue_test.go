package doltswarm

import (
	"testing"
	"time"
)

func TestCommitQueueAdd(t *testing.T) {
	q := NewCommitQueue()

	ad1 := &CommitAd{
		PeerID: "peer1",
		HLC:    HLCTimestamp{Wall: 100, Logical: 0, PeerID: "peer1"},
	}
	ad2 := &CommitAd{
		PeerID: "peer2",
		HLC:    HLCTimestamp{Wall: 200, Logical: 0, PeerID: "peer2"},
	}
	ad3 := &CommitAd{
		PeerID: "peer3",
		HLC:    HLCTimestamp{Wall: 150, Logical: 0, PeerID: "peer3"},
	}

	// Add in non-sorted order
	if !q.Add(ad2) {
		t.Error("Expected Add to return true for new commit")
	}
	if !q.Add(ad1) {
		t.Error("Expected Add to return true for new commit")
	}
	if !q.Add(ad3) {
		t.Error("Expected Add to return true for new commit")
	}

	// Should be sorted by HLC
	pending := q.GetPending()
	if len(pending) != 3 {
		t.Fatalf("Expected 3 pending commits, got %d", len(pending))
	}

	// Verify order: ad1 (100) < ad3 (150) < ad2 (200)
	if pending[0].HLC.Wall != 100 {
		t.Errorf("Expected first commit Wall=100, got %d", pending[0].HLC.Wall)
	}
	if pending[1].HLC.Wall != 150 {
		t.Errorf("Expected second commit Wall=150, got %d", pending[1].HLC.Wall)
	}
	if pending[2].HLC.Wall != 200 {
		t.Errorf("Expected third commit Wall=200, got %d", pending[2].HLC.Wall)
	}
}

func TestCommitQueueAddDuplicate(t *testing.T) {
	q := NewCommitQueue()

	ad := &CommitAd{
		PeerID: "peer1",
		HLC:    HLCTimestamp{Wall: 100, Logical: 0, PeerID: "peer1"},
	}

	if !q.Add(ad) {
		t.Error("Expected first Add to return true")
	}
	if q.Add(ad) {
		t.Error("Expected second Add to return false for duplicate")
	}

	if q.PendingCount() != 1 {
		t.Errorf("Expected 1 pending commit, got %d", q.PendingCount())
	}
}

func TestCommitQueueAddAfterApplied(t *testing.T) {
	q := NewCommitQueue()

	ad := &CommitAd{
		PeerID: "peer1",
		HLC:    HLCTimestamp{Wall: 100, Logical: 0, PeerID: "peer1"},
	}

	q.MarkApplied(ad)

	if q.Add(ad) {
		t.Error("Expected Add to return false for already applied commit")
	}
}

func TestCommitQueueGetPendingBefore(t *testing.T) {
	q := NewCommitQueue()

	ad1 := &CommitAd{HLC: HLCTimestamp{Wall: 100, Logical: 0, PeerID: "p1"}}
	ad2 := &CommitAd{HLC: HLCTimestamp{Wall: 200, Logical: 0, PeerID: "p2"}}
	ad3 := &CommitAd{HLC: HLCTimestamp{Wall: 300, Logical: 0, PeerID: "p3"}}

	q.Add(ad1)
	q.Add(ad2)
	q.Add(ad3)

	// Get commits before HLC 250
	before := q.GetPendingBefore(HLCTimestamp{Wall: 250, Logical: 0, PeerID: "x"})
	if len(before) != 2 {
		t.Fatalf("Expected 2 commits before 250, got %d", len(before))
	}

	// Get commits before HLC 100 with peer "a" (should be none since p1 > a)
	before = q.GetPendingBefore(HLCTimestamp{Wall: 100, Logical: 0, PeerID: "a"})
	if len(before) != 0 {
		t.Fatalf("Expected 0 commits before Wall=100/PeerID=a, got %d", len(before))
	}

	// Get commits before HLC 100 with peer "x" (should be 1 since p1 < x)
	before = q.GetPendingBefore(HLCTimestamp{Wall: 100, Logical: 0, PeerID: "x"})
	if len(before) != 1 {
		t.Fatalf("Expected 1 commit before Wall=100/PeerID=x (p1 < x), got %d", len(before))
	}
}

func TestCommitQueueGetPendingAfter(t *testing.T) {
	q := NewCommitQueue()

	ad1 := &CommitAd{HLC: HLCTimestamp{Wall: 100, Logical: 0, PeerID: "p1"}}
	ad2 := &CommitAd{HLC: HLCTimestamp{Wall: 200, Logical: 0, PeerID: "p2"}}
	ad3 := &CommitAd{HLC: HLCTimestamp{Wall: 300, Logical: 0, PeerID: "p3"}}

	q.Add(ad1)
	q.Add(ad2)
	q.Add(ad3)

	// Get commits after HLC 150
	after := q.GetPendingAfter(HLCTimestamp{Wall: 150, Logical: 0, PeerID: "x"})
	if len(after) != 2 {
		t.Fatalf("Expected 2 commits after 150, got %d", len(after))
	}

	// Get commits after HLC 300 (should be none)
	after = q.GetPendingAfter(HLCTimestamp{Wall: 300, Logical: 0, PeerID: "x"})
	if len(after) != 0 {
		t.Fatalf("Expected 0 commits after 300, got %d", len(after))
	}
}

func TestCommitQueueTakePending(t *testing.T) {
	q := NewCommitQueue()

	ad1 := &CommitAd{HLC: HLCTimestamp{Wall: 100, Logical: 0, PeerID: "p1"}}
	ad2 := &CommitAd{HLC: HLCTimestamp{Wall: 200, Logical: 0, PeerID: "p2"}}

	q.Add(ad1)
	q.Add(ad2)

	taken := q.TakePending()
	if len(taken) != 2 {
		t.Fatalf("Expected 2 taken commits, got %d", len(taken))
	}

	if q.HasPending() {
		t.Error("Expected no pending commits after TakePending")
	}
}

func TestCommitQueueRemove(t *testing.T) {
	q := NewCommitQueue()

	ad1 := &CommitAd{HLC: HLCTimestamp{Wall: 100, Logical: 0, PeerID: "p1"}}
	ad2 := &CommitAd{HLC: HLCTimestamp{Wall: 200, Logical: 0, PeerID: "p2"}}

	q.Add(ad1)
	q.Add(ad2)

	q.Remove(ad1)

	if q.PendingCount() != 1 {
		t.Errorf("Expected 1 pending commit after Remove, got %d", q.PendingCount())
	}

	pending := q.GetPending()
	if pending[0].HLC.Wall != 200 {
		t.Errorf("Expected remaining commit Wall=200, got %d", pending[0].HLC.Wall)
	}
}

func TestCommitQueueMarkApplied(t *testing.T) {
	q := NewCommitQueue()

	ad := &CommitAd{HLC: HLCTimestamp{Wall: 100, Logical: 0, PeerID: "p1"}}

	if q.IsApplied(ad.Key()) {
		t.Error("Expected IsApplied to return false before marking")
	}

	q.MarkApplied(ad)

	if !q.IsApplied(ad.Key()) {
		t.Error("Expected IsApplied to return true after marking")
	}
}

func TestCommitQueueConcurrency(t *testing.T) {
	q := NewCommitQueue()

	// Concurrently add many commits
	done := make(chan bool, 100)
	for i := 0; i < 100; i++ {
		go func(n int) {
			ad := &CommitAd{
				HLC: HLCTimestamp{
					Wall:    int64(n * 100),
					Logical: 0,
					PeerID:  "peer",
				},
			}
			q.Add(ad)
			done <- true
		}(i)
	}

	for i := 0; i < 100; i++ {
		<-done
	}

	if q.PendingCount() != 100 {
		t.Errorf("Expected 100 pending commits, got %d", q.PendingCount())
	}

	// Verify all commits are sorted
	pending := q.GetPending()
	for i := 1; i < len(pending); i++ {
		if !pending[i-1].HLC.Less(pending[i].HLC) {
			t.Errorf("Commits not sorted: %v >= %v", pending[i-1].HLC, pending[i].HLC)
		}
	}
}

func TestCommitQueueGetAppliedKeys(t *testing.T) {
	q := NewCommitQueue()

	ad1 := &CommitAd{HLC: HLCTimestamp{Wall: 100, Logical: 0, PeerID: "p1"}}
	ad2 := &CommitAd{HLC: HLCTimestamp{Wall: 200, Logical: 0, PeerID: "p2"}}

	q.MarkApplied(ad1)
	q.MarkApplied(ad2)

	keys := q.GetAppliedKeys()
	if len(keys) != 2 {
		t.Fatalf("Expected 2 applied keys, got %d", len(keys))
	}

	if q.AppliedCount() != 2 {
		t.Errorf("Expected AppliedCount() = 2, got %d", q.AppliedCount())
	}
}

func TestCommitQueueRejectedCommitMarkedApplied(t *testing.T) {
	// This test verifies the critical behavior that rejected commits
	// (e.g., due to conflicts) must be marked as applied to prevent livelock
	q := NewCommitQueue()

	ad := &CommitAd{
		HLC:     HLCTimestamp{Wall: 100, Logical: 0, PeerID: "p1"},
		Message: "conflicting commit",
	}

	q.Add(ad)

	// Simulate conflict rejection: remove from pending and mark applied
	q.Remove(ad)
	q.MarkApplied(ad)

	// The commit should not be re-addable
	if q.Add(ad) {
		t.Error("Rejected commit should not be re-addable after being marked applied")
	}

	if q.HasPending() {
		t.Error("Queue should have no pending commits")
	}

	if !q.IsApplied(ad.Key()) {
		t.Error("Rejected commit should be marked as applied")
	}
}

func makeCommitAd(wall int64, peerID string) *CommitAd {
	return &CommitAd{
		PeerID:      peerID,
		HLC:         HLCTimestamp{Wall: wall, Logical: 0, PeerID: peerID},
		ContentHash: "hash",
		CommitHash:  "commit",
		Message:     "test",
		Author:      peerID,
		Email:       peerID + "@test.com",
		Date:        time.Now(),
	}
}
