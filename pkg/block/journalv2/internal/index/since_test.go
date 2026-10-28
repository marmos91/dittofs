package index

import (
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// TestRemovalsSurviveToSince is RFC 1 §9.4's "removals survive to Since" row
// at the index, without the crash: three files written and then truncated,
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
			{Extent: ext(0, 8), Version: ver(2), Record: ra},
			{Removal: RemovalTruncate, Extent: ext(8, math.MaxInt64-8), Version: ver(5)},
		}},
		{b, ver(6), []Change{
			{Extent: ext(0, 4), Version: ver(3), Record: rb},
			{Extent: ext(8, 8), Version: ver(3), Record: rb},
			{Removal: RemovalDeallocate, Extent: ext(4, 4), Version: ver(6)},
		}},
		{c, ver(7), []Change{
			{Removal: RemovalDelete, Extent: ext(0, math.MaxInt64-0), Version: ver(7)},
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
			if ch.Removal != 0 {
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
			if ch.Removal != 0 {
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
		{Extent: ext(32, 4), Version: ver(2), Record: r1},
		{Removal: RemovalDeallocate, Extent: ext(8, 4), Version: ver(3)},
		{Extent: ext(8, 4), Version: ver(3), Record: r3},
		{Extent: ext(0, 4), Version: ver(4), Record: r2},
	}
	if got := fs.since(id, core.Version{}, nil); !slices.Equal(got, want) {
		t.Fatalf("since:\n got  %+v\n want %+v", got, want)
	}
}

// TestSinceHidesTheOffloadedBit checks that one record's piece split by its
// offloaded bit is yielded as one write: Since reports content, and nothing
// it reports may depend on the bit (RFC 1 §2).
func TestSinceHidesTheOffloadedBit(t *testing.T) {
	var fs files
	id := fid(1)
	r := rec(1, 0)
	change(t, &fs, id, func(f *file) error {
		f.write(write(0, 16, r, 2))
		f.held.markOffloaded(ext(4, 4), ver(2), ver(2))
		return nil
	})
	want := []Change{{Extent: ext(0, 16), Version: ver(2), Record: r}}
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

// TestSinceReadsAConsistentView is R49: Since sees no write half made. A
// writer replaces the two halves of the file in one update, each time at a
// new version, while Since runs beside it; every Since must see both halves
// at one version. A Since that read the held set without the file's lock, or
// in two steps, would see one half old and one new.
func TestSinceReadsAConsistentView(t *testing.T) {
	var fs files
	id := fid(1)
	change(t, &fs, id, func(f *file) error {
		f.write(write(0, 4, rec(1, 0), 1))
		f.write(write(4, 4, rec(1, 4), 1))
		return nil
	})

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for v := uint64(2); ; v++ {
			select {
			case <-done:
				return
			default:
			}
			_ = fs.update(id, func(f *file) error {
				f.write(write(0, 4, rec(core.SegmentID(v), 0), v))
				f.write(write(4, 4, rec(core.SegmentID(v), 4), v))
				return nil
			})
		}
	}()

	deadline := time.Now().Add(100 * time.Millisecond)
	var out []Change
	for time.Now().Before(deadline) {
		out = fs.since(id, core.Version{}, out[:0])
		if len(out) != 2 || out[0].Version != out[1].Version {
			close(done)
			wg.Wait()
			t.Fatalf("since saw a write half made: %+v", out)
		}
	}
	close(done)
	wg.Wait()
}
