package index

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// idInBucket returns a test FileID whose floor bucket is b and which shares no
// bucket with any ID in not. The ID is built rather than scanned, since the
// tests ask for thousands of them: bytes 14 and 15 carry the bucket — they
// are the top sixteen bits of the hash's second half, of which the index takes
// its twelve — and the counter in bytes 12 and 13 only makes the IDs distinct.
func idInBucket(b int, not ...core.FileID) core.FileID {
	for n := uint32(0); n <= 1<<20; n++ {
		id := core.NewFileID([16]byte{
			12: byte(n), 13: byte(n >> 8),
			14: byte(b&15) << 4, 15: byte(b >> 4),
		})
		if !slices.Contains(not, id) {
			return id
		}
	}
	panic(fmt.Sprintf("no test ID found in bucket %d", b))
}

// idOutside returns a test FileID whose floor bucket is not b: the n-th bucket
// after it. n must stay below floorBuckets-1, where the walk would come back
// around to b.
func idOutside(b int, n int) core.FileID { return idInBucket((b + 1 + n) % floorBuckets) }

// TestFloorBuckets is the "floor buckets" row: a fill of a file the index
// holds no entry for must be refused only when that file itself, or a file
// sharing its bucket, changed above the fill's asOf. Ten thousand other files
// deleted below their entries — all chosen outside X's bucket, since with
// random IDs about nine runs in ten some of that many land inside it, and one
// raise refuses X's fill; the row holds for files in other buckets — leave
// the fill of X accepted. A second file, Y, read before it was deleted and
// recreated, is refused: the delete dropped its entry but raised its bucket
// first. One journal-wide floor refuses the fill of X.
func TestFloorBuckets(t *testing.T) {
	var fs files
	const others = 10000

	// X: no entry, so X is checked against its bucket.
	x := idOutside(0, 0)
	bx := bucketOf(x)
	asOf := fs.asOf(x)
	for i := range others {
		tb := i % (floorBuckets - 1)
		if tb >= bx {
			tb++
		}
		id := idInBucket(tb)
		change(t, &fs, id, func(f *file) error {
			f.write(write(0, 4, rec(core.SegmentID(i+1), 0), uint64(2*i+1)))
			f.delete(ver(uint64(2*i + 2)))
			return nil
		})
		fs.settleRemovals(id, ver(uint64(2*i+2)))
		if present(&fs, id) {
			t.Fatalf("delete of other file %d settled but its entry is kept", i)
		}
	}
	if err := fs.fill(x, write(0, 4, rec(1, 0), 0), asOf); err != nil {
		t.Fatalf("fill of X after %d deletes in other buckets = %v, want accepted", others, err)
	}
	if ps := heldPieces(&fs, x); len(ps) != 1 || !ps[0].Offloaded {
		t.Fatalf("held after the accepted fill = %+v, want one offloaded piece", ps)
	}

	// Y: deleted and recreated with new content below the first read's asOf.
	y := idInBucket(bx, x)
	asOfY := fs.asOf(y)
	change(t, &fs, y, func(f *file) error {
		f.write(write(0, 4, rec(1, 0), 20))
		f.delete(ver(21))
		return nil
	})
	fs.settleRemovals(y, ver(21))
	if present(&fs, y) {
		t.Fatal("Y's entry kept after its delete was settled")
	}
	recreated := write(0, 4, rec(2, 0), 22)
	change(t, &fs, y, func(f *file) error { f.write(recreated); return nil })
	if err := fs.fill(y, write(0, 8, rec(3, 0), 0), asOfY); !errors.Is(err, core.ErrStaleFill) {
		t.Fatalf("fill of Y after its delete and recreate = %v, want ErrStaleFill", err)
	}
	want := []Piece{{Extent: ext(0, 4), Record: rec(2, 0), Version: ver(22)}}
	if got := heldPieces(&fs, y); !slices.Equal(got, want) {
		t.Fatalf("held after the refused fill = %+v, want the recreated write only", got)
	}
}

// TestDeleteInTheSameBucketRefuses is the coarse side of the buckets: dropping
// Z, a file in X's bucket, raises the bucket to Z's change version, so a fill
// of X below that version is refused even though X itself never changed. A
// bucket raise that misses the table, or a bucket index that ignores the ID,
// leaves the fill through.
func TestDeleteInTheSameBucketRefuses(t *testing.T) {
	var fs files
	x := idInBucket(0)
	z := idInBucket(0, x)
	asOf := fs.asOf(x)

	change(t, &fs, z, func(f *file) error {
		f.write(write(0, 4, rec(1, 0), 7))
		f.delete(ver(8))
		return nil
	})
	fs.settleRemovals(z, ver(8))
	if err := fs.fill(x, write(0, 4, rec(1, 0), 0), asOf); !errors.Is(err, core.ErrStaleFill) {
		t.Fatalf("fill below the bucket's version = %v, want ErrStaleFill", err)
	}
	if err := fs.fill(x, write(0, 4, rec(1, 0), 0), ver(8)); err != nil {
		t.Fatalf("fill at the bucket's version = %v, want accepted", err)
	}
}

// TestStaleFill is the "stale Fill" row: a fill read before a change that
// dropped the change version is refused whatever the change was. The four
// cases are a release that takes the file to nothing (the entry drops, the
// bucket carries the version), the same beside another held extent (the entry
// stays, its own change version carries it), a truncate down and up with the
// markers still unsettled, and the same after the markers are settled so the
// entry drops — the case only the bucket covers.
func TestStaleFill(t *testing.T) {
	// a. Released to nothing: the entry drops, and the fill is refused against
	// the bucket the release's removal raised before the drop.
	t.Run("released to nothing", func(t *testing.T) {
		var fs files
		id := fid(1)
		asOf := fs.asOf(id)
		change(t, &fs, id, func(f *file) error { f.write(write(0, 8, rec(1, 0), 5)); return nil })
		change(t, &fs, id, func(f *file) error {
			f.held.markOffloaded(ext(0, 8), ver(5), ver(5))
			_, err := f.held.release(ext(0, 8), nil)
			return err
		})
		if present(&fs, id) {
			t.Fatal("entry kept after everything the file held was released")
		}
		if err := fs.fill(id, write(0, 8, rec(2, 0), 0), asOf); !errors.Is(err, core.ErrStaleFill) {
			t.Fatalf("stale fill after release = %v, want ErrStaleFill", err)
		}
		if ps := heldPieces(&fs, id); len(ps) != 0 {
			t.Fatalf("held after the refused fill = %+v, want nothing", ps)
		}
		if present(&fs, id) {
			t.Fatal("a refused fill on a file with no entry left one")
		}
	})

	// b. Released in part: the file kept another extent, so its entry and its
	// own change version carry the refusal.
	t.Run("released in part", func(t *testing.T) {
		var fs files
		id := fid(1)
		asOf := fs.asOf(id)
		change(t, &fs, id, func(f *file) error {
			f.write(write(0, 4, rec(1, 0), 5))
			f.write(write(8, 4, rec(1, 64), 6))
			return nil
		})
		change(t, &fs, id, func(f *file) error {
			f.held.markOffloaded(ext(0, 4), ver(5), ver(5))
			_, err := f.held.release(ext(0, 4), nil)
			return err
		})
		if !present(&fs, id) {
			t.Fatal("entry dropped while the file still holds an extent")
		}
		if err := fs.fill(id, write(0, 4, rec(2, 0), 0), asOf); !errors.Is(err, core.ErrStaleFill) {
			t.Fatalf("stale fill beside a held extent = %v, want ErrStaleFill", err)
		}
		want := []Piece{{Extent: ext(8, 4), Record: rec(1, 64), Version: ver(6)}}
		if got := heldPieces(&fs, id); !slices.Equal(got, want) {
			t.Fatalf("held = %+v, want the surviving extent", got)
		}
	})

	// c. Truncated down and up, markers unsettled: the truncate up removed
	// nothing but changed the version, so the fill is refused by the entry's
	// change version, and masked by the markers besides.
	t.Run("truncated down and up, unsettled", func(t *testing.T) {
		var fs files
		id := fid(1)
		asOf := fs.asOf(id)
		change(t, &fs, id, func(f *file) error {
			f.truncate(0, ver(1))
			f.truncate(16, ver(2))
			return nil
		})
		if err := fs.fill(id, write(4, 8, rec(1, 0), 0), asOf); !errors.Is(err, core.ErrStaleFill) {
			t.Fatalf("stale fill under unsettled truncates = %v, want ErrStaleFill", err)
		}
		if ps := heldPieces(&fs, id); len(ps) != 0 {
			t.Fatalf("held after the refused fill = %+v, want nothing", ps)
		}
	})

	// d. The same, settled: the entry drops, and the fill is still refused,
	// against the bucket the settled removal raised before the drop.
	t.Run("truncated down and up, settled", func(t *testing.T) {
		var fs files
		id := fid(1)
		asOf := fs.asOf(id)
		change(t, &fs, id, func(f *file) error {
			f.truncate(0, ver(1))
			f.truncate(16, ver(2))
			return nil
		})
		fs.settleRemovals(id, ver(2))
		if present(&fs, id) {
			t.Fatal("entry kept after its markers were settled with nothing held")
		}
		if err := fs.fill(id, write(4, 8, rec(1, 0), 0), asOf); !errors.Is(err, core.ErrStaleFill) {
			t.Fatalf("stale fill after settling = %v, want ErrStaleFill", err)
		}
		if ps := heldPieces(&fs, id); len(ps) != 0 {
			t.Fatalf("held after the refused fill = %+v, want nothing", ps)
		}
	})
}

// TestFillAtOrAboveTheChangeVersionIsAccepted: an equal asOf is accepted, and
// so is a newer one, with an entry to the file's own change version and
// without one, checked against the bucket. A check that refused at the
// version rather than above it would needlessly fetch again.
func TestFillAtOrAboveTheChangeVersionIsAccepted(t *testing.T) {
	// With an entry: the check runs against the entry's change version.
	t.Run("with an entry", func(t *testing.T) {
		var fs files
		id := fid(1)
		change(t, &fs, id, func(f *file) error { f.write(write(0, 4, rec(1, 0), 5)); return nil })
		if err := fs.fill(id, write(4, 4, rec(1, 64), 5), ver(5)); err != nil {
			t.Fatalf("fill at the change version = %v, want accepted", err)
		}
		if err := fs.fill(id, write(8, 4, rec(1, 128), 6), ver(6)); err != nil {
			t.Fatalf("fill above the change version = %v, want accepted", err)
		}
	})

	// Without an entry: the check runs against the bucket the settled delete
	// raised. The version is the delete's, above the write's, so the write is
	// gone from the set and the entry drops.
	t.Run("with no entry", func(t *testing.T) {
		var fs files
		id := fid(1)
		change(t, &fs, id, func(f *file) error {
			f.write(write(0, 4, rec(1, 0), 5))
			f.delete(ver(6))
			return nil
		})
		fs.settleRemovals(id, ver(6))
		if present(&fs, id) {
			t.Fatal("entry kept after its delete was settled")
		}
		if err := fs.fill(id, write(0, 4, rec(2, 0), 6), ver(6)); err != nil {
			t.Fatalf("fill at the bucket's version = %v, want accepted", err)
		}
		if err := fs.fill(id, write(4, 4, rec(2, 64), 7), ver(7)); err != nil {
			t.Fatalf("fill above the bucket's version = %v, want accepted", err)
		}
	})
}

// TestWhatRaisesTheChangeVersion is R33: a write and each of the four removals
// raise the file's change version to their own; a release of part of the file
// and a fill at a higher version do not. A change that did not raise the
// version would let a fill resolve against a snapshot taken before it.
func TestWhatRaisesTheChangeVersion(t *testing.T) {
	raises := []struct {
		name  string
		apply func(f *file, v core.Version) error
	}{
		{"a write", func(f *file, v core.Version) error {
			f.write(write(0, 4, rec(1, 0), v.Counter))
			return nil
		}},
		{"a truncate", func(f *file, v core.Version) error { f.truncate(4, v); return nil }},
		{"a deallocate", func(f *file, v core.Version) error { return f.deallocate(ext(4, 8), v) }},
		{"a delete", func(f *file, v core.Version) error { f.delete(v); return nil }},
		{"a clone target", func(f *file, v core.Version) error {
			return f.cloneTarget(core.CloneSpec{Src: fid(9), SrcOff: 100, DstOff: 4, Len: 8, AsOf: ver(1)}, v)
		}},
	}
	for i, r := range raises {
		t.Run(r.name, func(t *testing.T) {
			var fs files
			id := fid(1)
			v := ver(uint64(2 + i))
			change(t, &fs, id, func(f *file) error { return r.apply(f, v) })
			if got := fs.asOf(id); got != v {
				t.Fatalf("asOf after %s at %v = %v, want %v", r.name, v, got, v)
			}
		})
	}

	// A release of part of the file does not: a Release carries a version
	// already counted, the version of the write or removal that put the
	// content there.
	t.Run("a release does not", func(t *testing.T) {
		var fs files
		id := fid(1)
		change(t, &fs, id, func(f *file) error {
			f.write(write(0, 4, rec(1, 0), 5))
			f.write(write(8, 4, rec(1, 64), 6))
			f.held.markOffloaded(ext(0, 4), ver(5), ver(5))
			_, err := f.held.release(ext(0, 4), nil)
			return err
		})
		if got := fs.asOf(id); got != ver(6) {
			t.Fatalf("asOf after a release = %v, want 6, the write's version", got)
		}
	})

	// A fill at a higher version does not either: a fill changes no version,
	// so the version it carries is the content's own age.
	t.Run("a fill does not", func(t *testing.T) {
		var fs files
		id := fid(1)
		change(t, &fs, id, func(f *file) error {
			f.write(write(0, 4, rec(1, 0), 5))
			f.fill(write(8, 4, rec(2, 0), 40))
			return nil
		})
		if got := fs.asOf(id); got != ver(5) {
			t.Fatalf("asOf after a fill at v40 = %v, want 5, the write's version", got)
		}
	})
}

// TestDropRaisesTheBucketToTheEntryVersion is R35: dropping an entry raises
// the file's bucket to the entry's change version, before the entry can be
// found missing, and never lowers it. A file in its tag-only life reports
// its bucket, and so does one whose entry holds only filled content, whose
// change version is the zero Version and would otherwise answer a read begun
// before a delete.
func TestDropRaisesTheBucketToTheEntryVersion(t *testing.T) {
	var fs files
	a := idInBucket(0)
	b := idInBucket(0, a)

	// A drop of an entry at change version 7 raises the bucket to 7.
	change(t, &fs, a, func(f *file) error {
		f.write(write(0, 4, rec(1, 0), 6))
		f.delete(ver(7))
		return nil
	})
	fs.settleRemovals(a, ver(7))
	if got := fs.buckets[bucketOf(a)].Load(); got != 7 {
		t.Fatalf("bucket after a drop at v7 = %d, want 7", got)
	}
	if got := fs.asOf(b); got != ver(7) {
		t.Fatalf("asOf of a no-entry file in the bucket = %v, want 7", got)
	}

	// A later drop at v3 in the same bucket leaves the bucket at 7.
	change(t, &fs, b, func(f *file) error {
		f.write(write(0, 4, rec(2, 0), 2))
		f.delete(ver(3))
		return nil
	})
	fs.settleRemovals(b, ver(3))
	if got := fs.buckets[bucketOf(a)].Load(); got != 7 {
		t.Fatalf("bucket after a later drop at v3 = %d, want 7 still", got)
	}

	// A file in its tag-only life — every extent released, a segment still
	// holding a record of it — kept its entry through the drop of its change
	// version, and reports its bucket, since its entry's change is the zero
	// Version.
	c := idInBucket(0, a, b)
	change(t, &fs, c, func(f *file) error {
		_ = f.recorded(9, 1, c)
		f.write(write(0, 4, rec(9, 0), 2))
		f.held.markOffloaded(ext(0, 4), ver(2), ver(2))
		_, err := f.held.release(ext(0, 4), nil)
		return err
	})
	if !present(&fs, c) {
		t.Fatal("entry dropped while a segment holds a record of the file")
	}
	if got := fs.asOf(c); got != ver(7) {
		t.Fatalf("asOf in the tag-only life = %v, want the bucket's 7", got)
	}

	// A file whose entry holds only filled content after its own delete and
	// drop at v is checked against the bucket, so a fill with an asOf below v
	// — a fetch that began before the delete — is refused, and one at v
	// reports v. Reporting the entry's zero change version there would let
	// the first through.
	d := idInBucket(0, a, b, c)
	change(t, &fs, d, func(f *file) error {
		f.write(write(0, 4, rec(3, 0), 12))
		f.delete(ver(13))
		return nil
	})
	fs.settleRemovals(d, ver(13))
	if got := fs.buckets[bucketOf(d)].Load(); got != 13 {
		t.Fatalf("bucket after a drop at v13 = %d, want 13", got)
	}
	if err := fs.fill(d, write(0, 4, rec(4, 0), 13), ver(13)); err != nil {
		t.Fatalf("fill at the bucket's version = %v, want accepted", err)
	}
	if !present(&fs, d) {
		t.Fatal("the accepted fill left no entry")
	}
	if got := fs.asOf(d); got != ver(13) {
		t.Fatalf("asOf of an entry holding only filled content = %v, want the bucket's 13", got)
	}
	if err := fs.fill(d, write(4, 4, rec(5, 0), 0), ver(11)); !errors.Is(err, core.ErrStaleFill) {
		t.Fatalf("fill below the bucket's version against a filled change-zero entry = %v, want ErrStaleFill", err)
	}
}

// TestOpenStartsEveryBucket: openBuckets starts every bucket at the version
// every entry not changed since is checked against, so no read that began
// before the open can fill, and every version a rebuild applies below it
// leaves the file checked against it.
func TestOpenStartsEveryBucket(t *testing.T) {
	var fs files
	fs.openBuckets(ver(10))

	if got := fs.buckets[0].Load(); got != 10 {
		t.Fatalf("bucket 0 = %d, want 10", got)
	}
	if got := fs.buckets[floorBuckets-1].Load(); got != 10 {
		t.Fatalf("bucket %d = %d, want 10", floorBuckets-1, got)
	}
	if got := fs.buckets[bucketOf(fid(7))].Load(); got != 10 {
		t.Fatalf("fid(7)'s bucket = %d, want 10", got)
	}

	// A file with no entry reports the start.
	id := idInBucket(0)
	if got := fs.asOf(id); got != ver(10) {
		t.Fatalf("asOf of a no-entry file = %v, want 10", got)
	}

	// A file written at v5 after the open-start, as a rebuild would, is still
	// checked against the start until it changes above it.
	change(t, &fs, id, func(f *file) error { f.write(write(0, 4, rec(1, 0), 5)); return nil })
	if got := fs.asOf(id); got != ver(10) {
		t.Fatalf("asOf of an entry written at v5 since open = %v, want 10", got)
	}
	if err := fs.fill(id, write(4, 4, rec(1, 64), 5), ver(5)); !errors.Is(err, core.ErrStaleFill) {
		t.Fatalf("fill below the open's start = %v, want ErrStaleFill", err)
	}
	if err := fs.fill(id, write(4, 4, rec(1, 64), 5), ver(10)); err != nil {
		t.Fatalf("fill at the open's start = %v, want accepted", err)
	}

	// Once the file changes above the start, it is checked against its own
	// change version, and the bucket is left where openBuckets put it.
	change(t, &fs, id, func(f *file) error { f.write(write(8, 4, rec(1, 128), 11)); return nil })
	if got := fs.asOf(id); got != ver(11) {
		t.Fatalf("asOf after a change at v11 = %v, want 11", got)
	}
	if got := fs.buckets[bucketOf(id)].Load(); got != 10 {
		t.Fatalf("bucket after the file's own change = %d, want the open's 10", got)
	}
}

// TestRefusedFillLeavesNoEntry: a refused fill on a file with no entry must
// not leave one behind, since the fill wrote nothing. An entry left holding
// nothing would pin the fill's version against a later check that should use
// the bucket.
func TestRefusedFillLeavesNoEntry(t *testing.T) {
	var fs files
	id := fid(1)
	// Give the file's bucket a version above the fill's asOf through another
	// file's drop, without creating id's entry.
	other := idInBucket(bucketOf(id), id)
	change(t, &fs, other, func(f *file) error {
		f.write(write(0, 4, rec(1, 0), 9))
		f.delete(ver(10))
		return nil
	})
	fs.settleRemovals(other, ver(10))
	if present(&fs, id) {
		t.Fatal("setup left the tested file an entry")
	}
	if err := fs.fill(id, write(0, 4, rec(1, 0), 0), ver(1)); !errors.Is(err, core.ErrStaleFill) {
		t.Fatalf("refused fill = %v, want ErrStaleFill", err)
	}
	if present(&fs, id) || fs.len() != 0 {
		t.Fatal("a refused fill on a file with no entry left an entry")
	}
}

// TestBucketsSpreadIDs: the bucket index takes the top twelve bits of the
// full-ID hash, an exclusive-or of the ID's two little-endian halves, so its
// bits come from the top two bytes of each half — bytes 6 and 7, and 14 and
// 15. Two families of 4,096 IDs, one varying only bytes 14 and 15 and one
// varying only bytes 6 and 7, must each spread over the buckets, encoded so
// the twelve index bits carry the whole counter. A bucket taken from bits
// only one half reaches — or read at the wrong end of the bytes — clumps one
// family.
func TestBucketsSpreadIDs(t *testing.T) {
	spread := func(makeID func(i int) core.FileID) bool {
		var hit [floorBuckets]bool
		for i := range floorBuckets {
			hit[bucketOf(makeID(i))] = true
		}
		n := 0
		for _, h := range hit {
			if h {
				n++
			}
		}
		return n >= 2400 // random would reach about 2,590
	}
	if !spread(func(i int) core.FileID {
		return core.NewFileID([16]byte{14: byte(i&15) << 4, 15: byte(i >> 4)})
	}) {
		t.Fatal("IDs differing only in the second half's top two bytes do not spread over the buckets")
	}
	if !spread(func(i int) core.FileID {
		return core.NewFileID([16]byte{6: byte(i&15) << 4, 7: byte(i >> 4)})
	}) {
		t.Fatal("IDs differing only in the first half's top two bytes do not spread over the buckets")
	}
}

// TestBucketRaiseRacesReads: drops of files sharing one bucket, at distinct
// versions and from many goroutines, run beside readers calling asOf and
// taking or refusing fills of a no-entry file in the bucket. Whatever
// interleaving the readers see, the bucket ends at the highest dropped
// version: a raise that could lose one would let a read begun after that
// drop still fill. The mid-run interleaving is checked by the race detector
// alone — an accepted fill might land just before a raise, by design — and
// the end state, where the writers have finished, is asserted here.
func TestBucketRaiseRacesReads(t *testing.T) {
	var fs files
	const bucket, drops = 17, 64
	victim := idInBucket(bucket)

	not := []core.FileID{victim}
	ids := make([]core.FileID, drops)
	for i := range ids {
		ids[i] = idInBucket(bucket, not...)
		not = append(not, ids[i])
	}
	var dropping sync.WaitGroup
	for i := range ids {
		dropping.Add(1)
		go func() {
			defer dropping.Done()
			_ = fs.update(ids[i], func(f *file) error {
				f.write(write(0, 4, rec(1, 0), uint64(2*i+1)))
				f.delete(ver(uint64(2*i + 2)))
				return nil
			})
			fs.settleRemovals(ids[i], ver(uint64(2*i+2)))
		}()
	}
	var reading sync.WaitGroup
	for range 8 {
		reading.Add(1)
		go func() {
			defer reading.Done()
			for range 100 {
				_ = fs.asOf(victim)
				_ = fs.fill(victim, write(0, 4, rec(2, 0), 2), ver(66))
			}
		}()
	}
	reading.Wait()
	dropping.Wait()

	const highest = 2*(drops-1) + 2
	if got := fs.buckets[bucket].Load(); got != highest {
		t.Fatalf("bucket after %d drops = %d, want %d: a raise was lost", drops, got, highest)
	}
	if err := fs.fill(victim, write(0, 4, rec(3, 0), 3), ver(highest-1)); !errors.Is(err, core.ErrStaleFill) {
		t.Fatalf("fill below the bucket after every drop = %v, want ErrStaleFill", err)
	}
	if err := fs.fill(victim, write(0, 4, rec(3, 0), 3), ver(highest)); err != nil {
		t.Fatalf("fill at the bucket after every drop = %v, want accepted", err)
	}
}

// TestAsOfAllocatesNothing: asOf runs on every read returning a placement, so
// it must not allocate, for an entry with a change version and for one
// without.
func TestAsOfAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not meaningful under -race")
	}
	var fs files
	id := fid(1)
	change(t, &fs, id, func(f *file) error { f.write(write(0, 4, rec(1, 0), 5)); return nil })
	missing := fid(2)
	if allocs := testing.AllocsPerRun(1000, func() { _ = fs.asOf(id) }); allocs != 0 {
		t.Fatalf("asOf of an entry allocated %v times per call, want 0", allocs)
	}
	if allocs := testing.AllocsPerRun(1000, func() { _ = fs.asOf(missing) }); allocs != 0 {
		t.Fatalf("asOf of a missing file allocated %v times per call, want 0", allocs)
	}
}
