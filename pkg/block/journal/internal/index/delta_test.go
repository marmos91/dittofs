package index

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// heldWalk returns the sum of the held lengths and of those whose offloaded
// bit is unset, read from the pieces rather than from the set's counters.
func heldWalk(s *heldSet) (bytes, dirty int64) {
	for _, p := range s.pieces(nil) {
		bytes += p.Extent.Len
		if !p.Offloaded {
			dirty += p.Extent.Len
		}
	}
	return bytes, dirty
}

// retainedWalk is heldWalk for the retained set.
func retainedWalk(s *retainedSet) (bytes, dirty int64) {
	for _, r := range s.list(nil) {
		bytes += r.Extent.Len
		if !r.Offloaded {
			dirty += r.Extent.Len
		}
	}
	return bytes, dirty
}

func TestHeldSetCountersFollowPutAndCut(t *testing.T) {
	var s heldSet // unaccounted: the counters must not depend on acct
	check := func(step string) {
		t.Helper()
		wb, wd := heldWalk(&s)
		if s.bytes != wb || s.dirty != wd {
			t.Fatalf("%s: counters bytes=%d dirty=%d, walk bytes=%d dirty=%d", step, s.bytes, s.dirty, wb, wd)
		}
	}
	s.put(Piece{Extent: ext(0, 100), Record: rec(1, 0), Version: ver(1)})
	check("dirty put")
	s.fill(write(100, 50, rec(2, 0), 2)) // a fill holds offloaded content
	if b, d := heldWalk(&s); b != 150 || d != 100 {
		t.Fatalf("setup: walk bytes=%d dirty=%d, want 150 and 100", b, d)
	}
	check("fill beside it")
	s.cut(ext(80, 40)) // across the dirty piece's tail and the offloaded head
	check("cut across both")
	s.markOffloaded(ext(10, 30), ver(1), ver(1))
	check("mark part")
	s.unmark(ext(20, 10), ver(1))
	check("unmark part")
	s.put(Piece{Extent: ext(5, 200), Record: rec(3, 0), Version: ver(3)})
	check("put over everything")
}

func TestRetainedSetCountersFollowAddAndEdit(t *testing.T) {
	var s retainedSet
	check := func(step string) {
		t.Helper()
		wb, wd := retainedWalk(&s)
		if s.bytes != wb || s.dirty != wd {
			t.Fatalf("%s: counters bytes=%d dirty=%d, walk bytes=%d dirty=%d", step, s.bytes, s.dirty, wb, wd)
		}
	}
	s.add(Retained{Piece: Piece{Extent: ext(0, 100), Record: rec(1, 0), Version: ver(1)}, By: ver(5)})
	s.add(Retained{Piece: Piece{Extent: ext(50, 100), Record: rec(2, 0), Version: ver(2), Offloaded: true}, By: ver(6)})
	s.add(Retained{Piece: Piece{Extent: ext(0, 0), Record: rec(3, 0), Version: ver(3)}, By: ver(6)}) // empty: not kept
	check("adds")
	if s.bytes != 200 || s.dirty != 100 {
		t.Fatalf("after adds bytes=%d dirty=%d, want 200 and 100", s.bytes, s.dirty)
	}
	before := s
	s.repoint(ext(20, 30), rec(1, 0), rec(9, 0), 0)
	check("repoint")
	if s.bytes != before.bytes || s.dirty != before.dirty {
		t.Fatalf("repoint moved the counters: %d/%d -> %d/%d", before.bytes, before.dirty, s.bytes, s.dirty)
	}
	s.drop(ext(40, 70), func(Retained) bool { return true }, nil) // splits both at both ends
	check("drop a middle part")
	s.drop(whole, bySuperseder(ver(6)), nil)
	check("drop by superseder")
}

func TestUpdateReturnsZeroDeltaWhenNothingChanges(t *testing.T) {
	var fs files
	id := fid(1)
	d, err := fs.update(id, func(*file) error { return nil })
	if err != nil || d != (HeldDelta{}) {
		t.Fatalf("empty update = %+v, %v", d, err)
	}
	d, err = fs.update(id, func(f *file) error {
		return f.recorded(core.SegmentID(1), ext(0, 10), Numbers{Version: ver(1), Seq: 1}, 7, id)
	})
	if err != nil || d != (HeldDelta{}) {
		t.Fatalf("update that only records = %+v, %v", d, err)
	}
}

func TestUpdateDeltaCarriesFileAndOwner(t *testing.T) {
	var fs files
	id := fid(1)
	d, err := fs.commit(id, 0, func(f *file) error {
		if err := f.recorded(core.SegmentID(1), ext(0, 4096), Numbers{Version: ver(1), Seq: 1}, 7, id); err != nil {
			return err
		}
		f.write(write(0, 4096, rec(1, 0), 1))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := HeldDelta{Tag: 7, File: id, HeldBytes: 4096, DirtyBytes: 4096, Dirty: BecameDirty}
	if d != want {
		t.Fatalf("delta = %+v, want %+v", d, want)
	}
}
