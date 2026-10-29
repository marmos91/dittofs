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
			tr.PopAllPending()
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
