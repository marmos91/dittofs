//go:build engine_conformance

package conformance

import (
	"bytes"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
)

// §8.2 deallocate: an extent larger than the journal's free capacity
// deallocates and reads as zeros; a deallocate that staged zeros would be
// refused by the journal's capacity first.
func TestRFC8A_Deallocate(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")
	write(t, f, id, 0, bytes.Repeat([]byte{'a'}, 4096))
	const huge = 1 << 50 // past any journal's capacity
	if err := f.E.Deallocate(t.Context(), f.Share.ID, id, 0, huge); err != nil {
		t.Fatalf("Deallocate: %v", err)
	}
	if got := read(t, f, id, 0, 4096); !bytes.Equal(got, make([]byte, 4096)) {
		t.Error("the deallocated extent does not read as zeros")
	}
}

// §5.1 sync before existence, the truncate half: the size a truncate's phase
// 1 commits is backed by synced records across a host crash right after it.
func TestRFC8A_TruncateSyncBeforeExistence(t *testing.T) {
	f := enginetest.New(t)
	h, id := f.File("a")
	p := bytes.Repeat([]byte{'t'}, 4096)
	write(t, f, id, 0, p)
	if err := f.E.Truncate(t.Context(), f.Share.ID, id, 1024); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if got := f.Committed(h).Size; got != 1024 {
		t.Fatalf("committed size %d after phase 1, want 1024", got)
	}
	f.Crash()
	if got := read(t, f, id, 0, 1024); !bytes.Equal(got, p[:1024]) {
		t.Error("after the crash the truncated file does not read its first 1024 bytes")
	}
}

// A truncate up crashed after its journal step, before the group commit
// commits it: the restart serves the new size from the journal.
func TestRFC8A_TruncateUpSurvivesACrash(t *testing.T) {
	f := enginetest.New(t)
	h, id := f.File("a")
	write(t, f, id, 0, []byte("abcd"))
	if err := f.E.Truncate(t.Context(), f.Share.ID, id, 1<<20); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	f.Crash()
	eventually(t, "the restart serves the grown size", func() bool { return f.Committed(h).Size == 1<<20 })
}
