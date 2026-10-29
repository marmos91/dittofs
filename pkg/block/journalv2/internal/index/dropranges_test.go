package index

import (
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// nums is a test pair: version v, sequence number s.
func nums(v, s uint64) Numbers { return Numbers{Version: ver(v), Seq: core.Seq(s)} }

// noteRec notes one record of id in seg through fs, with the extent, pair and
// tag given. It is the one-line form of every arranged record in these tests.
func noteRec(t *testing.T, fs *files, id core.FileID, seg core.SegmentID, e core.Extent, n Numbers) {
	t.Helper()
	err := fs.update(id, func(f *file) error { return f.recorded(seg, e, n, 1, id) })
	if err != nil {
		t.Fatalf("recorded: %v", err)
	}
}

// rangesOf returns the drop ranges fs holds for id in seg, and whether the
// file has an entry at all.
func rangesOf(fs *files, id core.FileID, seg core.SegmentID) ([]dropRange, bool) {
	var rs []dropRange
	ok := fs.view(id, func(f *file) {
		for _, s := range f.segs {
			if s.seg == seg {
				rs = s.ranges
				return
			}
		}
	})
	return rs, ok
}

// totalRanges returns the sum of range counts over every segment of every
// entry, which the gauge must equal.
func totalRanges(fs *files) int {
	n := 0
	for i := range fs.shards {
		s := &fs.shards[i]
		s.mu.Lock()
		for _, f := range s.m {
			f.mu.RLock()
			n += f.nranges
			f.mu.RUnlock()
		}
		s.mu.Unlock()
	}
	return n
}

// checkSegmentRanges asserts the structural invariants of one segment's
// ranges: at most maxDropRanges, sorted by start, none overlapping.
func checkSegmentRanges(t *testing.T, rs []dropRange, id core.FileID, seg core.SegmentID) {
	t.Helper()
	if len(rs) > maxDropRanges {
		t.Fatalf("file %v, segment %d: %d ranges, want at most %d", id, seg, len(rs), maxDropRanges)
	}
	for i, r := range rs {
		if r.start >= r.end {
			t.Fatalf("file %v, segment %d: range %d is empty: %+v", id, seg, i, r)
		}
		if i > 0 && rs[i-1].end > r.start {
			t.Fatalf("file %v, segment %d: ranges %d and %d overlap: %+v, %+v", id, seg, i-1, i, rs[i-1], r)
		}
	}
}

// exactIndex is the exact answer the property test compares against: every record
// ever noted and not forgotten, exactly as noted.
type exactIndex struct {
	// segs maps each segment to the records it holds of the file.
	segs map[core.SegmentID][]indexedRec
}

// indexedRec is one record of the exact index: the extent it covers and
// the pair it was noted with and no lower.
type indexedRec struct {
	e core.Extent
	n Numbers
}

// newExactIndex returns an empty exact index.
func newExactIndex() exactIndex {
	return exactIndex{segs: map[core.SegmentID][]indexedRec{}}
}

// note records that seg holds e with pair n.
func (o *exactIndex) note(seg core.SegmentID, e core.Extent, n Numbers) {
	o.segs[seg] = append(o.segs[seg], indexedRec{e, n})
}

// forget drops every record of seg, as segmentGone does.
func (o *exactIndex) forget(seg core.SegmentID) { delete(o.segs, seg) }

// segments returns the sorted segments holding any record of the file.
func (o *exactIndex) segments() []core.SegmentID {
	ks := make([]core.SegmentID, 0, len(o.segs))
	for k := range o.segs {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// mayOutrank answers the drop query exactly: whether a record of another
// segment overlaps e with a pair strictly below n.
func (o *exactIndex) mayOutrank(seg core.SegmentID, e core.Extent, n Numbers) bool {
	for oSeg, recs := range o.segs {
		if oSeg == seg {
			continue
		}
		for _, r := range recs {
			if r.e.Len > 0 && overlaps(r.e.Off, r.e.Off+r.e.Len, e.Off, e.Off+e.Len) && r.n.Compare(n) < 0 {
				return true
			}
		}
	}
	return false
}

// overlaps reports whether the byte ranges [aLo, aHi) and [bLo, bHi) share a
// byte. One that only touches the other does not overlap.
func overlaps(aLo, aHi, bLo, bHi int64) bool { return aLo < bHi && bLo < aHi }

// TestDropRangesAgainstExactOracle is the property test: whatever the note and
// merge order, every record lies inside exactly one range of its segment with
// a low pair no greater than its own, the set of segments marks exactly those
// holding a record, and a drop query is answered soundly. The counted check at
// the end proves the run reaches the ninth-range merge, so checks (a) and (c)
// cover the merged ranges under random arrival order: removing the cap fails
// (a), and merging away coverage or keeping a pair above a record fails (c).
// Which pair the ninth merge picks — smallest gap, lowest offsets on a tie —
// is asserted exactly by TestNinthMergesSmallestGap and
// TestNinthMergeTieTakesLowestOffsets, which the one-directional soundness
// check (d) cannot see.
func TestDropRangesAgainstExactOracle(t *testing.T) {
	var fs files
	const filesN, segsN = 3, 5
	ids := [filesN]core.FileID{fid(1), fid(2), fid(3)}
	os := [filesN]exactIndex{newExactIndex(), newExactIndex(), newExactIndex()}

	rng := rand.New(rand.NewPCG(3374, 42))
	const space = 64 << 10
	// ninthNotes counts notes that must have run the ninth-range merge: a
	// record with an extent noted into a segment that already holds eight
	// ranges none of which it overlaps.
	ninthNotes := 0
	for step := 0; step < 4000; step++ {
		fi := rng.IntN(filesN)
		seg := core.SegmentID(1 + rng.IntN(segsN))
		if step%20 == 19 {
			_ = fs.update(ids[fi], func(f *file) error { f.segmentGone(seg); return nil })
			os[fi].forget(seg)
		} else {
			var e core.Extent
			switch rng.IntN(12) {
			case 0:
				e = core.Extent{} // no extent: marks the segment only
			case 1:
				e = core.Extent{Off: int64(rng.IntN(space)), Len: math.MaxInt64} // to-end
				e.Len = math.MaxInt64 - e.Off
			default:
				off := int64(rng.IntN(space))
				var n int64
				if rng.IntN(2) == 0 {
					n = 1 + int64(rng.IntN(64)) // short: lets a segment reach nine ranges
				} else {
					n = 1 + int64(rng.IntN(int(space-off)))
				}
				e = ext(off, n)
			}
			n := nums(1+uint64(rng.Int64N(64)), uint64(step+1))
			if e.Len > 0 {
				if rs, ok := rangesOf(&fs, ids[fi], seg); ok && len(rs) == maxDropRanges {
					overlapped := false
					for _, r := range rs {
						if r.overlaps(e.Off, e.Off+e.Len) {
							overlapped = true
							break
						}
					}
					if !overlapped {
						ninthNotes++
					}
				}
			}
			noteRec(t, &fs, ids[fi], seg, e, n)
			os[fi].note(seg, e, n)
		}

		// (a) and (b): each segment's ranges are bounded, sorted, disjoint,
		// and exactly the segments holding a record are marked.
		for i, id := range ids {
			wantSegs := os[i].segments()
			_ = fs.view(id, func(f *file) {
				gotSegs := make([]core.SegmentID, len(f.segs))
				for j, s := range f.segs {
					gotSegs[j] = s.seg
					checkSegmentRanges(t, s.ranges, id, s.seg)
				}
				slices.Sort(gotSegs)
				if !slices.Equal(gotSegs, wantSegs) {
					t.Fatalf("step %d, file %v: segments %v, want %v", step, id, gotSegs, wantSegs)
				}
			})
			if got, want := present(&fs, id), len(wantSegs) > 0; got != want {
				t.Fatalf("step %d, file %v: entry present = %v with segments %v, want %v", step, id, got, wantSegs, want)
			}
		}

		// (c): every exact-index record with an extent lies inside exactly one
		// range of its segment, whose low pair is no greater than its own.
		for i, id := range ids {
			for seg, recs := range os[i].segs {
				rs, ok := rangesOf(&fs, id, seg)
				if !ok {
					t.Fatalf("step %d: file %v has no entry", step, id)
				}
				for _, r := range recs {
					if r.e.Len == 0 {
						continue
					}
					inside := 0
					for _, q := range rs {
						if q.start <= r.e.Off && r.e.Off+r.e.Len <= q.end {
							inside++
							if q.low.Compare(r.n) > 0 {
								t.Fatalf("step %d, file %v: range %+v keeps pair above record %+v", step, id, q, r)
							}
						}
					}
					if inside != 1 {
						t.Fatalf("step %d, file %v: record %+v lies in %d ranges of segment %d, want 1:\n%+v", step, id, r, inside, seg, rs)
					}
				}
			}
		}

		// (d) and (e): random queries answered soundly, and the gauge exact.
		// Soundness is one direction: whenever the oracle has a lower
		// overlapping record in another segment, the answer must be true. The
		// answer may be true where the oracle has none — a merged range
		// over-covers, keeping a removal longer, the safe direction: a range
		// may cover bytes no record holds, but under-covering would drop a
		// removal it needs. Over-answering is bounded by (c): the range's low
		// is never above any record inside it.
		for range 32 {
			fi := rng.IntN(filesN)
			seg := core.SegmentID(1 + rng.IntN(segsN))
			e := ext(int64(rng.IntN(space)), 1+int64(rng.IntN(64)))
			n := nums(1+uint64(rng.Int64N(64)), uint64(step+1))
			if want, got := os[fi].mayOutrank(seg, e, n), fs.mayOutrank(ids[fi], seg, e, n); want && !got {
				t.Fatalf("step %d, file %v: mayOutrank(seg %d, %+v, %+v) = %v, want true: a lower record is under-covered", step, ids[fi], seg, e, n, got)
			}
		}
		if got, want := fs.dropRangeCount(), int64(totalRanges(&fs)); got != want {
			t.Fatalf("after step %d: gauge = %d, want %d", step, got, want)
		}
	}
	if ninthNotes == 0 {
		t.Fatal("no note ever reached the ninth-range merge: the run's extents never gave a segment nine disjoint records")
	}
}

// TestDropRangesExactUpToEight: with at most eight disjoint ranges, no merge
// ever runs, so the answer is the exact index's, exactly. Merging below the ninth
// over-covers, and answers true where the exact index says false.
func TestDropRangesExactUpToEight(t *testing.T) {
	var fs files
	id := fid(1)
	// Eight disjoint, non-touching extents of one segment, each its own range
	// with a distinct pair.
	for i := range int64(8) {
		noteRec(t, &fs, id, 2, ext(8*i, 4), nums(uint64(i+1), uint64(i+1)))
	}
	o := newExactIndex()
	for i := range int64(8) {
		o.note(2, ext(8*i, 4), nums(uint64(i+1), uint64(i+1)))
	}
	// Grid of queries from another segment: extents at and around every noted
	// range's bounds, and pairs around every noted pair.
	for qOff := int64(0); qOff < 64; qOff++ {
		for _, qLen := range []int64{1, 2, 4, 8} {
			if qOff+qLen > 64 {
				continue
			}
			e := ext(qOff, qLen)
			for v := uint64(1); v <= 9; v++ {
				n := nums(v, v)
				if got, want := fs.mayOutrank(id, 1, e, n), o.mayOutrank(1, e, n); got != want {
					t.Fatalf("mayOutrank(%+v, pair %d) = %v, want %v", e, v, got, want)
				}
			}
		}
	}
}

// TestNinthMergeTieTakesLowestOffsets: on a tie between two equal smallest
// gaps, the pair at the lower offsets merges and the pair above stays split.
// The tie-break is the deterministic ordering behind the rebuild decision, so
// a comparison flipped to <= changes which ranges merge with nothing else
// noticing.
func TestNinthMergeTieTakesLowestOffsets(t *testing.T) {
	var fs files
	id := fid(1)
	// Eight disjoint ranges, four bytes each. Sorted starts: 0, 12, 40, 52,
	// 100, 140, 180, 220; the gaps are 8, 24, 8, 44, 36, 36, 36 — the
	// smallest gap, 8, ties between the pairs at 0 and 12 and at 40 and 52.
	offs := []int64{220, 100, 40, 12, 140, 180, 52, 0}
	for i, off := range offs {
		noteRec(t, &fs, id, 1, ext(off, 4), nums(uint64(100-i), uint64(100-i)))
	}
	before, _ := rangesOf(&fs, id, 1)
	if len(before) != 8 {
		t.Fatalf("%d ranges before the ninth, want 8", len(before))
	}
	// The ninth: far from every gap, with the highest pair.
	noteRec(t, &fs, id, 1, ext(300, 4), nums(200, 200))
	rs, _ := rangesOf(&fs, id, 1)
	if len(rs) != 8 {
		t.Fatalf("%d ranges after the ninth, want 8", len(rs))
	}
	// The lower-offset tied pair merged into [0, 16): offs[i] carries the
	// pair 100-i, so the records at 0 (offs[7]) and 12 (offs[3]) carry 93
	// and 97, and the merged low is 93. The higher tied pair at 40 and 52
	// stays as its two ranges.
	want := dropRange{start: 0, end: 16, low: nums(93, 93)}
	if !slices.Contains(rs, want) {
		t.Fatalf("the lowest-offset tied pair did not merge: %+v not in %+v", want, rs)
	}
	for _, start := range []int64{40, 52} {
		if !slices.ContainsFunc(rs, func(r dropRange) bool { return r.start == start && r.end == start+4 }) {
			t.Fatalf("the higher tied pair at %d merged too or was changed: %+v", start, rs)
		}
	}
}

// TestNinthMergesSmallestGap: a ninth range in one segment merges the adjacent
// pair with the smallest gap between them, keeping the lower pair. Merging the
// largest gap instead would drop the coverage of the pair discarded and keep
// the smallest one split. Ranges here start, sorted, at 0, 20, 40, 60, 66, 82,
// 100 and 118, each four long: the gaps are 16, 16, 16, 2, 12, 14 and 14, so
// the pair at 60 and 66 merges, keeping pairs 96 and 95, of which 95 is lower.
func TestNinthMergesSmallestGap(t *testing.T) {
	var fs files
	id := fid(1)
	// offs[i], noted in this order, carries the pair 100-i: the record at
	// 66 (i=4) carries 96 and the one at 60 (i=1) carries 99.
	offs := []int64{100, 60, 82, 0, 66, 20, 118, 40}
	for i, off := range offs {
		noteRec(t, &fs, id, 1, ext(off, 4), nums(uint64(100-i), uint64(100-i)))
	}
	before, _ := rangesOf(&fs, id, 1)
	if len(before) != 8 {
		t.Fatalf("%d ranges before the ninth, want 8", len(before))
	}
	// The ninth: far from every gap, with the highest pair.
	noteRec(t, &fs, id, 1, ext(200, 4), nums(200, 200))
	rs, _ := rangesOf(&fs, id, 1)
	if len(rs) != 8 {
		t.Fatalf("%d ranges after the ninth, want 8", len(rs))
	}
	want := dropRange{start: 60, end: 70, low: nums(96, 96)}
	if !slices.Contains(rs, want) {
		t.Fatalf("merged range %+v not found in %+v", want, rs)
	}
	// Every unmerged range is still there, unchanged.
	for _, r := range before {
		if r.start == 60 || r.start == 66 {
			continue
		}
		if !slices.Contains(rs, r) {
			t.Fatalf("range %+v was changed by the merge, in %+v", r, rs)
		}
	}
}

// TestOverlapMergesKeepsLowerPair: a record overlapping two ranges collapses
// them and itself into one range spanning all three, with the minimum pair of
// the three. Keeping the higher pair would answer false where the exact index
// says true. A record that only touches a range stays a range of its own.
func TestOverlapMergesKeepsLowerPair(t *testing.T) {
	var fs files
	id := fid(1)
	noteRec(t, &fs, id, 1, ext(0, 8), nums(9, 1))
	noteRec(t, &fs, id, 1, ext(12, 8), nums(5, 2)) // the lowest pair
	noteRec(t, &fs, id, 1, ext(24, 8), nums(7, 3))
	// A record overlapping both of the first two, with the highest pair of
	// any: [6, 16) covers [0, 8) and [12, 20).
	noteRec(t, &fs, id, 1, ext(6, 10), nums(20, 4))
	rs, _ := rangesOf(&fs, id, 1)
	if len(rs) != 2 {
		t.Fatalf("%+v: %d ranges, want 2", rs, len(rs))
	}
	if rs[0].start != 0 || rs[0].end != 20 {
		t.Fatalf("merged range = %+v, want [0, 20)", rs[0])
	}
	if rs[0].low.Compare(nums(5, 2)) != 0 {
		t.Fatalf("merged low = %+v, want %+v", rs[0].low, nums(5, 2))
	}
	// A record whose end touches the next range's start stays separate.
	noteRec(t, &fs, id, 1, ext(20, 4), nums(3, 5))
	rs, _ = rangesOf(&fs, id, 1)
	if len(rs) != 3 {
		t.Fatalf("a touching record did not stay a separate range: %+v", rs)
	}
}

// TestDropQueryIgnoresOwnSegment: a lower record in the removal's own segment
// does not keep it. The drop test asks only about records in other segments.
func TestDropQueryIgnoresOwnSegment(t *testing.T) {
	var fs files
	id := fid(1)
	noteRec(t, &fs, id, 1, ext(0, 16), nums(2, 1))
	if fs.mayOutrank(id, 1, ext(0, 16), nums(5, 2)) {
		t.Fatal("a lower record in the removal's own segment keeps it")
	}
	// The same record in another segment does.
	noteRec(t, &fs, id, 2, ext(8, 16), nums(2, 1))
	if !fs.mayOutrank(id, 1, ext(0, 16), nums(5, 2)) {
		t.Fatal("a lower record of another segment fails to keep the removal")
	}
}

// TestDropQueryNeedsOverlap: a record of another segment keeps the removal
// only where it overlaps its extent. One that only touches it, or lies
// outside it, does not; a one-byte overlap does.
func TestDropQueryNeedsOverlap(t *testing.T) {
	var fs files
	id := fid(1)
	noteRec(t, &fs, id, 2, ext(16, 8), nums(1, 1))
	if fs.mayOutrank(id, 1, ext(0, 16), nums(5, 2)) { // [0,16) and [16,24) touch
		t.Fatal("a record only touching the extent keeps the removal")
	}
	if fs.mayOutrank(id, 1, ext(0, 8), nums(5, 2)) {
		t.Fatal("a record outside the extent keeps the removal")
	}
	if !fs.mayOutrank(id, 1, ext(15, 2), nums(5, 2)) { // [15, 17): one byte
		t.Fatal("a one-byte overlap fails to keep the removal")
	}
	// A zero-length extent covers no byte, so nothing overlaps it — not even
	// a lower record whose range spans the offset. Without the guard, the
	// strict-overlap test r.start < hi && lo < r.end holds of a range around
	// the offset, and a no-extent removal would be wrongly kept.
	if fs.mayOutrank(id, 1, core.Extent{Off: 20, Len: 0}, nums(5, 2)) {
		t.Fatal("a zero-length removal inside a lower range is kept")
	}
	if fs.mayOutrank(id, 1, core.Extent{}, nums(5, 2)) {
		t.Fatal("a zero-length removal at 0 inside a lower range is kept")
	}
}

// TestDropQueryComparesVersionFirst: the pair is compared version first, then
// sequence number, and an equal pair does not keep the removal: that is the
// record itself or a carried copy of it, never a record it outranks. Each case
// notes its record at its own extent, so the one below it never answers for it.
func TestDropQueryComparesVersionFirst(t *testing.T) {
	var fs files
	id := fid(1)
	// A lower version with a higher seq keeps the removal.
	noteRec(t, &fs, id, 2, ext(0, 8), nums(3, 99))
	if !fs.mayOutrank(id, 1, ext(0, 8), nums(4, 1)) {
		t.Fatal("a record with a lower version but higher seq failed to keep the removal")
	}
	// A higher version with a lower seq does not: the version compares first.
	noteRec(t, &fs, id, 3, ext(16, 8), nums(5, 1))
	if fs.mayOutrank(id, 1, ext(16, 8), nums(4, 99)) {
		t.Fatal("a record with a higher version but lower seq kept the removal")
	}
	// The same version with a lower seq keeps it.
	noteRec(t, &fs, id, 4, ext(32, 8), nums(5, 9))
	if !fs.mayOutrank(id, 1, ext(32, 8), nums(5, 10)) {
		t.Fatal("a record of the same version with a lower seq failed to keep the removal")
	}
	// An equal pair does not.
	noteRec(t, &fs, id, 5, ext(48, 8), nums(5, 10))
	if fs.mayOutrank(id, 1, ext(48, 8), nums(5, 10)) {
		t.Fatal("a record with an equal pair keeps the removal")
	}
}

// TestDropQueryOtherFileIgnored: the query answers only in terms of the one
// file: a lower overlapping record of another file does not keep this one's
// removal, and a file with no entry has no record to keep one at all.
func TestDropQueryOtherFileIgnored(t *testing.T) {
	var fs files
	a, b := fid(1), fid(2)
	noteRec(t, &fs, b, 1, ext(0, 8), nums(1, 1))
	if fs.mayOutrank(a, 2, ext(0, 8), nums(5, 2)) {
		t.Fatal("another file's lower overlapping record keeps a's removal")
	}
	if fs.mayOutrank(fid(3), 2, ext(0, 8), nums(5, 2)) {
		t.Fatal("a file with no entry reports itself kept")
	}
}

// TestSegmentGoneForgetsItsRanges: once a segment is unlinked, its records are
// gone with it, so the range that kept a removal no longer does, the gauge
// falls by its range count, and a second call is a no-op.
func TestSegmentGoneForgetsItsRanges(t *testing.T) {
	var fs files
	id := fid(1)
	noteRec(t, &fs, id, 1, ext(0, 8), nums(4, 2))
	noteRec(t, &fs, id, 2, ext(0, 8), nums(1, 1))
	if !fs.mayOutrank(id, 1, ext(0, 8), nums(4, 2)) {
		t.Fatal("segment 2 does not have a lower record")
	}
	before := fs.dropRangeCount()
	_ = fs.update(id, func(f *file) error { f.segmentGone(2); return nil })
	if fs.mayOutrank(id, 1, ext(0, 8), nums(4, 2)) {
		t.Fatal("a gone segment keeps the removal")
	}
	if after, want := fs.dropRangeCount(), before-1; after != want {
		t.Fatalf("gauge = %d after segmentGone, want %d", after, want)
	}
	_ = fs.update(id, func(f *file) error { f.segmentGone(2); return nil })
	if after, want := fs.dropRangeCount(), before-1; after != want {
		t.Fatalf("gauge = %d after a second segmentGone, want %d", after, want)
	}
}

// TestSegmentGoneClearsTheVacatedSlot: the swap-remove moves the last segment
// entry over the removed one, leaving the last slot holding the tail entry —
// and its ranges array — past the slice's end. A slot that nothing clears
// keeps every unlinked tail segment's array alive through the pointer, so the
// entry costs memory for as long as the file lives.
func TestSegmentGoneClearsTheVacatedSlot(t *testing.T) {
	var fs files
	id := fid(1)
	for seg := core.SegmentID(1); seg <= 3; seg++ {
		noteRec(t, &fs, id, seg, ext(0, 8), nums(1, 1))
	}
	// Remove a segment that is not the last: slots 1 and 2 swap, and the
	// slot past the end must no longer hold the moved tail's array.
	_ = fs.update(id, func(f *file) error { f.segmentGone(1); return nil })
	_ = fs.view(id, func(f *file) {
		n := len(f.segs)
		if n != 2 {
			t.Fatalf("len(segs) = %d, want 2", n)
		}
		// The slot past the end is reachable through the backing array, of
		// which three appends leave room for four.
		vacated := f.segs[:cap(f.segs)][n]
		if vacated.seg != 0 || vacated.ranges != nil {
			t.Fatalf("the vacated slot holds segment %d with ranges %+v, want the zero entry", vacated.seg, vacated.ranges)
		}
	})
}

// TestRecordWithoutExtentKeepsTag: a record with no extent marks its segment
// only, so the tag outlives the change version, and it adds no range. A path
// that dropped the tag for having no extent would lose the file's owner
// while a record of it is still on disk.
func TestRecordWithoutExtentKeepsTag(t *testing.T) {
	var fs files
	id, seg := fid(1), core.SegmentID(3)
	_ = fs.update(id, func(f *file) error {
		if err := f.recorded(seg, core.Extent{}, nums(1, 1), 7, id); err != nil {
			return err
		}
		f.write(write(0, 8, rec(seg, 0), 5))
		return nil
	})
	err := fs.update(id, func(f *file) error {
		f.held.markOffloaded(ext(0, 8), ver(0), ver(5))
		_, err := f.held.release(ext(0, 8), nil)
		return err
	})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if !fs.view(id, func(*file) {}) {
		t.Fatal("entry dropped while a no-extent record is still on disk")
	}
	if got := fs.dropRangeCount(); got != 0 {
		t.Fatalf("gauge = %d from a no-extent record, want 0", got)
	}
	if rs, _ := rangesOf(&fs, id, seg); len(rs) != 0 {
		t.Fatalf("no-extent record added %+v, want no range", rs)
	}
	if fs.mayOutrank(id, 2, ext(0, 4), nums(2, 5)) {
		t.Fatal("a no-extent record keeps a removal")
	}
}

// TestToEndExtents: a truncate-shaped extent runs to MaxInt64 without
// overflowing, and a record near the top of the space overlaps it — in both
// directions of the query.
func TestToEndExtents(t *testing.T) {
	var fs files
	id := fid(1)
	toEnd := core.Extent{Off: 1 << 20, Len: math.MaxInt64 - 1<<20}
	noteRec(t, &fs, id, 1, toEnd, nums(3, 1))
	rs, _ := rangesOf(&fs, id, 1)
	if len(rs) != 1 || rs[0].start != toEnd.Off || rs[0].end != math.MaxInt64 {
		t.Fatalf("to-end range = %+v, want one [%d, MaxInt64)", rs, toEnd.Off)
	}
	noteRec(t, &fs, id, 2, core.Extent{Off: math.MaxInt64 - 8, Len: 8}, nums(1, 2))
	if !fs.mayOutrank(id, 1, toEnd, nums(5, 1)) {
		t.Fatal("a record near MaxInt64 failed to keep a to-end removal")
	}
	if !fs.mayOutrank(id, 1, core.Extent{Off: math.MaxInt64 - 8, Len: 8}, nums(5, 1)) {
		t.Fatal("a to-end query under the MaxInt64 record failed")
	}
}

// TestDropRangeGaugeAcrossEntryDrop: the gauge stays exact when an update
// drops the entry with its ranges, and when fn panics after noting.
func TestDropRangeGaugeAcrossEntryDrop(t *testing.T) {
	var fs files
	id := fid(1)
	noteRec(t, &fs, id, 1, ext(0, 8), nums(4, 1))
	before := fs.dropRangeCount()
	// The update that empties the file drops the entry whole, with its ranges.
	_ = fs.update(id, func(f *file) error { f.segmentGone(1); return nil })
	if present(&fs, id) {
		t.Fatal("entry kept with nothing held and no record on disk")
	}
	if got, want := fs.dropRangeCount(), before-1; got != want {
		t.Fatalf("gauge = %d after the entry dropped, want %d", got, want)
	}

	// A panic inside fn after noting still settles the gauge through the
	// deferred accounting, so the count never leaks a panicked note.
	noteRec(t, &fs, id, 1, ext(16, 8), nums(4, 1))
	func() {
		defer func() { _ = recover() }()
		_ = fs.update(id, func(f *file) error {
			_ = f.recorded(1, ext(0, 8), nums(2, 2), 1, id)
			panic("fn")
		})
	}()
	if got, want := fs.dropRangeCount(), int64(totalRanges(&fs)); got != want {
		t.Fatalf("gauge = %d after a panicked update, want %d", got, want)
	}
}

// TestDropRangesAfterMillionOverwrites checks the eight-range bound and the
// cost it promises under a random overwrite workload: one file of 1 GiB, 10^6
// overwrites of 4 KiB at 4 KiB-aligned offsets, spread over about sixteen
// segments. Whatever the order, every (file, segment) keeps at most eight
// ranges; a merge that only sometimes ran would let this count past eight.
func TestDropRangesAfterMillionOverwrites(t *testing.T) {
	if testing.Short() {
		t.Skip("a million overwrites are skipped in short runs")
	}
	var fs files
	id := fid(1)
	rng := rand.New(rand.NewPCG(104, 10))
	const block = 4096
	appended := int64(0)
	for i := int64(1); i <= 1_000_000; i++ {
		off := rng.Int64N((1<<30)/block) * block
		seg := core.SegmentID(1 + appended/core.SegmentCap)
		appended += block + 48 // payload and record header
		noteRec(t, &fs, id, seg, ext(off, block), nums(uint64(i), uint64(i)))
	}
	_ = fs.view(id, func(f *file) {
		for _, s := range f.segs {
			checkSegmentRanges(t, s.ranges, id, s.seg)
		}
	})
	if got, want := fs.dropRangeCount(), int64(totalRanges(&fs)); got != want {
		t.Fatalf("gauge = %d, want %d", got, want)
	}
}

// TestConcurrentDropRanges runs writers that note records with real extents on
// one file every writer shares and on one that comes and goes — its entry is
// dropped whenever all of its held bytes are released — while a reader runs
// drop queries on the same files and on one with no entry. Whatever the
// interleaving, the gauge must equal the sum when the writers stop.
func TestConcurrentDropRanges(t *testing.T) {
	var fs files
	shared, churn, absent := fid(1), fid(2), fid(3)
	const workers, rounds = 4, 200
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			off := int64(w)
			for r := range rounds {
				seg := core.SegmentID(w*rounds + r + 1)
				_ = fs.update(churn, func(f *file) error {
					err := f.recorded(seg, ext(off, 4), nums(1, uint64(seg)), 1, churn)
					f.held.fill(write(off, 4, rec(seg, 0), 1))
					return err
				})
				_ = fs.update(churn, func(f *file) error {
					_, err := f.held.release(ext(off, 4), nil)
					f.segmentGone(seg)
					return err
				})
				_ = fs.update(shared, func(f *file) error {
					return f.recorded(core.SegmentID(1+r%3), ext(int64(w*rounds+r)*4, 4), nums(1, uint64(r+1)), 1, shared)
				})
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range rounds {
			fs.view(shared, func(f *file) { f.mayOutrank(core.SegmentID(1), ext(0, 64), nums(2, 2)) })
			fs.mayOutrank(churn, core.SegmentID(1), ext(0, 64), nums(2, 2))
			fs.mayOutrank(absent, core.SegmentID(1), ext(0, 64), nums(2, 2))
		}
	}()
	wg.Wait()
	if got, want := fs.dropRangeCount(), int64(totalRanges(&fs)); got != want {
		t.Fatalf("gauge = %d, want %d", got, want)
	}
}

// TestNoteKeepsRangeArrayAtMaxDropRanges pins the pair's memory cost: whatever
// notes arrive, the backing array a (file, segment) holds has at most
// maxDropRanges slots, not the sixteen a grow-then-shrink insert leaves behind.
// Eight ranges is what the pair costs in memory, not only in length. The loop
// passes through every path that could grow it: the ninth disjoint note, the
// overlap of one range, a run of several collapsing, and further ninth notes
// once the pair is full again.
func TestNoteKeepsRangeArrayAtMaxDropRanges(t *testing.T) {
	var fs files
	id := fid(1)
	k := 0
	note := func(e core.Extent) {
		t.Helper()
		k++
		noteRec(t, &fs, id, 1, e, nums(uint64(k), uint64(k)))
		rs, ok := rangesOf(&fs, id, 1)
		if !ok || len(rs) > maxDropRanges {
			t.Fatalf("note %d: len(ranges) = %d (ok %v), want at most %d", k, len(rs), ok, maxDropRanges)
		}
		if cap(rs) > maxDropRanges {
			t.Fatalf("note %d (%+v): cap(ranges) = %d, want at most %d", k, e, cap(rs), maxDropRanges)
		}
	}
	for i := range int64(9) { // nine disjoint: the ninth forces a merge
		note(ext(i*100, 1))
	}
	// An overlap of one range, then a note spanning a run of several.
	note(ext(150, 60))
	note(ext(0, 500))
	// Disjoint notes until the pair is full again, then further ninth notes.
	for i := range int64(6) {
		note(ext(900+90*i, 4))
	}
}

// TestNinthMergesIntoItsClosestNeighbour pins that the ninth's own two gaps
// count as merge candidates and are compared with the old pairs' gaps
// together: when the space beside the arriving range is the smallest of the
// nine, the range merges with that neighbour rather than an old pair. Both
// sides must win on their own gap — a left-then-right search that compares
// only the left gap against the old ones merges the wrong side whenever the
// right gap is smaller — and on a tie between the two, the left, lower-offset
// one merges.
func TestNinthMergesIntoItsClosestNeighbour(t *testing.T) {
	// Eight ranges ten long at 0, 100, …, 700, carrying pairs 1..8: every gap
	// between old ranges is 90.
	fill := func(fs *files, id core.FileID) {
		for i := range int64(8) {
			noteRec(t, fs, id, 1, ext(i*100, 10), nums(uint64(i+1), uint64(i+1)))
		}
	}
	// untouched returns the ranges of the given slots, which the merge must not
	// have touched: neighbours are indexed by their slot above.
	untouched := func(t *testing.T, fs *files, id core.FileID, slots ...int64) {
		t.Helper()
		rs, _ := rangesOf(fs, id, 1)
		for _, i := range slots {
			if !slices.Contains(rs, dropRange{start: i * 100, end: i*100 + 10, low: nums(uint64(i+1), uint64(i+1))}) {
				t.Fatalf("range at %d was changed by the merge: %+v", i*100, rs)
			}
		}
	}

	t.Run("left gap smaller", func(t *testing.T) {
		var fs files
		id := fid(1)
		fill(&fs, id)
		// The ninth, [112, 120): a gap of 2 to [100, 110) on its left, 80 to
		// [200, 210) on its right — the left is the smallest of the nine.
		noteRec(t, &fs, id, 1, ext(112, 8), nums(3, 99))
		rs, _ := rangesOf(&fs, id, 1)
		if len(rs) != 8 {
			t.Fatalf("%d ranges after the ninth, want 8", len(rs))
		}
		// The merged neighbour keeps its own lower pair (2, 2).
		want := dropRange{start: 100, end: 120, low: nums(2, 2)}
		if !slices.Contains(rs, want) {
			t.Fatalf("the ninth did not merge with its closest neighbour: %+v not in %+v", want, rs)
		}
		if slices.ContainsFunc(rs, func(r dropRange) bool { return r.start == 112 }) {
			t.Fatalf("the ninth stayed its own range: %+v", rs)
		}
		untouched(t, &fs, id, 0, 2, 3, 4, 5, 6, 7)
	})

	t.Run("right gap smaller", func(t *testing.T) {
		var fs files
		id := fid(1)
		fill(&fs, id)
		// The ninth, [180, 198): a gap of 70 to [100, 110) on its left, 2 to
		// [200, 210) on its right, and 90 between the old pairs — the right
		// is the smallest of the nine. A left-first search that never reaches
		// the right gap merges left across 70 bytes instead.
		noteRec(t, &fs, id, 1, ext(180, 18), nums(3, 99))
		rs, _ := rangesOf(&fs, id, 1)
		if len(rs) != 8 {
			t.Fatalf("%d ranges after the ninth, want 8", len(rs))
		}
		// The merged neighbour's pair (3, 3) is the lower of (3, 3) and (3, 99).
		want := dropRange{start: 180, end: 210, low: nums(3, 3)}
		if !slices.Contains(rs, want) {
			t.Fatalf("the ninth did not merge with its closest neighbour: %+v not in %+v", want, rs)
		}
		if slices.ContainsFunc(rs, func(r dropRange) bool { return r.start == 100 && r.end > 110 }) {
			t.Fatalf("the ninth merged left across the larger gap: %+v", rs)
		}
		if slices.ContainsFunc(rs, func(r dropRange) bool { return r.start == 180 && r.end == 198 }) {
			t.Fatalf("the ninth stayed its own range: %+v", rs)
		}
		untouched(t, &fs, id, 0, 1, 3, 4, 5, 6, 7)
	})

	t.Run("left and right gaps tie", func(t *testing.T) {
		var fs files
		id := fid(1)
		fill(&fs, id)
		// The ninth, [150, 160): 40 to [100, 110) on its left and 40 to
		// [200, 210) on its right — the smallest of the nine, tied. The tie
		// takes the lower offsets, so it merges left.
		noteRec(t, &fs, id, 1, ext(150, 10), nums(3, 99))
		rs, _ := rangesOf(&fs, id, 1)
		if len(rs) != 8 {
			t.Fatalf("%d ranges after the ninth, want 8", len(rs))
		}
		want := dropRange{start: 100, end: 160, low: nums(2, 2)}
		if !slices.Contains(rs, want) {
			t.Fatalf("the tied ninth did not merge with its lower-offset neighbour: %+v not in %+v", want, rs)
		}
		if slices.ContainsFunc(rs, func(r dropRange) bool { return r.start == 150 && r.end == 160 }) {
			t.Fatalf("the ninth stayed its own range: %+v", rs)
		}
		if slices.ContainsFunc(rs, func(r dropRange) bool { return r.start < 210 && r.end > 210 && r.start > 160 }) {
			t.Fatalf("the tie merged right instead: %+v", rs)
		}
		untouched(t, &fs, id, 0, 3, 4, 5, 6, 7)
	})
}

// TestNoteIntoFullPairAllocatesNothing pins the note's steady-state cost: once
// a pair's eight ranges are kept, a note that overlaps them — shifting the run
// it collapses — and one that falls in a gap — bringing its merge partner down
// first — both work in the array the pair already holds. A per-note allocation
// would churn garbage on a full pair's random overwrites, which is where the
// drop ranges spend their life.
func TestNoteIntoFullPairAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not meaningful under -race")
	}
	pairs := make([]segRecords, 8)
	for p := range pairs {
		for i := range int64(8) {
			pairs[p].note(ext(i*8, 4), nums(uint64(i+1), uint64(i+1)))
		}
	}
	k := 0
	allocs := testing.AllocsPerRun(100, func() {
		p := &pairs[k%len(pairs)]
		// The bridge spans the pair's first two ranges on its first visit and
		// the merged one after; the far note lands in a gap past the last
		// range, so the pair stays full either way.
		p.note(ext(0, 9), nums(9, 9))
		p.note(ext(1000+8*int64(k), 4), nums(9, 10))
		k++
	})
	if allocs != 0 {
		t.Fatalf("a note into a full pair allocated %v times, want 0", allocs)
	}
}
