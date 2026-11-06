package index

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// totalsOf reads the file's counters through the handle's own table.
func totalsOf(ix *Index, id core.FileID) heldTotals {
	var t heldTotals
	ix.fs.view(id, func(f *file) { t = f.totals() })
	return t
}

// TestFailWindowServesTheLastSyncedWriteAndDropsOnlyThatRecord: a failed
// record is dropped by its record, not by its version, so the synced write
// under it is held again and is not among the dropped pieces.
func TestFailWindowServesTheLastSyncedWriteAndDropsOnlyThatRecord(t *testing.T) {
	ix := New(1 << 20)
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 5))
	if _, err := ix.LiftSuperseded(testFile, ver(5)); err != nil {
		t.Fatal(err)
	}
	writeRetained(t, ix, write(0, 4096, rec(2, 0), 10))

	fw, _, err := ix.FailWindow(testFile, []WindowRecord{failed(rec(2, 0), 10)})
	if err != nil {
		t.Fatalf("FailWindow: %v", err)
	}
	got, ok := ix.Lookup(testFile, 0)
	if !ok || got.Version != ver(5) || got.Record != rec(1, 0) {
		t.Fatalf("Lookup = %+v, %v; want the synced v5 in record (1,0)", got, ok)
	}
	want := []Piece{piece(0, 4096, rec(2, 0), 0, 10, false)}
	if !slices.Equal(fw.Dropped, want) {
		t.Fatalf("Dropped = %+v, want %+v", fw.Dropped, want)
	}
	if got := ix.Retained(testFile); len(got) != 0 {
		t.Fatalf("Retained = %+v, want none", got)
	}
}

// TestFailWindowLeavesOtherRecordsAlone: only the part a window record
// overwrote comes back; the rest of the file, and the rest of the record that
// overwrote only part of another, is untouched.
func TestFailWindowLeavesOtherRecordsAlone(t *testing.T) {
	ix := New(1 << 20)
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 5))
	writeRetained(t, ix, write(8192, 4096, rec(1, 4096), 7))
	if _, err := ix.LiftSuperseded(testFile, ver(7)); err != nil {
		t.Fatal(err)
	}
	writeRetained(t, ix, write(8192, 2048, rec(2, 0), 9)) // over v7's first half

	if _, _, err := ix.FailWindow(testFile, []WindowRecord{failed(rec(2, 0), 9)}); err != nil {
		t.Fatal(err)
	}
	var pieces []Piece
	ix.fs.view(testFile, func(f *file) { pieces = f.held.pieces(nil) })
	// The held-again half and the untouched half of v7 are adjacent parts of
	// one record and read back as one piece.
	want := []Piece{
		piece(0, 4096, rec(1, 0), 0, 5, false),
		piece(8192, 4096, rec(1, 4096), 0, 7, false),
	}
	if !slices.Equal(pieces, want) {
		t.Fatalf("held:\n got  %+v\n want %+v", pieces, want)
	}
}

// TestFailWindowDropsAGapUnderNoHold: a part the failed record superseded that
// a removal has since taken is under no content, and with no snapshot hold to
// keep it, it is dropped rather than left retained under a version that never
// comes round again.
func TestFailWindowDropsAGapUnderNoHold(t *testing.T) {
	ix := New(1 << 20)
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 5))
	writeRetained(t, ix, write(0, 4096, rec(2, 0), 10))
	if _, err := ix.fs.update(testFile, func(f *file) error { return f.deallocate(ext(0, 4096), ver(11)) }); err != nil {
		t.Fatal(err)
	}

	if _, _, err := ix.FailWindow(testFile, []WindowRecord{failed(rec(2, 0), 10)}); err != nil {
		t.Fatal(err)
	}
	if got := ix.Retained(testFile); len(got) != 0 {
		t.Fatalf("Retained = %+v, want the gap dropped", got)
	}
	if sb := ix.SegmentBytes(1); sb.Retained != 0 {
		t.Fatalf("SegmentBytes(1).Retained = %d, want 0", sb.Retained)
	}
}

// TestFailWindowNamesTheCoveringVersions: a part newer content outside the
// window has covered stays retained by that content, and the version is
// returned for the caller to lift if it has synced.
func TestFailWindowNamesTheCoveringVersions(t *testing.T) {
	ix := New(1 << 20)
	writeRetained(t, ix, write(0, 4096, rec(1, 0), 5))
	writeRetained(t, ix, write(0, 4096, rec(2, 0), 10))
	writeRetained(t, ix, write(0, 2048, rec(3, 0), 12))

	fw, _, err := ix.FailWindow(testFile, []WindowRecord{failed(rec(2, 0), 10)})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fw.Covers, []core.Version{ver(12)}) {
		t.Fatalf("Covers = %v, want [v12]", fw.Covers)
	}
	ret := ix.Retained(testFile)
	if len(ret) != 1 || ret[0].Version != ver(5) || ret[0].By != ver(12) || ret[0].Extent != ext(0, 2048) {
		t.Fatalf("Retained = %+v, want v5 [0,2K) by v12", ret)
	}
}

// TestFailWindowDeltaMatchesTheFileCounters: the delta FailWindow returns is
// the change in the file's held, dirty and retained bytes.
func TestFailWindowDeltaMatchesTheFileCounters(t *testing.T) {
	ix := New(1 << 20)
	writeRetained(t, ix, write(0, 8192, rec(1, 0), 5))
	writeRetained(t, ix, write(4096, 8192, rec(2, 0), 10))
	before := totalsOf(ix, testFile)

	_, d, err := ix.FailWindow(testFile, []WindowRecord{failed(rec(2, 0), 10)})
	if err != nil {
		t.Fatal(err)
	}
	after := totalsOf(ix, testFile)
	if d.HeldBytes != after.held-before.held || d.DirtyBytes != after.dirty-before.dirty ||
		d.RetainedBytes != after.retained-before.retained {
		t.Fatalf("delta %+v does not match totals %+v -> %+v", d, before, after)
	}
	if d.HeldBytes == 0 || d.RetainedBytes == 0 {
		t.Fatalf("delta %+v moved nothing; the test proves nothing", d)
	}
}

// TestFailWindowOfUnknownFile: failing a window of a file with no entry does
// nothing and creates no entry.
func TestFailWindowOfUnknownFile(t *testing.T) {
	ix := New(1 << 20)
	fw, d, err := ix.FailWindow(testFile, []WindowRecord{failed(rec(2, 0), 10)})
	if err != nil || d != (HeldDelta{}) || len(fw.Dropped) != 0 || ix.fs.len() != 0 {
		t.Fatalf("FailWindow of an unknown file = %+v, %+v, %v, entries %d", fw, d, err, ix.fs.len())
	}
}
