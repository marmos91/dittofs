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
