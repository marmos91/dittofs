package metadata

import (
	"fmt"
	"testing"
)

// A pop removes pending state while a flush may still be committing it. A
// second flush, truncate or clone reconciliation must keep waiting on the
// same lock until the first commit finishes, including across shutdown drains.
func TestPendingFlushLockSurvivesPop(t *testing.T) {
	tr := NewPendingWritesTracker()
	for _, all := range []bool{false, true} {
		h := FileHandle("share:file")
		tr.RecordWrite(h, &WriteOperation{Handle: h, NewSize: 100}, false)
		mu := tr.GetFlushLock(h)
		mu.Lock()
		if all {
			for _, handle := range tr.PendingHandles() {
				tr.PopPending(handle)
			}
		} else {
			tr.PopPending(h)
		}
		next := tr.GetFlushLock(h)
		acquired := next.TryLock()
		if acquired {
			next.Unlock()
		}
		mu.Unlock()
		if acquired {
			t.Fatal("pending pop replaced an owned flush lock")
		}
	}
	// The bank stays bounded across distinct handles, without any lock deletion.
	for i := 0; i < 10000; i++ {
		tr.GetFlushLock(FileHandle(fmt.Sprintf("share:%d", i)))
	}
	if len(tr.flushLocks) != 256 {
		t.Fatal("flush lock bank grew")
	}
}

// Shutdown snapshots handles, then pops each under its stable flush lock.
func TestPendingHandlesDrainReturnsAllEntries(t *testing.T) {
	tr := NewPendingWritesTracker()
	want := map[string]uint64{}
	for i := 0; i < 8; i++ {
		key := fmt.Sprintf("share:f%d", i)
		h := FileHandle(key)
		tr.RecordWrite(h, &WriteOperation{Handle: h, NewSize: uint64(100 + i)}, false)
		want[key] = uint64(100 + i)
	}
	handles := tr.PendingHandles()
	if len(handles) != len(want) || tr.Count() != len(want) {
		t.Fatal("snapshot lost pending entries")
	}
	for _, handle := range handles {
		mu := tr.GetFlushLock(handle)
		mu.Lock()
		state, ok := tr.PopPending(handle)
		mu.Unlock()
		if !ok || state.MaxSize != want[string(handle)] {
			t.Fatalf("state for %q differs: %+v", handle, state)
		}
	}
	if tr.Count() != 0 {
		t.Fatal("drain left pending entries")
	}
}

// TestRestorePending_ReinstatesAndMergesRacingWrite asserts that a state popped
// for a flush is put back by RestorePending when the flush fails (so the buffered
// size is not lost), and that a write which raced in after the pop is not
// clobbered: the size keeps the max and the setuid-clear latches on.
func TestRestorePending_ReinstatesAndMergesRacingWrite(t *testing.T) {
	tr := NewPendingWritesTracker()
	h := FileHandle("share:restore")

	// A deferred write buffers size 100; the flush pops it, then fails.
	tr.RecordWrite(h, &WriteOperation{Handle: h, NewSize: 100}, true)
	state, ok := tr.PopPending(h)
	if !ok {
		t.Fatal("PopPending: expected buffered state")
	}
	if _, ok := tr.GetPending(h); ok {
		t.Fatal("state must be gone immediately after pop")
	}

	// No racing write: the state is reinstated verbatim so a later flush retries.
	tr.RestorePending(h, state)
	got, ok := tr.GetPending(h)
	if !ok || got.MaxSize != 100 {
		t.Fatalf("restore: got %+v ok=%v, want MaxSize=100", got, ok)
	}

	// Racing write: pop again, a newer write (size 200) lands before the failed
	// flush restores the old state. The newer size must win; setuid-clear latches.
	old, _ := tr.PopPending(h)
	tr.RecordWrite(h, &WriteOperation{Handle: h, NewSize: 200}, false)
	tr.RestorePending(h, old)
	merged, ok := tr.GetPending(h)
	if !ok {
		t.Fatal("merged state missing after restore")
	}
	if merged.MaxSize != 200 {
		t.Fatalf("merged MaxSize = %d, want 200 (racing write must not be clobbered)", merged.MaxSize)
	}
	if !merged.ClearSetuidSetgid {
		t.Fatal("restored setuid-clear must latch onto the merged state")
	}
}
