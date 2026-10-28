package index

import (
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// TestRemovalsSurviveToSince checks that a removal is reported until settled,
// at the index and without a crash: three files written and then truncated,
// deallocated and deleted. Since from 0 yields each held extent and each
// marker at its version, in version order; Since from a removal's own version
// does not yield it, since only versions strictly above are yielded. Settling
// below the removal keeps its marker, settling at it drops it, and Since then
// yields no marker. A Since that compared at or above from would hand the
// engine a removal it has already recorded, and a Settle that dropped at or
// below a version it was not given would lose an unrecorded removal.
func TestRemovalsSurviveToSince(t *testing.T) {
	var fs files
	a, b, c := fid(1), fid(2), fid(3)
	ra, rb, rc := rec(1, 0), rec(1, 64), rec(1, 128)
	change(t, &fs, a, func(f *file) error {
		f.write(write(0, 16, ra, 2))
		f.truncate(8, ver(5))
		return nil
	})
	change(t, &fs, b, func(f *file) error {
		f.write(write(0, 16, rb, 3))
		return f.deallocate(ext(4, 4), ver(6))
	})
	change(t, &fs, c, func(f *file) error {
		f.write(write(0, 16, rc, 4))
		f.delete(ver(7))
		return nil
	})

	cases := []struct {
		id      core.FileID
		removal core.Version
		want    []Change
	}{
		{a, ver(5), []Change{
			{Kind: core.ChangeWrite, Extent: ext(0, 8), Version: ver(2), Record: ra},
			{Kind: core.ChangeTruncate, Extent: ext(8, math.MaxInt64-8), Version: ver(5)},
		}},
		{b, ver(6), []Change{
			{Kind: core.ChangeWrite, Extent: ext(0, 4), Version: ver(3), Record: rb},
			{Kind: core.ChangeWrite, Extent: ext(8, 8), Version: ver(3), Record: rb},
			{Kind: core.ChangeDeallocate, Extent: ext(4, 4), Version: ver(6)},
		}},
		{c, ver(7), []Change{
			{Kind: core.ChangeDelete, Extent: ext(0, math.MaxInt64-0), Version: ver(7)},
		}},
	}
	// Held content at from is not yielded either: a write metadata already
	// applied would be committed again.
	if got, want := fs.since(a, ver(2), nil), cases[0].want[1:]; !slices.Equal(got, want) {
		t.Fatalf("since(a, 2):\n got  %+v\n want %+v", got, want)
	}
	for _, tc := range cases {
		if got := fs.since(tc.id, core.Version{}, nil); !slices.Equal(got, tc.want) {
			t.Fatalf("since(%v, 0):\n got  %+v\n want %+v", tc.id, got, tc.want)
		}
		for _, ch := range fs.since(tc.id, tc.removal, nil) {
			if ch.Kind != core.ChangeWrite {
				t.Fatalf("since(%v, %v) yielded the removal at its own version: %+v", tc.id, tc.removal, ch)
			}
		}

		fs.settleRemovals(tc.id, core.Version{Counter: tc.removal.Counter - 1})
		if n := markerCount(&fs, tc.id); n != 1 {
			t.Fatalf("markers of %v = %d after settling below its removal, want 1", tc.id, n)
		}
		fs.settleRemovals(tc.id, tc.removal)
		if n := markerCount(&fs, tc.id); n != 0 {
			t.Fatalf("markers of %v = %d after settling at its removal, want 0", tc.id, n)
		}
		for _, ch := range fs.since(tc.id, core.Version{}, nil) {
			if ch.Kind != core.ChangeWrite {
				t.Fatalf("since(%v, 0) yielded a settled marker: %+v", tc.id, ch)
			}
		}
	}
}

// TestSinceIsInVersionOrder checks Since's order: by version, a marker before
// held content of the same version, then by offset. Content written later at
// a lower offset comes after content written earlier at a higher one, which an
// offset-ordered read-out would get wrong. A marker and a fill can share a
// version, and the marker comes first so a caller applying them in order never
// removes the fill.
func TestSinceIsInVersionOrder(t *testing.T) {
	var fs files
	id := fid(1)
	r1, r2, r3 := rec(1, 0), rec(1, 64), rec(1, 128)
	change(t, &fs, id, func(f *file) error {
		f.write(write(32, 4, r1, 2))
		f.write(write(0, 4, r2, 4))
		if err := f.deallocate(ext(8, 4), ver(3)); err != nil {
			return err
		}
		f.fill(write(8, 4, r3, 3))
		return nil
	})
	want := []Change{
		{Kind: core.ChangeWrite, Extent: ext(32, 4), Version: ver(2), Record: r1},
		{Kind: core.ChangeDeallocate, Extent: ext(8, 4), Version: ver(3)},
		{Kind: core.ChangeWrite, Extent: ext(8, 4), Version: ver(3), Record: r3},
		{Kind: core.ChangeWrite, Extent: ext(0, 4), Version: ver(4), Record: r2},
	}
	if got := fs.since(id, core.Version{}, nil); !slices.Equal(got, want) {
		t.Fatalf("since:\n got  %+v\n want %+v", got, want)
	}
}

// TestSinceOrdersOneVersionByOffset checks the offset tie-break among items of
// one version. One record overwritten in every other slot leaves pieces of
// two versions interleaved by offset, so the sort must move them, and a sort
// that compared versions alone could hand back each version's pieces in any
// order, applying the file's content out of place.
func TestSinceOrdersOneVersionByOffset(t *testing.T) {
	var fs files
	id := fid(1)
	const slots = 64
	change(t, &fs, id, func(f *file) error {
		f.write(write(0, 4*slots, rec(1, 0), 2))
		for i := int64(1); i < slots; i += 2 {
			f.write(write(4*i, 4, rec(2, uint32(4*i)), 4))
		}
		return nil
	})
	offs := map[core.Version][]int64{}
	for _, c := range fs.since(id, core.Version{}, nil) {
		offs[c.Version] = append(offs[c.Version], c.Extent.Off)
	}
	for v, o := range offs {
		if len(o) != slots/2 || !slices.IsSorted(o) {
			t.Fatalf("offsets at %v = %v, want %d in offset order", v, o, slots/2)
		}
	}
}

// TestSinceHidesTheOffloadedBit checks that one record's piece split by its
// offloaded bit is yielded as one write: Since reports content, and nothing
// it reports may depend on the bit.
func TestSinceHidesTheOffloadedBit(t *testing.T) {
	var fs files
	id := fid(1)
	r := rec(1, 0)
	change(t, &fs, id, func(f *file) error {
		f.write(write(0, 16, r, 2))
		f.held.markOffloaded(ext(4, 4), ver(2), ver(2))
		return nil
	})
	want := []Change{{Kind: core.ChangeWrite, Extent: ext(0, 16), Version: ver(2), Record: r}}
	if got := fs.since(id, core.Version{}, nil); !slices.Equal(got, want) {
		t.Fatalf("since:\n got  %+v\n want %+v", got, want)
	}
}

// TestSinceOfAnAbsentFile checks that Since of a file with no entry yields
// nothing and creates no entry.
func TestSinceOfAnAbsentFile(t *testing.T) {
	var fs files
	if got := fs.since(fid(1), core.Version{}, nil); len(got) != 0 {
		t.Fatalf("since of an absent file = %+v", got)
	}
	if fs.len() != 0 {
		t.Fatalf("since of an absent file left an entry")
	}
}

// TestSinceReadsAConsistentView checks that Since sees no change half made. A
// writer replaces the two halves of the file and removes a third extent in
// one update, each time at new versions, while Since runs beside it; every
// Since must see both halves at one version and the removal just above it. A
// Since that read the held set without the file's lock, or read the held set
// and the markers in two steps, would see parts of different updates.
func TestSinceReadsAConsistentView(t *testing.T) {
	var fs files
	id := fid(1)
	change(t, &fs, id, func(f *file) error {
		f.write(write(0, 4, rec(1, 0), 1))
		f.write(write(4, 4, rec(1, 4), 1))
		return f.deallocate(ext(16, 4), ver(2))
	})

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for v := uint64(3); ; v++ {
			select {
			case <-done:
				return
			default:
			}
			_ = fs.update(id, func(f *file) error {
				f.settleRemovals(ver(v))
				f.write(write(0, 4, rec(core.SegmentID(v), 0), v))
				f.write(write(4, 4, rec(core.SegmentID(v), 4), v))
				return f.deallocate(ext(16, 4), ver(v+1))
			})
			v++
		}
	}()

	deadline := time.Now().Add(100 * time.Millisecond)
	var out []Change
	for time.Now().Before(deadline) {
		out = fs.since(id, core.Version{}, out[:0])
		if len(out) != 3 || out[0].Version != out[1].Version || out[2].Kind == core.ChangeWrite ||
			out[2].Version.Counter != out[0].Version.Counter+1 {
			close(done)
			wg.Wait()
			t.Fatalf("since saw a change half made: %+v", out)
		}
	}
	close(done)
	wg.Wait()
}
