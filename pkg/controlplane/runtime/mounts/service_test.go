package mounts

import (
	"sync"
	"testing"
)

func TestRecordAndCount(t *testing.T) {
	mt := NewTracker()
	if mt.Count() != 0 {
		t.Fatalf("new tracker count = %d, want 0", mt.Count())
	}
	mt.Record("10.0.0.1", "nfs", "share1", "path-data")
	mt.Record("10.0.0.2", "smb", "share1", nil)
	if mt.Count() != 2 {
		t.Errorf("count = %d, want 2", mt.Count())
	}
}

// Re-recording the same protocol:client:share key overwrites rather than
// duplicating, so the count stays at one.
func TestRecordSameKeyOverwrites(t *testing.T) {
	mt := NewTracker()
	mt.Record("10.0.0.1", "nfs", "share1", "v1")
	mt.Record("10.0.0.1", "nfs", "share1", "v2")
	if mt.Count() != 1 {
		t.Errorf("count = %d, want 1 (overwrite)", mt.Count())
	}
	got := mt.List()
	if len(got) != 1 || got[0].AdapterData != "v2" {
		t.Errorf("AdapterData = %v, want overwritten v2", got)
	}
}

func TestRemove(t *testing.T) {
	mt := NewTracker()
	mt.Record("10.0.0.1", "nfs", "share1", nil)

	if !mt.Remove("10.0.0.1", "nfs", "share1") {
		t.Error("Remove of existing mount returned false")
	}
	if mt.Count() != 0 {
		t.Errorf("count after remove = %d, want 0", mt.Count())
	}
	// Removing again (or a non-existent key) returns false.
	if mt.Remove("10.0.0.1", "nfs", "share1") {
		t.Error("Remove of missing mount returned true")
	}
}

func TestRemoveByClient(t *testing.T) {
	mt := NewTracker()
	mt.Record("10.0.0.1", "nfs", "share1", nil)
	mt.Record("10.0.0.1", "smb", "share2", nil)
	mt.Record("10.0.0.2", "nfs", "share1", nil)

	if !mt.RemoveByClient("10.0.0.1") {
		t.Error("RemoveByClient returned false for present client")
	}
	if mt.Count() != 1 {
		t.Errorf("count = %d, want 1 (only 10.0.0.2 remains)", mt.Count())
	}
	if mt.RemoveByClient("10.0.0.99") {
		t.Error("RemoveByClient returned true for absent client")
	}
}

func TestRemoveAllByProtocol(t *testing.T) {
	mt := NewTracker()
	mt.Record("a", "nfs", "s1", nil)
	mt.Record("b", "nfs", "s2", nil)
	mt.Record("c", "smb", "s1", nil)

	if n := mt.RemoveAllByProtocol("nfs"); n != 2 {
		t.Errorf("RemoveAllByProtocol(nfs) = %d, want 2", n)
	}
	if mt.Count() != 1 {
		t.Errorf("count = %d, want 1", mt.Count())
	}
	if n := mt.RemoveAllByProtocol("nfs"); n != 0 {
		t.Errorf("second RemoveAllByProtocol(nfs) = %d, want 0", n)
	}
}

func TestRemoveAll(t *testing.T) {
	mt := NewTracker()
	mt.Record("a", "nfs", "s1", nil)
	mt.Record("b", "smb", "s2", nil)
	if n := mt.RemoveAll(); n != 2 {
		t.Errorf("RemoveAll = %d, want 2", n)
	}
	if mt.Count() != 0 {
		t.Errorf("count = %d, want 0", mt.Count())
	}
}

func TestListAndListByProtocol(t *testing.T) {
	mt := NewTracker()
	mt.Record("a", "nfs", "s1", nil)
	mt.Record("b", "smb", "s2", nil)
	mt.Record("c", "nfs", "s3", nil)

	if got := mt.List(); len(got) != 3 {
		t.Errorf("List len = %d, want 3", len(got))
	}
	nfs := mt.ListByProtocol("nfs")
	if len(nfs) != 2 {
		t.Errorf("ListByProtocol(nfs) len = %d, want 2", len(nfs))
	}
	for _, m := range nfs {
		if m.Protocol != "nfs" {
			t.Errorf("ListByProtocol(nfs) returned %q", m.Protocol)
		}
	}
	if got := mt.ListByProtocol("none"); len(got) != 0 {
		t.Errorf("ListByProtocol(none) len = %d, want 0", len(got))
	}
}

// List must return copies: mutating a returned record must not affect the
// tracker's internal state.
func TestListReturnsCopies(t *testing.T) {
	mt := NewTracker()
	mt.Record("a", "nfs", "s1", "orig")

	got := mt.List()
	got[0].ShareName = "tampered"
	got[0].AdapterData = "tampered"

	fresh := mt.List()
	if fresh[0].ShareName != "s1" || fresh[0].AdapterData != "orig" {
		t.Errorf("internal state mutated via returned copy: %+v", fresh[0])
	}
}

// The tracker is documented thread-safe; run it under -race with concurrent
// writers, removers, and readers to validate the locking.
func TestTrackerConcurrent(t *testing.T) {
	mt := NewTracker()
	var wg sync.WaitGroup
	const n = 50

	for i := 0; i < n; i++ {
		wg.Add(3)
		client := string(rune('A' + (i % 26)))
		go func() { defer wg.Done(); mt.Record(client, "nfs", "s", nil) }()
		go func() { defer wg.Done(); _ = mt.List() }()
		go func() { defer wg.Done(); mt.Remove(client, "nfs", "s") }()
	}
	wg.Wait()
	// No assertion on final count (racy by construction); the point is the
	// race detector finding no data races.
	_ = mt.Count()
}
