package index

import (
	"fmt"
	"math"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// fmtEntries prints one entry per line with every field, its Clone
// dereferenced, so two differing read-outs name the entry and the field that
// differ.
func fmtEntries(es []Entry) string {
	out := "\n"
	for _, e := range es {
		kind := map[EntryKind]string{EntryHeld: "held", EntryRetained: "retained", EntryMarker: "marker"}[e.Kind]
		line := fmt.Sprintf("  %s ext %v @v%d tag %d rec %v:%d skip %d off %v by v%d removal %d",
			kind, e.Extent, e.Version.Counter, e.Tag, e.Record.Segment, e.Record.Offset, e.Skip, e.Offloaded, e.By.Counter, e.Removal)
		if e.Clone != nil {
			line += fmt.Sprintf(" clone %+v", *e.Clone)
		}
		out += line + "\n"
	}
	return out
}

// sameEntry compares two read-out entries field by field, Clone by value: both
// nil, or the specs equal. A whole-struct compare would take the Clone pointer
// for a field and name every copied spec unequal, so each field is compared on
// its own. It is the comparison the rebuild property tests share, so a field
// the read-out drops fails both.
func sameEntry(a, b Entry) bool {
	switch {
	case a.Clone == nil || b.Clone == nil:
		if a.Clone != b.Clone {
			return false
		}
	case *a.Clone != *b.Clone:
		return false
	}
	// The Clone pointers have been compared by value above; clearing both
	// keeps the struct compare from taking the pointers for a field.
	a.Clone, b.Clone = nil, nil
	return a == b
}

// inspectFile reads one file out, through the read-out and nothing else.
func inspectFile(fs *files, id core.FileID) []Entry { return fs.inspect(id, nil) }

// heldTag, retainedTag and markerTag are the tags the expectation entries
// of each test carry; a per-test variable keeps the tables short while still
// naming the tag the read-out owes.
var (
	heldTag     core.ShareTag
	retainedTag core.ShareTag
	markerTag   core.ShareTag
)

// heldEntry, retainedEntry and markerEntry build the entries the tests
// expect, keeping the expectation tables short.
func heldEntry(off, n int64, r RecordAt, skip uint32, v uint64, offloaded bool) Entry {
	return Entry{
		Kind:      EntryHeld,
		Extent:    ext(off, n),
		Version:   ver(v),
		Tag:       heldTag,
		Record:    r,
		Skip:      skip,
		Offloaded: offloaded,
	}
}

func retainedEntry(off, n int64, r RecordAt, skip uint32, v, by uint64) Entry {
	return Entry{
		Kind:    EntryRetained,
		Extent:  ext(off, n),
		Version: ver(v),
		Tag:     retainedTag,
		Record:  r,
		Skip:    skip,
		By:      ver(by),
	}
}

// offloaded sets a built entry's offloaded bit, for the retained expectations
// whose pieces were marked before they were superseded.
func (e Entry) offloaded() Entry {
	e.Offloaded = true
	return e
}

func markerEntry(kind core.ChangeKind, off, n int64, v uint64) Entry {
	return Entry{Kind: EntryMarker, Extent: ext(off, n), Version: ver(v), Tag: markerTag, Removal: kind}
}

// TestInspectFileEntries: one file holding a three-way split, a piece split by
// a mark, retained shapes from two overlapping retaining writes, and the four
// marker kinds reads out exactly those entries. The markers go in first: a
// removal cuts content below its version, so the content is written above
// them and survives. Catches: a missing kind, a lost tail skip, a lost
// offloaded bit or By, a wrong Removal or Tag, and a merge across a bit
// change.
func TestInspectFileEntries(t *testing.T) {
	heldTag, retainedTag, markerTag = 7, 7, 7
	var fs files
	id := fid(1)
	spec := core.CloneSpec{Src: fid(9), SrcOff: 10, DstOff: 160, Len: 8, AsOf: ver(9)}
	change(t, &fs, id, func(f *file) error {
		// The four marker kinds, at versions the content below avoids.
		if err := f.recorded(3, ext(100, 8), nums(6, 6), 7, id); err != nil {
			return err
		}
		if err := f.deallocate(ext(100, 8), ver(6)); err != nil {
			return err
		}
		if err := f.recorded(3, ext(200, 0), nums(7, 7), 7, id); err != nil {
			return err
		}
		f.truncate(200, ver(7))
		if err := f.recorded(3, ext(0, 0), nums(8, 8), 7, id); err != nil {
			return err
		}
		f.held.fill(write(256, 4, rec(3, 0), 8)) // above the delete: it survives
		if err := f.recorded(3, ext(160, 8), nums(9, 9), 7, id); err != nil {
			return err
		}
		if err := f.cloneTarget(spec, ver(9)); err != nil {
			return err
		}
		// The first record holds [0, 32) whole at v10; a write inside it at
		// v11 splits it in three.
		if err := f.recorded(1, ext(0, 32), nums(10, 10), 7, id); err != nil {
			return err
		}
		f.write(write(0, 32, rec(1, 0), 10))
		f.write(write(8, 8, rec(1, 4096), 11))
		// A mark over half of [0, 8) splits the piece where its bit changes.
		f.held.markOffloaded(ext(0, 4), ver(0), ver(10))
		// Retained pieces: the second retaining write supersedes part of the
		// first's piece, which stays retained under the newer By. The [64, 80)
		// piece is marked first, so what the write retains carries the bit —
		// the read-out must report it.
		if err := f.recorded(2, ext(64, 16), nums(12, 12), 7, id); err != nil {
			return err
		}
		f.writeRetain(write(64, 16, rec(2, 4096), 12))
		f.held.markOffloaded(ext(64, 16), ver(0), ver(12))
		if err := f.recorded(2, ext(70, 4), nums(14, 14), 7, id); err != nil {
			return err
		}
		return nil
	})
	// The second retaining write lands outside the update above, so its
	// superseded shape is exactly predictable.
	change(t, &fs, id, func(f *file) error {
		f.writeRetain(write(70, 4, rec(2, 8192), 14))
		return nil
	})
	want := []Entry{
		heldEntry(0, 4, rec(1, 0), 0, 10, true),
		heldEntry(4, 4, rec(1, 0), 4, 10, false),
		heldEntry(8, 8, rec(1, 4096), 0, 11, false),
		heldEntry(16, 16, rec(1, 0), 16, 10, false),
		heldEntry(64, 6, rec(2, 4096), 0, 12, true),
		heldEntry(70, 4, rec(2, 8192), 0, 14, false),
		retainedEntry(70, 4, rec(2, 4096), 6, 12, 14).offloaded(),
		heldEntry(74, 6, rec(2, 4096), 10, 12, true),
		markerEntry(core.ChangeDeallocate, 100, 8, 6),
		{Kind: EntryMarker, Extent: ext(160, 8), Version: ver(9), Tag: 7, Removal: core.ChangeCloneTarget, Clone: &spec},
		markerEntry(core.ChangeTruncate, 200, math.MaxInt64-200, 7),
		heldEntry(256, 4, rec(3, 0), 0, 8, true),
	}
	got := inspectFile(&fs, id)
	if !slices.EqualFunc(got, want, sameEntry) {
		t.Fatalf("entries:\n got%s\nwant%s", fmtEntries(got), fmtEntries(want))
	}
	fs.view(id, func(f *file) {
		if n := int64(len(got)); n != f.entries() {
			t.Fatalf("read out %d entries, the file holds %d", n, f.entries())
		}
	})
	// Every entry names the file's tag.
	for _, e := range got {
		if e.Tag != 7 {
			t.Fatalf("entry %v names tag %d, want the file's 7", e, e.Tag)
		}
	}
}

// TestInspectToEndMarker: a marker running to the end of the file is reported
// as stored, with the to-end length the marker keeps, never the record's
// length 0. Catches reporting the marker as empty. The marker is written at a
// version below the held content's, so the content survives the removal and
// sits beside its marker in the read-out.
func TestInspectToEndMarker(t *testing.T) {
	heldTag, markerTag = 1, 1
	entries := func(apply func(f *file) error, wantEntry Entry, msg string) {
		t.Helper()
		var fs files
		id := fid(1)
		change(t, &fs, id, func(f *file) error {
			// The to-end marker first, at v2; the content at v3 survives it.
			if err := f.recorded(4, ext(0, 1), nums(2, 2), 1, id); err != nil {
				return err
			}
			if err := apply(f); err != nil {
				return err
			}
			if err := f.recorded(1, ext(0, 4), nums(3, 3), 1, id); err != nil {
				return err
			}
			f.write(write(0, 4, rec(1, 0), 3))
			return nil
		})
		got := inspectFile(&fs, id)
		if len(got) != 2 || !sameEntry(got[1], wantEntry) {
			t.Fatalf("entries = %v, want the %s and one write", got, msg)
		}
	}
	entries(func(f *file) error { f.truncate(100, ver(2)); return nil },
		markerEntry(core.ChangeTruncate, 100, math.MaxInt64-100, 2), "truncate marker {100, MaxInt64-100} @v2")
	entries(func(f *file) error { f.delete(ver(2)); return nil },
		markerEntry(core.ChangeDelete, 0, math.MaxInt64, 2), "delete marker {0, MaxInt64} @v2")
}

// TestInspectOrderTieBreaks: at one offset the read-out hands out a held
// piece, then retained pieces in storage order, then markers in version
// order. The markers are built the way remove builds them — an
// out-of-version-order marker slice is a state no operation can produce —
// and the retained pair is added in the reverse of version order, so a
// rewrite of either tie-break shows.
//
// The kind tie-break itself cannot be caught by this test: appendEntries
// appends held, then retained, then markers, so entries leaving the three
// loops already sit in kind order, and on five entries the sort is an
// insertion sort that never moves an equal offset pair. A kind-first
// comparison over offsets would still show here — nothing below moves an
// entry whose offset differs — so what the test pins is the offset-first
// key, and the stored order of each kind at one offset.
func TestInspectOrderTieBreaks(t *testing.T) {
	heldTag, retainedTag, markerTag = 0, 0, 0
	var fs files
	id := fid(1)
	change(t, &fs, id, func(f *file) error {
		// Retained v2, then retained v1: storage order is precisely the
		// reverse of version order here, so a wrong tie-break shows.
		f.retained.add(Retained{Piece: Piece{Extent: ext(2, 4), Record: rec(1, 4096), Version: ver(2)}, By: ver(6)})
		f.retained.add(Retained{Piece: Piece{Extent: ext(2, 4), Record: rec(1, 8192), Version: ver(1)}, By: ver(5)})
		// Markers inserted as remove inserts them: applied v5 first, the slice
		// keeps v3 before v5, version order.
		if err := f.deallocate(ext(2, 4), ver(5)); err != nil {
			return err
		}
		if err := f.deallocate(ext(2, 4), ver(3)); err != nil {
			return err
		}
		f.write(write(2, 4, rec(1, 0), 8))
		return nil
	})
	want := []Entry{
		heldEntry(2, 4, rec(1, 0), 0, 8, false),
		retainedEntry(2, 4, rec(1, 8192), 0, 1, 5),
		retainedEntry(2, 4, rec(1, 4096), 0, 2, 6),
		markerEntry(core.ChangeDeallocate, 2, 4, 3),
		markerEntry(core.ChangeDeallocate, 2, 4, 5),
	}
	if got := inspectFile(&fs, id); !slices.EqualFunc(got, want, sameEntry) {
		t.Fatalf("entries:\n got %v\nwant %v", got, want)
	}
}

// TestInspectCloneSpecIsACopy: mutating the Clone the read-out handed back
// changes nothing of a second read-out. Catches handing out the marker's own
// pointer.
func TestInspectCloneSpecIsACopy(t *testing.T) {
	markerTag = 1
	var fs files
	id := fid(1)
	spec := core.CloneSpec{Src: fid(9), SrcOff: 10, DstOff: 4, Len: 8, AsOf: ver(1)}
	change(t, &fs, id, func(f *file) error {
		if err := f.recorded(1, ext(4, 8), nums(2, 2), 1, id); err != nil {
			return err
		}
		return f.cloneTarget(spec, ver(2))
	})
	entries := inspectFile(&fs, id)
	if len(entries) != 1 || entries[0].Clone == nil {
		t.Fatalf("entries = %v, want one marker with a clone spec", entries)
	}
	*entries[0].Clone = core.CloneSpec{Src: fid(1)}
	want := markerEntry(core.ChangeCloneTarget, 4, 8, 2)
	want.Clone = &spec
	if again := inspectFile(&fs, id); !slices.EqualFunc(again, []Entry{want}, sameEntry) {
		t.Fatalf("a second read-out changed: %v", again)
	}
}

// TestInspectMissingFileCreatesNothing: a read-out of an unknown id yields
// nothing and creates no entry. Catches a read-out that goes through update
// or commit.
func TestInspectMissingFileCreatesNothing(t *testing.T) {
	var fs files
	if got := inspectFile(&fs, fid(1)); got != nil {
		t.Fatalf("read-out of a missing file = %v, want nil", got)
	}
	if n := fs.len(); n != 0 {
		t.Fatalf("len() = %d, want 0", n)
	}
}

// inspectState is the index's whole state, captured as the does-not-mutate
// check compares it. Every per-file field — pieces, entries, change version,
// tags, rival, segments, drop ranges, scratch — is keyed by FileID, so the
// comparison sees every file's state and not only the one the shard map
// iteration reached last; the per-file slices are captured under each file's
// own view, in FileID order, so two captures of the same index list them the
// same way.
type inspectState struct {
	files     int
	entries   map[core.FileID][]Entry
	held      map[core.FileID][]Piece
	retained  map[core.FileID][]Retained
	change    map[core.FileID]core.Version
	hasChange map[core.FileID]bool
	tag       map[core.FileID]core.ShareTag
	tagged    map[core.FileID]bool
	rival     map[core.FileID]core.ShareTag
	rivalled  map[core.FileID]bool
	segs      map[core.FileID][]core.SegmentID
	ranges    map[core.FileID]map[core.SegmentID][]dropRange
	count     int64
	drops     int64
	scratch   map[core.FileID][2]int
	work      map[core.FileID]int
	parts     map[core.FileID]int
	buckets   [floorBuckets]uint64
	segments  map[core.SegmentID]SegmentBytes
}

// capture copies the index's whole state out, no read-out involved.
func capture(fs *files) inspectState {
	var st inspectState
	clear := func() {
		st.entries = map[core.FileID][]Entry{}
		st.held = map[core.FileID][]Piece{}
		st.retained = map[core.FileID][]Retained{}
		st.change = map[core.FileID]core.Version{}
		st.hasChange = map[core.FileID]bool{}
		st.tag = map[core.FileID]core.ShareTag{}
		st.tagged = map[core.FileID]bool{}
		st.rival = map[core.FileID]core.ShareTag{}
		st.rivalled = map[core.FileID]bool{}
		st.segs = map[core.FileID][]core.SegmentID{}
		st.ranges = map[core.FileID]map[core.SegmentID][]dropRange{}
		st.scratch = map[core.FileID][2]int{}
		st.work = map[core.FileID]int{}
		st.parts = map[core.FileID]int{}
	}
	clear()
	st.segments = map[core.SegmentID]SegmentBytes{}

	var ids []core.FileID
	for i := range fs.shards {
		s := &fs.shards[i]
		s.mu.Lock()
		for id := range s.m {
			ids = append(ids, id)
		}
		s.mu.Unlock()
	}
	slices.SortFunc(ids, compareFileIDs)
	st.files = len(ids)
	for _, id := range ids {
		fs.view(id, func(f *file) {
			st.held[id] = f.held.pieces(nil)
			st.retained[id] = f.retained.list(nil)
			st.entries[id] = f.appendEntries(nil)
			st.change[id], st.hasChange[id] = f.changeVersion()
			st.tag[id], st.tagged[id] = f.owner()
			st.rival[id], st.rivalled[id] = f.rival, f.rivalled
			st.segs[id] = make([]core.SegmentID, 0, len(f.segs))
			st.ranges[id] = map[core.SegmentID][]dropRange{}
			for _, sr := range f.segs {
				st.segs[id] = append(st.segs[id], sr.seg)
				st.ranges[id][sr.seg] = slices.Clone(sr.ranges)
			}
			slices.Sort(st.segs[id])
			st.scratch[id] = [2]int{cap(f.held.scratch), len(f.held.scratch)}
			st.work[id] = cap(f.held.work)
			st.parts[id] = cap(f.retained.parts)
		})
	}
	for b := range fs.buckets {
		st.buckets[b] = fs.buckets[b].Load()
	}
	st.count = fs.extentCount()
	st.drops = fs.dropRangeCount()

	// Every segment any file names contributes its counters to the table compare.
	segs := make(map[core.SegmentID]bool)
	for _, ss := range st.segs {
		for _, seg := range ss {
			segs[seg] = true
		}
	}
	for seg := range segs {
		st.segments[seg] = fs.segmentBytes(seg)
	}
	return st
}

// TestInspectDoesNotMutate: a read-out of every driven file and a drain of the
// bulk read-out leave the index where it stood. Catches scratch reuse, a
// counter or bucket the read-out touched, and entry creation.
func TestInspectDoesNotMutate(t *testing.T) {
	h := drive(t, 7, 500)
	fs := &h.fs
	before := capture(fs)

	drive(t, 0, 0) // keeps the helper's rng out of the capture
	var ids []core.FileID
	for id := range fs.inspectAll() {
		ids = append(ids, id)
	}
	for _, id := range ids {
		inspectFile(fs, id)
	}
	inspectFile(fs, fid(1))
	inspectFile(fs, fid(200)) // a file with no entry

	after := capture(fs)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the read-out changed the index:\nbefore %v\nafter  %v", before, after)
	}

	// A second drain yields the same files.
	var second []core.FileID
	for id := range fs.inspectAll() {
		second = append(second, id)
	}
	if !slices.Equal(ids, second) {
		t.Fatalf("two drains yielded %v and %v", ids, second)
	}
}

// TestInspectEntriesMatchCount: after a driven history, the read-out's entry
// total is the count, and its held and retained bytes per segment equal the
// per-segment counters. Catches a read-out that skips a kind or a bit state.
func TestInspectEntriesMatchCount(t *testing.T) {
	for _, seed := range []uint64{1, 2, 3} {
		h := drive(t, seed, 2000)
		fs := &h.fs
		var entries int64
		perSeg := map[core.SegmentID]SegmentBytes{}
		for _, es := range fs.inspectAll() {
			for _, e := range es {
				entries++
				switch e.Kind {
				case EntryHeld:
					b := perSeg[e.Record.Segment]
					b.Held += e.Extent.Len
					perSeg[e.Record.Segment] = b
				case EntryRetained:
					b := perSeg[e.Record.Segment]
					b.Retained += e.Extent.Len
					perSeg[e.Record.Segment] = b
				}
			}
		}
		if entries != fs.extentCount() {
			t.Fatalf("seed %d: read out %d entries, extentCount() = %d", seed, entries, fs.extentCount())
		}
		walk := walkSegments(t, fs)
		for seg, want := range walk {
			if got := perSeg[seg]; got != want {
				t.Fatalf("seed %d: segment %d read out %+v, walk says %+v", seed, seg, got, want)
			}
		}
		for seg, got := range perSeg {
			if _, ok := walk[seg]; !ok {
				t.Fatalf("seed %d: read out segment %d the walk does not know: %+v", seed, seg, got)
			}
		}
	}
}

// testFiles builds n files, file i holding one piece at [0, 8) of segment i+1,
// with their tags set to i, so their read-outs can be told apart.
func testFiles(t *testing.T, fs *files, n byte) {
	t.Helper()
	for i := byte(1); i <= n; i++ {
		id := fid(i)
		change(t, fs, id, func(f *file) error {
			if err := f.recorded(core.SegmentID(i), ext(0, 8), nums(uint64(i), uint64(i)), core.ShareTag(i), id); err != nil {
				return err
			}
			f.write(write(0, 8, rec(core.SegmentID(i), 0), uint64(i)))
			return nil
		})
	}
}

// TestInspectAllOrderAndSkips: the bulk read-out yields the files in FileID
// order, each once, each equal to its own read-out, and skips a file whose
// entry holds nothing but its tag. A yield that returns false stops the walk
// at once. Catches shard-order iteration, duplicates, yielding empty files
// and ignoring yield's answer.
func TestInspectAllOrderAndSkips(t *testing.T) {
	var fs files
	testFiles(t, &fs, 5)
	// A file with a tag only, to be skipped.
	change(t, &fs, fid(6), func(f *file) error {
		return f.recorded(7, ext(0, 4), nums(1, 1), 7, fid(6))
	})

	var got []core.FileID
	var all [][]Entry
	for id, es := range fs.inspectAll() {
		got = append(got, id)
		all = append(all, es)
	}
	want := []core.FileID{fid(1), fid(2), fid(3), fid(4), fid(5)}
	if !slices.Equal(got, want) {
		t.Fatalf("inspectAll yielded %v, want %v", got, want)
	}
	for i, id := range got {
		if !slices.EqualFunc(all[i], inspectFile(&fs, id), sameEntry) {
			t.Fatalf("bulk read-out of %d differs from its own read-out:\n%v\n%v", id, all[i], inspectFile(&fs, id))
		}
	}

	// A break after the first yield sees exactly one file.
	var one []core.FileID
	fs.inspectAll()(func(id core.FileID, _ []Entry) bool {
		one = append(one, id)
		return false
	})
	if len(one) != 1 {
		t.Fatalf("a broken drain yielded %d files, want 1", len(one))
	}
}

// TestInspectAllYieldsHoldingNoLock: a yield that writes to its own file and
// to a file in another shard still runs, so no lock is held across it.
func TestInspectAllYieldsHoldingNoLock(t *testing.T) {
	var fs files
	testFiles(t, &fs, 8)

	done := make(chan []core.FileID, 1)
	go func() {
		var ids []core.FileID
		fs.inspectAll()(func(id core.FileID, _ []Entry) bool {
			change(t, &fs, id, func(f *file) error {
				f.write(write(8, 8, rec(1, 4096), 99))
				return nil
			})
			other := fid(byte(id.Bytes()[15]) + 20)
			_ = fs.update(other, func(f *file) error {
				f.write(write(0, 8, rec(1, 0), 98))
				return nil
			})
			ids = append(ids, id)
			return true
		})
		done <- ids
	}()
	select {
	case ids := <-done:
		if len(ids) == 0 {
			t.Fatal("the drain yielded nothing")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a yield blocked writing its own file: a lock is held across yield")
	}
}

// TestInspectAllDuringChanges: a file removed inside a yield is not yielded,
// and one created during the walk, where the drain reaches it, reads out
// like any other. Catches stale yields and panics on vanished entries.
func TestInspectAllDuringChanges(t *testing.T) {
	var fs files
	testFiles(t, &fs, 4)

	var yielded []core.FileID
	fs.inspectAll()(func(id core.FileID, _ []Entry) bool {
		yielded = append(yielded, id)
		if len(yielded) == 1 {
			// Remove fid(4) so it leaves the map, and create fid(10), which
			// sorts after every file already there.
			if err := fs.update(fid(4), func(f *file) error {
				f.held.cut(toEnd(0))
				f.segmentGone(4)
				return nil
			}); err != nil {
				t.Errorf("remove: %v", err)
			}
			change(t, &fs, fid(10), func(f *file) error {
				f.write(write(0, 4, rec(1, 0), 50))
				return nil
			})
		}
		return true
	})
	if len(yielded) >= 4 && yielded[3] == fid(4) {
		t.Fatalf("a file removed inside the first yield was still yielded: %v", yielded)
	}
}

// TestInspectFileConsistentUnderWrites: while a writer rewrites one file in
// eight-piece rounds, each round at one version with the pieces' segments
// drawn from it, every read-out sees either nothing or a whole round at one
// version. Catches locking per piece, or reading with no lock.
func TestInspectFileConsistentUnderWrites(t *testing.T) {
	var fs files
	id := fid(1)
	fs.setExtentLimit(1 << 20)
	change(t, &fs, id, func(f *file) error {
		return f.recorded(core.SegmentID(80), ext(0, 1), nums(1, 1), 1, id)
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := int64(1); i <= 300; i++ {
			err := fs.update(id, func(f *file) error {
				f.held.cut(toEnd(0))
				for j := range int64(8) {
					off := j * 4096
					if err := f.recorded(core.SegmentID(i), ext(off, 4096), nums(uint64(i), uint64(i)), 1, id); err != nil {
						return err
					}
					f.write(write(off, 4096, rec(core.SegmentID(i), uint32(off)), uint64(i)))
				}
				return nil
			})
			if err != nil {
				panic(err)
			}
		}
	}()

	// The reader stops when the writer ends, checking every read-out it takes
	// against the invariant. round checks one.
	round := func(es []Entry) (core.Version, bool) {
		if len(es) == 0 {
			return core.Version{}, true
		}
		if len(es) != 8 {
			return core.Version{}, false
		}
		for i, e := range es {
			if e.Kind != EntryHeld || e.Extent != ext(int64(i)*4096, 4096) {
				return core.Version{}, false
			}
		}
		var at int64
		for _, e := range es {
			at += e.Extent.Len
		}
		if at != 8*4096 {
			return core.Version{}, false
		}
		return es[0].Version, es[0].Version == es[7].Version
	}
	var last core.Version
	stopped := false
	for !stopped {
		func() {
			es := inspectFile(&fs, id)
			v, ok := round(es)
			if !ok {
				t.Errorf("a read-out mid-round: %v", es)
				return
			}
			if v.Compare(last) < 0 {
				t.Errorf("a read-out went back: %v after %v", v, last)
				return
			}
			last = v
			for e := range fs.inspectAll() {
				_ = e
				break
			}
		}()
		select {
		case <-done:
			stopped = true
		default:
		}
	}
	// The writer having stopped, the final read-out is a whole round.
	if _, ok := round(inspectFile(&fs, id)); !ok {
		t.Fatalf("read-out after the writer stopped: %v", inspectFile(&fs, id))
	}
}
