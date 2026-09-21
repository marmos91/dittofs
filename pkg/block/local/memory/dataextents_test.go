package memory

import (
	"context"
	"reflect"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journal"
)

// DataExtents reports the ranges the store was actually given, not the span of
// its buffer, so the residency cross-check that subtracts it does not call a
// share safe over bytes it never received.
//
// That exactness is only safe while EVERY path that puts bytes in the buffer
// records its range: a writer that skips the record reports its own bytes as a
// hole, and a hole reported over real data is zeros to the client — the same
// silent-loss failure from the opposite side. Recording lives in writeLocked,
// the single place the buffer is written, so a new writer that goes through it
// is covered by construction; one that does not is what these pin.
func extentsOf(t *testing.T, s *MemoryStore, id journal.FileID, size int64) [][2]uint64 {
	t.Helper()
	got, err := s.DataExtents(context.Background(), id, size)
	if err != nil {
		t.Fatalf("DataExtents: %v", err)
	}
	return got
}

// TestDataExtents_EveryWriterRecordsItsRange walks the writers one at a time.
// Each case drives exactly one path into the buffer so a missed record shows up
// as that path's own bytes reading back as a hole.
func TestDataExtents_EveryWriterRecordsItsRange(t *testing.T) {
	ctx := context.Background()

	t.Run("WriteAt", func(t *testing.T) {
		s := New()
		if err := s.WriteAt(ctx, "p", 0, make([]byte, 4096)); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		if got := extentsOf(t, s, "p", 4096); !reflect.DeepEqual(got, [][2]uint64{{0, 4096}}) {
			t.Errorf("after WriteAt, DataExtents = %v, want [[0 4096]]", got)
		}
	})

	// Hydrate is the path whose whole purpose is to put bytes in the buffer that
	// WriteAt never touched — a cold read's write-back. If it did not record,
	// every hydrated range would read back as a hole.
	t.Run("Hydrate", func(t *testing.T) {
		s := New()
		if err := s.Hydrate(ctx, "p", 0, make([]byte, 4096), 0); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		if got := extentsOf(t, s, "p", 4096); !reflect.DeepEqual(got, [][2]uint64{{0, 4096}}) {
			t.Errorf("after Hydrate, DataExtents = %v, want [[0 4096]]; a hydrated range "+
				"reported as a hole hands the client zeros over real bytes", got)
		}
	})

	// A hydrate landing past a write must not merge the untouched middle into
	// one span: that gap is the interior hole the cross-check has to see.
	t.Run("WriteAt and Hydrate leave the untouched middle uncovered", func(t *testing.T) {
		s := New()
		if err := s.WriteAt(ctx, "p", 0, make([]byte, 1024)); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		if err := s.Hydrate(ctx, "p", 8192, make([]byte, 1024), 0); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		want := [][2]uint64{{0, 1024}, {8192, 9216}}
		if got := extentsOf(t, s, "p", 9216); !reflect.DeepEqual(got, want) {
			t.Errorf("DataExtents = %v, want %v", got, want)
		}
	})

	t.Run("adjacent writes coalesce", func(t *testing.T) {
		s := New()
		for _, off := range []int64{0, 1024, 2048} {
			if err := s.WriteAt(ctx, "p", off, make([]byte, 1024)); err != nil {
				t.Fatalf("WriteAt(%d): %v", off, err)
			}
		}
		if got := extentsOf(t, s, "p", 3072); !reflect.DeepEqual(got, [][2]uint64{{0, 3072}}) {
			t.Errorf("DataExtents = %v, want one coalesced [[0 3072]]", got)
		}
	})

	// Writing the gap closed must merge the two sides rather than leave a seam.
	t.Run("backfilling a gap merges both sides", func(t *testing.T) {
		s := New()
		if err := s.WriteAt(ctx, "p", 0, make([]byte, 1024)); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		if err := s.WriteAt(ctx, "p", 2048, make([]byte, 1024)); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		if err := s.WriteAt(ctx, "p", 1024, make([]byte, 1024)); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		if got := extentsOf(t, s, "p", 3072); !reflect.DeepEqual(got, [][2]uint64{{0, 3072}}) {
			t.Errorf("DataExtents = %v, want [[0 3072]] once the gap is filled", got)
		}
	})

	t.Run("Truncate clips the record with the buffer", func(t *testing.T) {
		s := New()
		if err := s.WriteAt(ctx, "p", 0, make([]byte, 4096)); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		if err := s.Truncate(ctx, "p", 1024); err != nil {
			t.Fatalf("Truncate: %v", err)
		}
		if got := extentsOf(t, s, "p", 4096); !reflect.DeepEqual(got, [][2]uint64{{0, 1024}}) {
			t.Errorf("after Truncate, DataExtents = %v, want [[0 1024]]", got)
		}
	})

	// Growing adds no bytes, so it must add no coverage — an extent over a range
	// nobody wrote is the over-report this store no longer makes.
	t.Run("Truncate growing invents no coverage", func(t *testing.T) {
		s := New()
		if err := s.WriteAt(ctx, "p", 0, make([]byte, 1024)); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		if err := s.Truncate(ctx, "p", 8192); err != nil {
			t.Fatalf("Truncate: %v", err)
		}
		if got := extentsOf(t, s, "p", 8192); !reflect.DeepEqual(got, [][2]uint64{{0, 1024}}) {
			t.Errorf("after growing Truncate, DataExtents = %v, want [[0 1024]]", got)
		}
	})

	t.Run("Delete drops the record with the file", func(t *testing.T) {
		s := New()
		if err := s.WriteAt(ctx, "p", 0, make([]byte, 4096)); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		if err := s.Delete(ctx, "p"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if got := extentsOf(t, s, "p", 4096); len(got) != 0 {
			t.Errorf("after Delete, DataExtents = %v, want none", got)
		}
	})

	// Flush carves bytes to a sink and clears their unsynced charge; the bytes
	// stay in the buffer, so their coverage must stay too.
	t.Run("Flush leaves the record intact", func(t *testing.T) {
		s := New()
		if err := s.WriteAt(ctx, "p", 0, make([]byte, 4096)); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		flipAll := func(_ context.Context, r journal.Run) ([]journal.Extent, error) {
			return []journal.Extent{{Off: r.Extent.Off, Len: r.Extent.Len, State: journal.StateResident}}, nil
		}
		if err := s.Flush(ctx, "p", journal.FlushOptions{Force: true}, flipAll); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		if got := extentsOf(t, s, "p", 4096); !reflect.DeepEqual(got, [][2]uint64{{0, 4096}}) {
			t.Errorf("after Flush, DataExtents = %v, want [[0 4096]]", got)
		}
	})
}

// TestDataExtents_FullyWrittenFileIsFullyCovered asserts the invariant the
// engine's own DataExtents documents — the map may over-report but must never
// under-report — at its strongest point: a file every byte of which was
// written must come back covered end to end, with no seam a reader could take
// for a hole. A recording bug that splits or drops a range fails here rather
// than downstream in SEEK/READ_PLUS, where it surfaces as a client silently
// skipping real data.
func TestDataExtents_FullyWrittenFileIsFullyCovered(t *testing.T) {
	ctx := context.Background()
	const size = 64 * 1024
	s := New()
	// Written out of order and in uneven pieces, so full coverage depends on
	// the record coalescing rather than on the writes arriving tidily.
	for _, w := range [][2]int64{{32768, 16384}, {0, 4096}, {49152, 16384}, {4096, 28672}} {
		if err := s.WriteAt(ctx, "p", w[0], make([]byte, w[1])); err != nil {
			t.Fatalf("WriteAt(%d,%d): %v", w[0], w[1], err)
		}
	}
	got := extentsOf(t, s, "p", size)
	if !reflect.DeepEqual(got, [][2]uint64{{0, size}}) {
		t.Fatalf("DataExtents = %v, want one extent [[0 %d]] covering the whole file", got, size)
	}

	// Same claim stated as coverage, so a future split into several adjacent
	// extents still passes while a genuine gap does not.
	var covered uint64
	for _, e := range got {
		covered += e[1] - e[0]
	}
	if covered != size {
		t.Errorf("covered %d of %d bytes; the uncovered remainder reads as a hole over data "+
			"that was written", covered, size)
	}
}
