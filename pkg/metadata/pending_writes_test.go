package metadata

import (
	"fmt"
	"testing"
)

// TestPopAllPending_CleansFlushLocks asserts PopAllPending removes the per-file
// flush locks for every popped entry, so flushLocks does not grow unbounded
// across repeated record/pop cycles (mirrors the cleanup in PopPending).
func TestPopAllPending_CleansFlushLocks(t *testing.T) {
	tr := NewPendingWritesTracker()

	const rounds = 50
	const perRound = 20

	for r := 0; r < rounds; r++ {
		for i := 0; i < perRound; i++ {
			h := FileHandle(fmt.Appendf(nil, "share:%d-%d", r, i))
			// GetFlushLock populates flushLocks (as the real flush path does),
			// and RecordWrite populates pending.
			tr.GetFlushLock(h)
			tr.RecordWrite(h, &WriteOperation{Handle: h, NewSize: uint64(i + 1)}, false)
		}

		popped := tr.PopAllPending()
		if len(popped) != perRound {
			t.Fatalf("round %d: PopAllPending returned %d entries, want %d", r, len(popped), perRound)
		}

		tr.flushMu.Lock()
		n := len(tr.flushLocks)
		tr.flushMu.Unlock()
		if n != 0 {
			t.Fatalf("round %d: flushLocks has %d entries after PopAllPending, want 0", r, n)
		}
	}

	if c := tr.Count(); c != 0 {
		t.Fatalf("pending count = %d after all rounds, want 0", c)
	}
}

// TestPopAllPending_ReturnsAllEntries asserts the pop still surfaces every
// recorded entry with its state intact.
func TestPopAllPending_ReturnsAllEntries(t *testing.T) {
	tr := NewPendingWritesTracker()

	want := map[string]uint64{}
	for i := 0; i < 8; i++ {
		key := fmt.Sprintf("share:f%d", i)
		h := FileHandle(key)
		tr.RecordWrite(h, &WriteOperation{Handle: h, NewSize: uint64(100 + i)}, false)
		want[key] = uint64(100 + i)
	}

	popped := tr.PopAllPending()
	if len(popped) != len(want) {
		t.Fatalf("popped %d entries, want %d", len(popped), len(want))
	}
	for _, e := range popped {
		k := string(e.Handle)
		if e.State == nil {
			t.Fatalf("entry %q has nil state", k)
		}
		if e.State.MaxSize != want[k] {
			t.Errorf("entry %q MaxSize=%d, want %d", k, e.State.MaxSize, want[k])
		}
	}
	if c := tr.Count(); c != 0 {
		t.Fatalf("pending count = %d after pop, want 0", c)
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
