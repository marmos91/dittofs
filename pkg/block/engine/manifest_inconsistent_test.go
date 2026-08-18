package engine

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// A manifest row whose ID carries no parseable offset cannot be placed in the
// file. The bytes it points at are still in the store, so reporting the range as
// a hole would hand the caller zeros for data that exists — the failure a reader
// cannot detect, because a zero-filled hole is a legitimate answer for a sparse
// file and arrives without an error to distinguish it.
func TestFindRowCoveringOffset_MalformedIDIsNotAHole(t *testing.T) {
	rows := []*block.FileChunk{
		{ID: "payload-1", DataSize: 4096},         // no "/offset" suffix
		{ID: "payload-1/4096", DataSize: 1 << 20}, // well-formed, covers past 4096
	}

	rw, err := findRowCoveringOffset(rows, 0)
	if err == nil {
		t.Fatalf("offset 0 returned rw=%v err=nil; an unplaceable row must not read back as a hole", rw)
	}
	if !errors.Is(err, block.ErrManifestInconsistent) {
		t.Fatalf("err = %v, want it to wrap ErrManifestInconsistent", err)
	}
}

// The absence of a row is the real representation of a hole, and stays one: a
// sparse file must keep reading back as zeros rather than becoming an error.
func TestFindRowCoveringOffset_AbsentRowIsStillAHole(t *testing.T) {
	rows := []*block.FileChunk{
		{ID: "payload-1/0", DataSize: 4096},
		// nothing covers [4096, 8192)
		{ID: "payload-1/8192", DataSize: 4096},
	}

	rw, err := findRowCoveringOffset(rows, 5000)
	if err != nil {
		t.Fatalf("unexpected error for a genuine hole: %v", err)
	}
	if rw != nil {
		t.Fatalf("rw = %+v, want nil — no row covers offset 5000", rw)
	}
}

// An unplaceable row must not cost the reader the rest of the file. Offsets some
// other row covers still resolve, so a payload with one damaged range stays
// readable everywhere else.
func TestFindRowCoveringOffset_UnplaceableRowDoesNotPoisonCoveredOffsets(t *testing.T) {
	rows := []*block.FileChunk{
		{ID: "payload-1", DataSize: 4096},         // unplaceable
		{ID: "payload-1/4096", DataSize: 1 << 20}, // covers 4096..1052672
	}

	rw, err := findRowCoveringOffset(rows, 8192)
	if err != nil {
		t.Fatalf("unexpected error for a covered offset: %v", err)
	}
	if rw == nil || rw.absOffset != 4096 {
		t.Fatalf("rw = %+v, want the row starting at 4096", rw)
	}
}

func TestFindRowCoveringOffset_WellFormedRowResolves(t *testing.T) {
	rows := []*block.FileChunk{
		{ID: "payload-1/0", DataSize: 4096},
		{ID: "payload-1/4096", DataSize: 1 << 20},
	}

	rw, err := findRowCoveringOffset(rows, 4096)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rw == nil {
		t.Fatal("rw = nil, want the row starting at 4096")
	}
	if rw.absOffset != 4096 {
		t.Fatalf("absOffset = %d, want 4096", rw.absOffset)
	}
}

// Overlapping rows resolve to the greatest start, matching the indexed badger
// lookup. A truncate narrows a straddling row to the new size and a later write
// re-carves from an earlier boundary, so the narrowed row keeps claiming bytes
// the new row also covers; the newer row holds what the last write put there.
func TestFindRowCoveringOffset_OverlapResolvesToGreatestStart(t *testing.T) {
	rows := []*block.FileChunk{
		{ID: "payload-1/0", DataSize: 8192},    // narrowed survivor, reached first
		{ID: "payload-1/4096", DataSize: 4096}, // newer row over [4096, 8192)
	}

	rw, err := findRowCoveringOffset(rows, 5000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rw == nil || rw.absOffset != 4096 {
		t.Fatalf("rw = %+v, want the row starting at 4096", rw)
	}

	// Offsets only the older row covers still resolve to it.
	rw, err = findRowCoveringOffset(rows, 1000)
	if err != nil || rw == nil || rw.absOffset != 0 {
		t.Fatalf("findRowCoveringOffset(1000) = %+v, %v; want the row at 0", rw, err)
	}
}
