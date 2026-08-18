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

// Non-overlap is an invariant the carve maintains, not one the lookup may
// assume: the row this walk reaches first need not be the one the indexed
// badger path (largest start) returns, and both read back as ordinary data.
func TestFindRowCoveringOffset_OverlappingRowsAreReported(t *testing.T) {
	rows := []*block.FileChunk{
		{ID: "payload-1/0", DataSize: 8192},
		{ID: "payload-1/4096", DataSize: 4096}, // overlaps [4096, 8192)
	}

	rw, err := findRowCoveringOffset(rows, 5000)
	if err == nil {
		t.Fatalf("offset 5000 returned rw=%v err=nil; two rows cover it", rw)
	}
	if !errors.Is(err, block.ErrManifestInconsistent) {
		t.Fatalf("err = %v, want it to wrap ErrManifestInconsistent", err)
	}
	// Offsets only one row covers stay readable.
	rw, err = findRowCoveringOffset(rows, 1000)
	if err != nil || rw == nil || rw.absOffset != 0 {
		t.Fatalf("findRowCoveringOffset(1000) = %+v, %v; want the row at 0", rw, err)
	}
}
