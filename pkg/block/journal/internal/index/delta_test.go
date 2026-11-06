package index

import (
	"errors"
	"reflect"
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

// fileTotals is what a read-out walk of one file says about its held, dirty
// and retained bytes, and whether it is on the dirty-file list: the figures a
// mutator's delta must equal the change of.
type fileTotals struct {
	held, dirty, retained int64
	onDirtyList           bool
}

func walkTotals(fs *files, id core.FileID) fileTotals {
	var t fileTotals
	for _, e := range fs.inspect(id) {
		switch e.Kind {
		case EntryHeld:
			t.held += e.Extent.Len
			if !e.Offloaded {
				t.dirty += e.Extent.Len
				t.onDirtyList = true
			}
		case EntryRetained:
			t.retained += e.Extent.Len
			if !e.Offloaded {
				t.onDirtyList = true
			}
		}
	}
	return t
}

// walkDelta is the delta the read-out walk says a call made.
func walkDelta(tag core.ShareTag, id core.FileID, before, after fileTotals) HeldDelta {
	d := HeldDelta{
		HeldBytes:     after.held - before.held,
		DirtyBytes:    after.dirty - before.dirty,
		RetainedBytes: after.retained - before.retained,
	}
	switch {
	case after.onDirtyList && !before.onDirtyList:
		d.Dirty = BecameDirty
	case !after.onDirtyList && before.onDirtyList:
		d.Dirty = BecameClean
	}
	if d != (HeldDelta{}) {
		d.Tag, d.File = tag, id
	}
	return d
}

// TestMutatorDeltas: one row per mutator and outcome. Each row's delta is
// checked against its expected value and against the change a read-out walk
// of the file saw across the call, and a refusal must also leave the read-out
// as it was.
func TestMutatorDeltas(t *testing.T) {
	const owner, rival core.ShareTag = 7, 8
	var seq uint64
	// through runs fn on the file inside one update, after noting a record
	// under tag first, as every journal call does.
	through := func(tag core.ShareTag, fn func(*file) error) func(*files, core.FileID) (HeldDelta, error) {
		return func(fs *files, id core.FileID) (HeldDelta, error) {
			return fs.update(id, func(f *file) error {
				seq++
				if err := f.recorded(core.SegmentID(90), ext(0, 1), Numbers{Version: ver(90), Seq: core.Seq(seq)}, tag, id); err != nil {
					return err
				}
				return fn(f)
			})
		}
	}
	ok := func(f func(*file)) func(*file) error { return func(x *file) error { f(x); return nil } }
	isOther := func(err error) bool {
		var o *core.OtherShareError
		return errors.As(err, &o)
	}
	is := func(target error) func(error) bool { return func(err error) bool { return errors.Is(err, target) } }
	dirty4k := func(f *file) { f.write(write(0, 4096, rec(1, 0), 1)) }
	offloaded4k := func(f *file) { f.fill(write(0, 4096, rec(1, 0), 1)) }
	// retainedUnderOffloaded leaves the file's held content offloaded and one
	// dirty retained piece of it, superseded by version 2.
	retainedUnderOffloaded := func(f *file) {
		f.write(write(0, 4096, rec(1, 0), 1))
		f.writeRetain(write(0, 4096, rec(2, 0), 2))
		f.held.markOffloaded(ext(0, 4096), ver(2), ver(2))
	}
	// extendedWindow leaves a write that grew the file and superseded dirty
	// content, as a window to fail.
	extendedWindow := func(f *file) {
		f.write(write(0, 16, rec(1, 0), 1))
		f.writeRetain(write(8, 16, rec(3, 0), 3))
	}
	zero := HeldDelta{}
	at := func(d HeldDelta) HeldDelta { d.Tag = owner; return d }

	rows := []struct {
		name    string
		setup   func(*file)
		run     func(*files, core.FileID) (HeldDelta, error)
		want    HeldDelta
		wantErr func(error) bool // nil: no error
	}{
		{"write/hit", nil, through(owner, ok(func(f *file) { f.write(write(0, 4096, rec(2, 0), 10)) })),
			at(HeldDelta{HeldBytes: 4096, DirtyBytes: 4096, Dirty: BecameDirty}), nil},
		{"write/miss: over dirty content of the same extent", dirty4k, through(owner, ok(func(f *file) { f.write(write(0, 4096, rec(2, 0), 10)) })), zero, nil},
		{"write/refusal", dirty4k, through(rival, ok(func(f *file) { f.write(write(0, 8192, rec(2, 0), 10)) })), zero, isOther},
		{"write/over offloaded content", offloaded4k, through(owner, ok(func(f *file) { f.write(write(0, 4096, rec(2, 0), 10)) })),
			at(HeldDelta{DirtyBytes: 4096, Dirty: BecameDirty}), nil},

		{"writeRetain/hit: over dirty content", dirty4k, through(owner, ok(func(f *file) { f.writeRetain(write(0, 4096, rec(2, 0), 10)) })),
			at(HeldDelta{RetainedBytes: 4096}), nil},
		{"writeRetain/miss: zero length", dirty4k, through(owner, ok(func(f *file) { f.writeRetain(write(0, 0, rec(2, 0), 10)) })), zero, nil},
		{"writeRetain/refusal", dirty4k, through(rival, ok(func(f *file) { f.writeRetain(write(0, 4096, rec(2, 0), 10)) })), zero, isOther},

		{"fill/hit: a gap", dirty4k, func(fs *files, id core.FileID) (HeldDelta, error) {
			return fs.fill(id, write(4096, 4096, rec(2, 0), 5), ver(1))
		}, at(HeldDelta{HeldBytes: 4096}), nil},
		{"fill/miss: under held content", dirty4k, func(fs *files, id core.FileID) (HeldDelta, error) {
			return fs.fill(id, write(0, 4096, rec(2, 0), 0), ver(1))
		}, zero, nil},
		{"fill/refusal: stale", dirty4k, func(fs *files, id core.FileID) (HeldDelta, error) {
			return fs.fill(id, write(4096, 4096, rec(2, 0), 5), ver(0))
		}, zero, is(core.ErrStaleFill)},

		{"truncate/hit: all the dirty content", dirty4k, through(owner, ok(func(f *file) { f.truncate(0, ver(10)) })),
			at(HeldDelta{HeldBytes: -4096, DirtyBytes: -4096, Dirty: BecameClean}), nil},
		{"truncate/hit: part of it", func(f *file) { f.write(write(0, 8192, rec(1, 0), 1)) }, through(owner, ok(func(f *file) { f.truncate(4096, ver(10)) })),
			at(HeldDelta{HeldBytes: -4096, DirtyBytes: -4096}), nil},
		{"truncate/miss: beyond the end", dirty4k, through(owner, ok(func(f *file) { f.truncate(8192, ver(10)) })), zero, nil},
		{"truncate/refusal", dirty4k, through(rival, ok(func(f *file) { f.truncate(0, ver(10)) })), zero, isOther},

		{"deallocate/hit", dirty4k, through(owner, func(f *file) error { return f.deallocate(ext(0, 4096), ver(10)) }),
			at(HeldDelta{HeldBytes: -4096, DirtyBytes: -4096, Dirty: BecameClean}), nil},
		{"deallocate/miss: over a gap", dirty4k, through(owner, func(f *file) error { return f.deallocate(ext(8192, 4096), ver(10)) }), zero, nil},
		{"deallocate/refusal: zero length", dirty4k, through(owner, func(f *file) error { return f.deallocate(ext(0, 0), ver(10)) }), zero, is(core.ErrZeroLength)},

		{"delete/hit", dirty4k, through(owner, ok(func(f *file) { f.delete(ver(10)) })),
			at(HeldDelta{HeldBytes: -4096, DirtyBytes: -4096, Dirty: BecameClean}), nil},
		{"delete/miss: a file holding nothing", nil, through(owner, ok(func(f *file) { f.delete(ver(10)) })), zero, nil},
		{"delete/refusal", dirty4k, through(rival, ok(func(f *file) { f.delete(ver(10)) })), zero, isOther},

		{"cloneTarget/hit", dirty4k, through(owner, func(f *file) error { return f.cloneTarget(core.CloneSpec{DstOff: 0, Len: 4096}, ver(10)) }),
			at(HeldDelta{HeldBytes: -4096, DirtyBytes: -4096, Dirty: BecameClean}), nil},
		{"cloneTarget/miss", dirty4k, through(owner, func(f *file) error { return f.cloneTarget(core.CloneSpec{DstOff: 8192, Len: 4096}, ver(10)) }), zero, nil},
		{"cloneTarget/refusal: zero length", dirty4k, through(owner, func(f *file) error { return f.cloneTarget(core.CloneSpec{DstOff: 0, Len: 0}, ver(10)) }), zero, is(core.ErrZeroLength)},

		{"release/hit: offloaded content", offloaded4k, through(owner, func(f *file) error { _, err := f.held.release(ext(0, 4096), nil); return err }),
			at(HeldDelta{HeldBytes: -4096}), nil},
		{"release/miss: a gap", offloaded4k, through(owner, func(f *file) error { _, err := f.held.release(ext(8192, 4096), nil); return err }), zero, nil},
		{"release/refusal: dirty content", dirty4k, through(owner, func(f *file) error { _, err := f.held.release(ext(0, 4096), nil); return err }), zero, is(ErrNotOffloaded)},

		{"markOffloaded/hit: the last dirty bytes", dirty4k, through(owner, ok(func(f *file) { f.held.markOffloaded(ext(0, 4096), ver(1), ver(1)) })),
			at(HeldDelta{DirtyBytes: -4096, Dirty: BecameClean}), nil},
		{"markOffloaded/miss: versions matching nothing", dirty4k, through(owner, ok(func(f *file) { f.held.markOffloaded(ext(0, 4096), ver(5), ver(5)) })), zero, nil},
		{"markOffloaded/refusal", dirty4k, through(rival, ok(func(f *file) { f.held.markOffloaded(ext(0, 4096), ver(1), ver(1)) })), zero, isOther},

		{"unmark/hit", offloaded4k, through(owner, ok(func(f *file) { f.held.unmark(ext(0, 4096), ver(1)) })),
			at(HeldDelta{DirtyBytes: 4096, Dirty: BecameDirty}), nil},
		{"unmark/miss: a gap", offloaded4k, through(owner, ok(func(f *file) { f.held.unmark(ext(8192, 4096), ver(1)) })), zero, nil},
		{"unmark/refusal", offloaded4k, through(rival, ok(func(f *file) { f.held.unmark(ext(0, 4096), ver(1)) })), zero, isOther},

		{"retained lift/hit: the only dirty retained piece", retainedUnderOffloaded, through(owner, ok(func(f *file) { f.retained.drop(whole, bySuperseder(ver(2)), nil) })),
			at(HeldDelta{RetainedBytes: -4096, Dirty: BecameClean}), nil},
		{"retained lift/miss: a version that superseded nothing", retainedUnderOffloaded, through(owner, ok(func(f *file) { f.retained.drop(whole, bySuperseder(ver(9)), nil) })), zero, nil},
		{"retained lift/refusal", retainedUnderOffloaded, through(rival, ok(func(f *file) { f.retained.drop(whole, bySuperseder(ver(2)), nil) })), zero, isOther},

		{"failWindow/hit: a write that superseded dirty content", extendedWindow, through(owner, ok(func(f *file) { f.failWindow([]WindowRecord{failed(rec(3, 0), 3)}, nil) })),
			at(HeldDelta{HeldBytes: -8, DirtyBytes: -8, RetainedBytes: -8}), nil},
		{"failWindow/miss: a record the file does not hold", extendedWindow, through(owner, ok(func(f *file) { f.failWindow([]WindowRecord{failed(rec(9, 0), 9)}, nil) })), zero, nil},
		{"failWindow/refusal", extendedWindow, through(rival, ok(func(f *file) { f.failWindow([]WindowRecord{failed(rec(3, 0), 3)}, nil) })), zero, isOther},

		{"repoint/held: moves pieces, no bytes", dirty4k, through(owner, ok(func(f *file) { f.held.repoint(ext(0, 4096), rec(1, 0), rec(9, 0), 0) })), zero, nil},
		{"repoint/retained: moves pieces, no bytes", retainedUnderOffloaded, through(owner, ok(func(f *file) { f.retained.repoint(ext(0, 4096), rec(1, 0), rec(9, 0), 0) })), zero, nil},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			var fs files
			id := fid(1)
			if _, err := fs.commit(id, 0, func(f *file) error {
				seq++
				if err := f.recorded(core.SegmentID(80), ext(0, 1), Numbers{Version: ver(80), Seq: core.Seq(seq)}, owner, id); err != nil {
					return err
				}
				if row.setup != nil {
					row.setup(f)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			beforeEntries, beforeTotals := fs.inspect(id), walkTotals(&fs, id)

			got, err := row.run(&fs, id)

			if (row.wantErr == nil) != (err == nil) || (err != nil && !row.wantErr(err)) {
				t.Fatalf("error = %v", err)
			}
			want := row.want
			if want != zero {
				want.File = id
			}
			if got != want {
				t.Fatalf("delta = %+v, want %+v", got, want)
			}
			afterTotals := walkTotals(&fs, id)
			if w := walkDelta(owner, id, beforeTotals, afterTotals); got != w {
				t.Fatalf("delta = %+v, the read-out walk says the call changed %+v", got, w)
			}
			if err != nil && !reflect.DeepEqual(fs.inspect(id), beforeEntries) {
				t.Fatalf("a refused call changed the file:\n before %+v\n after  %+v", beforeEntries, fs.inspect(id))
			}
		})
	}
}
