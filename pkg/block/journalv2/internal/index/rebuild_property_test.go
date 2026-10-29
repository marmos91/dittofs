package index

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// snapMarker is one removal marker by value, its clone spec copied, so two
// snapshots compare without a file's lock.
type snapMarker struct {
	extent  core.Extent
	version core.Version
	kind    core.ChangeKind
	clone   core.CloneSpec
	hasSpec bool
}

// fileSnap is one file's state, copied out under its read lock. ranges is
// captured only when the comparison wants it: a rebuild's drop ranges are
// sound and deterministic, but not equal to an incremental build's, so the
// incremental comparison leaves them out and checks them separately.
type fileSnap struct {
	held      []Piece
	retained  []Retained
	markers   []snapMarker
	change    core.Version
	hasChange bool
	tag       core.ShareTag
	tagged    bool
	rival     core.ShareTag
	rivalled  bool
	segs      []core.SegmentID
	ranges    map[core.SegmentID][]dropRange
	withRange bool
}

// indexSnap is a whole index copied out: every file with an entry, the entry
// count and the drop-range gauge.
type indexSnap struct {
	files  map[core.FileID]fileSnap
	count  int64
	drops  int64
	ranges bool
}

// snapshot copies the index out. withRanges captures each (file, segment)'s
// drop ranges too, for the determinism comparison, where every feed order
// must give the same ranges.
func snapshot(fs *files, withRanges bool) indexSnap {
	snap := indexSnap{
		files:  make(map[core.FileID]fileSnap),
		count:  fs.extentCount(),
		drops:  fs.dropRangeCount(),
		ranges: withRanges,
	}
	for i := range fs.shards {
		s := &fs.shards[i]
		s.mu.Lock()
		ids := make([]core.FileID, 0, len(s.m))
		for id := range s.m {
			ids = append(ids, id)
		}
		s.mu.Unlock()
		for _, id := range ids {
			fs.view(id, func(f *file) {
				fsnap := fileSnap{
					held:      f.held.pieces(nil),
					segs:      make([]core.SegmentID, 0, len(f.segs)),
					withRange: withRanges,
				}
				fsnap.retained = f.retained.list(fsnap.retained)
				for _, m := range f.markers {
					sm := snapMarker{extent: m.extent, version: m.version, kind: m.kind}
					if m.clone != nil {
						sm.clone, sm.hasSpec = *m.clone, true
					}
					fsnap.markers = append(fsnap.markers, sm)
				}
				fsnap.change, fsnap.hasChange = f.changeVersion()
				fsnap.tag, fsnap.tagged = f.owner()
				fsnap.rival, fsnap.rivalled = f.rival, f.rivalled
				if withRanges {
					fsnap.ranges = make(map[core.SegmentID][]dropRange)
					for _, s := range f.segs {
						fsnap.segs = append(fsnap.segs, s.seg)
						fsnap.ranges[s.seg] = slices.Clone(s.ranges)
					}
				} else {
					for _, s := range f.segs {
						fsnap.segs = append(fsnap.segs, s.seg)
					}
				}
				slices.Sort(fsnap.segs)
				snap.files[id] = fsnap
			})
		}
	}
	return snap
}

// diffSnap returns the first way two snapshots differ, the file and the
// field, or "" when they are equal. drops is compared only when both
// captured ranges, which is the comparison it belongs to.
func diffSnap(a, b indexSnap) string {
	if len(a.files) != len(b.files) {
		return fmt.Sprintf("the indexes hold %d and %d files", len(a.files), len(b.files))
	}
	for id, fa := range a.files {
		fb, ok := b.files[id]
		if !ok {
			return fmt.Sprintf("file %x in one index only", id.Bytes())
		}
		if d := diffFile(fmt.Sprintf("file %x", id.Bytes()), fa, fb); d != "" {
			return d
		}
	}
	for id := range b.files {
		if _, ok := a.files[id]; !ok {
			return fmt.Sprintf("file %x in one index only", id.Bytes())
		}
	}
	if a.count != b.count {
		return fmt.Sprintf("extentCount() = %d and %d", a.count, b.count)
	}
	if a.ranges && b.ranges && a.drops != b.drops {
		return fmt.Sprintf("dropRangeCount() = %d and %d", a.drops, b.drops)
	}
	return ""
}

// diffFile returns the first field two files' snapshots differ in.
func diffFile(name string, a, b fileSnap) string {
	k := func(field, format string, args ...any) string {
		return fmt.Sprintf("%s: %s = %s", name, field, fmt.Sprintf(format, args...))
	}
	if !slices.Equal(a.held, b.held) {
		return fmt.Sprintf("%s: held\n  %v\n  %v", name, a.held, b.held)
	}
	if !slices.Equal(a.retained, b.retained) {
		return fmt.Sprintf("%s: retained = %v and %v", name, a.retained, b.retained)
	}
	if !slices.Equal(a.markers, b.markers) {
		return fmt.Sprintf("%s: markers = %v and %v", name, a.markers, b.markers)
	}
	if a.change != b.change || a.hasChange != b.hasChange {
		return k("change version", "%v and %v (has: %v %v)", a.change, b.change, a.hasChange, b.hasChange)
	}
	if a.tag != b.tag || a.tagged != b.tagged {
		return k("tag", "%d and %d (tagged: %v %v)", a.tag, b.tag, a.tagged, b.tagged)
	}
	if a.rival != b.rival || a.rivalled != b.rivalled {
		return k("rival", "%d and %d (rivalled: %v %v)", a.rival, b.rival, a.rivalled, b.rivalled)
	}
	if !slices.Equal(a.segs, b.segs) {
		return fmt.Sprintf("%s: segments = %v and %v", name, a.segs, b.segs)
	}
	if a.withRange && b.withRange {
		if len(a.ranges) != len(b.ranges) {
			return k("range sets", "%d segments and %d", len(a.ranges), len(b.ranges))
		}
		for seg, ra := range a.ranges {
			rb, ok := b.ranges[seg]
			if !ok {
				return fmt.Sprintf("%s: segment %d noted in one index only", name, seg)
			}
			if !slices.Equal(ra, rb) {
				return fmt.Sprintf("%s: segment %d ranges = %v and %v", name, seg, ra, rb)
			}
		}
	}
	return ""
}

// history is one driven file-set: the reference index the runtime's own
// operations build, the records their appends would have logged, and the
// bookkeeping the generator needs to draw plausible next records.
type history struct {
	t     *testing.T
	rng   *rand.Rand
	fs    files
	ctr   Counters
	recs  []Record
	past  []core.Version // the versions seen, for fills and marks to draw
	segAt [genSegs + 1]uint32
	// pending, the most recent write, may be copied by a repack before
	// anything else touches it.
	pending *Record
	// removals are the removal records fed, for a carried-forward copy.
	removals []Record
	// debug, when set, prints every record fed and every op's effect on
	// file 0x0b. Temporary: only the debug test sets it.
	debug bool
	// cover tracks, per file and per 4 KiB cell of the generator's 256 KiB
	// world, the newest version any record has committed over that cell.
	// A fill draws its version from it: the remote tier's ref returns
	// content at least as new as the newest version committed for the
	// span, so a refill of a released or superseded span never carries an
	// older version than the content the journal wrote there. Without the
	// clamp the generator would invent fills the running engine cannot
	// produce — a fill older than the content a release record names,
	// which no precedence rule can rebuild identically to the runtime.
	cover [16][64]core.Version
}

// covered raises the span's cells to v, the version committed there. A
// removal runs to the end of the file; n reaching past the world clamps to
// its last cell.
func (h *history) covered(fi int, off, n int64, v core.Version) {
	lo := off / 4096
	hi := (off + n) / 4096
	if off+n > 256*1024 {
		hi = 64
	}
	for c := lo; c < hi; c++ {
		if h.cover[fi][c].Compare(v) < 0 {
			h.cover[fi][c] = v
		}
	}
}

// clampTo raises v to the span's newest committed version, the version the
// ref can return for it.
func (h *history) clampTo(fi int, off, n int64, v core.Version) core.Version {
	for c := off / 4096; c < (off+n)/4096; c++ {
		if h.cover[fi][c].Compare(v) > 0 {
			v = h.cover[fi][c]
		}
	}
	return v
}

// genSegs is the number of segments the generator draws record homes from.
const genSegs = 8

// randExt draws a file extent: 4 KiB granularity, within 256 KiB.
func (h *history) randExt() (int64, int64) {
	off := int64(h.rng.IntN(64)) * 4096
	n := int64(1+h.rng.IntN(4)) * 4096
	if off+n > 256*1024 {
		off = 256*1024 - n
	}
	return off, n
}

// place draws a record's (segment, offset in it), advancing the offset so no
// two records share a placement.
func (h *history) place() (core.SegmentID, uint32) {
	seg := core.SegmentID(1 + h.rng.IntN(genSegs))
	at := h.segAt[seg]
	h.segAt[seg] = at + 64
	return seg, at
}

// note "appends" a record: draws its place and numbers, notes it, and logs
// what the recovery scan would read back.
func (h *history) note(kind RecordKind, id core.FileID, v core.Version, e core.Extent) Record {
	seg, at := h.place()
	seq, err := h.ctr.NextSeq()
	if err != nil {
		h.t.Fatal(err)
	}
	r := Record{
		Kind: kind, File: id, Tag: 7,
		Segment: seg, At: at,
		Off: e.Off, Len: e.Len,
		Numbers: Numbers{Version: v, Seq: seq},
	}
	change(h.t, &h.fs, id, func(f *file) error {
		return f.recorded(seg, notedExtent(r), r.Numbers, 7, id)
	})
	h.recs = append(h.recs, r)
	return r
}

// notedExtent is the extent a record is noted over: the removal's own reach
// for the two to-end kinds, beside the header's extent otherwise. It mirrors
// the rebuild's convention, so the two sides note the same shapes.
func notedExtent(r Record) core.Extent {
	switch r.Kind {
	case RecTruncate:
		return toEnd(r.Off)
	case RecDelete:
		return toEnd(0)
	default:
		return core.Extent{Off: r.Off, Len: r.Len}
	}
}

// draw sorts a number as retention... no: draw issues the next version.
func (h *history) draw() core.Version {
	v, err := h.ctr.Draw()
	if err != nil {
		h.t.Fatal(err)
	}
	h.past = append(h.past, v)
	return v
}

// step drives one runtime operation and logs the record it appends. Every
// operation is one the running journal performs, logged as the record its
// append writes, so a rebuild of the log must equal the reference.
func (h *history) step() {
	ids := [16]core.FileID{}
	for i := range ids {
		ids[i] = fid(byte(i + 1))
	}
	fi := h.rng.IntN(len(ids))
	id := ids[fi]
	switch op := h.rng.IntN(11); op {
	case 0, 1, 2: // write
		off, n := h.randExt()
		v := h.draw()
		r := h.note(RecWrite, id, v, ext(off, n))
		change(h.t, &h.fs, id, func(f *file) error {
			f.write(Placed{Extent: ext(r.Off, r.Len), Record: RecordAt{Segment: r.Segment, Offset: r.At}, Version: v})
			return nil
		})
		h.covered(fi, off, n, v)
		rCopy := r
		h.pending = &rCopy

	case 3: // fill of the absent, unmasked parts of a span
		if len(h.past) == 0 {
			return
		}
		off, n := h.randExt()
		v := h.clampTo(fi, off, n, h.past[h.rng.IntN(len(h.past))])
		// Each absent, unmasked sub-extent gets one record of its own, whose
		// payload starts at its first byte, and one fill that holds exactly
		// it: what the runtime holds and what the rebuild replays must be
		// the same record.
		var fills []core.Extent
		change(h.t, &h.fs, id, func(f *file) error {
			f.fillParts(Placed{Extent: ext(off, n), Version: v}, func(part Placed) {
				var placed []Placed
				var missing []core.Extent
				_, missing = f.held.span(part.Extent, placed, missing)
				fills = append(fills, missing...)
			})
			return nil
		})
		for _, e := range fills {
			r := h.note(RecFill, id, v, e)
			change(h.t, &h.fs, id, func(f *file) error {
				f.fill(Placed{Extent: e, Record: RecordAt{Segment: r.Segment, Offset: r.At}, Version: v})
				return nil
			})
			h.covered(fi, e.Off, e.Len, v)
		}

	case 4: // offloaded report over a span
		off, n := h.randExt()
		if len(h.past) == 0 {
			return
		}
		v := h.past[h.rng.IntN(len(h.past))]
		_ = h.note(RecOffloaded, id, v, ext(off, n))
		change(h.t, &h.fs, id, func(f *file) error {
			f.held.markOffloaded(ext(off, n), core.Version{}, v)
			return nil
		})

	case 5: // unmark over a span
		if len(h.past) == 0 {
			return
		}
		off, n := h.randExt()
		v := h.past[h.rng.IntN(len(h.past))]
		_ = h.note(RecUnmark, id, v, ext(off, n))
		change(h.t, &h.fs, id, func(f *file) error {
			f.held.unmark(ext(off, n), v)
			return nil
		})

	case 6: // release of the fully offloaded pieces in a span
		off, n := h.randExt()
		var out []Piece
		change(h.t, &h.fs, id, func(f *file) error {
			f.held.visit(ext(off, n), func(p Piece) bool {
				if p.Offloaded && p.Extent.Off >= off && p.Extent.Off+p.Extent.Len <= off+n {
					out = append(out, p)
				}
				return true
			})
			return nil
		})
		for _, p := range out {
			r := h.note(RecRelease, id, p.Version, p.Extent)
			if h.debug {
				fmt.Printf("release of piece %v v=%d bits=%v\n", p.Extent, p.Version.Counter, p.Offloaded)
			}
			change(h.t, &h.fs, id, func(f *file) error {
				_, err := f.held.release(p.Extent, nil)
				return err
			})
			_ = r
			h.covered(fi, p.Extent.Off, p.Extent.Len, p.Version)
			h.pending = nil
		}

	case 7: // truncate
		v := h.draw()
		size := int64(h.rng.IntN(64)) * 4096
		r := h.note(RecTruncate, id, v, ext(size, 0))
		change(h.t, &h.fs, id, func(f *file) error { f.truncate(size, v); return nil })
		h.covered(fi, size, 256*1024-size, v)
		h.removals = append(h.removals, r)
		h.pending = nil

	case 8: // deallocate
		off, n := h.randExt()
		v := h.draw()
		r := h.note(RecDeallocate, id, v, ext(off, n))
		change(h.t, &h.fs, id, func(f *file) error { return f.deallocate(ext(off, n), v) })
		h.covered(fi, off, n, v)
		h.removals = append(h.removals, r)
		h.pending = nil

	case 9: // delete
		v := h.draw()
		r := h.note(RecDelete, id, v, ext(0, 0))
		change(h.t, &h.fs, id, func(f *file) error { f.delete(v); return nil })
		h.covered(fi, 0, 256*1024, v)
		h.removals = append(h.removals, r)
		h.pending = nil

	case 10: // clone target, then sometimes a repack copy of the last write
		v := h.draw()
		off, n := h.randExt()
		spec := core.CloneSpec{Src: fid(byte(1 + h.rng.IntN(9))), SrcOff: 4096, DstOff: off, Len: n, AsOf: v}
		seg, at := h.place()
		seq, err := h.ctr.NextSeq()
		if err != nil {
			h.t.Fatal(err)
		}
		r := Record{Kind: RecCloneTarget, File: id, Tag: 7, Segment: seg, At: at,
			Off: off, Len: n, Numbers: Numbers{Version: v, Seq: seq}, Clone: &spec}
		change(h.t, &h.fs, id, func(f *file) error {
			return f.recorded(seg, notedExtent(r), r.Numbers, 7, id)
		})
		change(h.t, &h.fs, id, func(f *file) error { return f.cloneTarget(spec, v) })
		h.covered(fi, off, n, v)
		h.recs = append(h.recs, r)
		h.removals = append(h.removals, r)
		h.pending = nil
	}

	// Two late operations, each after its own main one: a repack copy of
	// the last write, and a carried-forward removal.
	switch h.rng.IntN(3) {
	case 0:
		// Repack copies the last write's own record: the copy is noted on
		// the file that wrote it, so a pending write of another file is
		// not copied onto this step's file. Nothing between the write and
		// its copy can have touched the piece — every other op clears
		// pending or fills and marks only.
		if h.pending == nil || h.pending.File != id {
			return
		}
		from := RecordAt{Segment: h.pending.Segment, Offset: h.pending.At}
		v := h.pending.Numbers.Version
		r := h.note(RecWrite, id, v, ext(h.pending.Off, h.pending.Len))
		best := *h.pending
		change(h.t, &h.fs, id, func(f *file) error {
			f.held.repoint(ext(best.Off, best.Len), from, RecordAt{Segment: r.Segment, Offset: r.At}, 0)
			return nil
		})
		h.pending = nil
	case 1:
		if len(h.removals) == 0 {
			return
		}
		origin := h.removals[h.rng.IntN(len(h.removals))]
		// the copy keeps the ORIGIN file's index so `covered` lands right
		originFi := int(origin.File.Bytes()[15] - 1)
		seg, at := h.place()
		change(h.t, &h.fs, origin.File, func(f *file) error {
			return f.recorded(seg, notedExtent(origin), origin.Numbers, origin.Tag, origin.File)
		})
		h.covered(originFi, origin.Off, origin.Len, origin.Numbers.Version)
		h.recs = append(h.recs, Record{Kind: origin.Kind, File: origin.File, Tag: origin.Tag,
			Segment: seg, At: at, Off: origin.Off, Len: origin.Len,
			Numbers: origin.Numbers, Clone: origin.Clone})
	}
}

// drive builds a history of ops operations over 16 files.
func drive(t *testing.T, seed uint64, ops int) *history {
	h := &history{t: t, rng: rand.New(rand.NewPCG(seed, 3375))}
	h.fs.openBuckets(ver(0))
	_ = h.ctr.SetVersionLimit(ver(maxVersion))
	for range ops {
		h.step()
	}
	return h
}

// rebuild feeds the history's records to a fresh index, in fed order.
func rebuild(t *testing.T, order []Record, floor core.Version) *files {
	t.Helper()
	fs := &files{}
	var ctr Counters
	b := newRebuilder(fs, &ctr)
	for _, r := range order {
		if err := b.Feed(r); err != nil {
			t.Fatalf("Feed(kind %d, seq %d) = %v", r.Kind, r.Numbers.Seq, err)
		}
	}
	if err := b.Finish(floor); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return fs
}

// TestRebuildShuffledIdentical: the rebuild's result — held pieces, markers,
// tags, segments and drop ranges alike — depends only on the records, not on
// the order they were fed in. A rebuild that sorted by feed order, or noted
// by feed order, gives different ranges or state to two shuffles.
func TestRebuildShuffledIdentical(t *testing.T) {
	h := drive(t, 1, 120)
	orders := make([][]Record, 0, 53)
	shuffled := slices.Clone(h.recs)
	orders = append(orders, slices.Clone(h.recs), slices.Clone(h.recs))
	slices.Reverse(orders[1])
	rng := rand.New(rand.NewPCG(2, 3375))
	for range 50 {
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		orders = append(orders, slices.Clone(shuffled))
	}
	want := snapshot(rebuild(t, orders[0], ver(0)), true)
	for i := 1; i < len(orders); i++ {
		fs := rebuild(t, orders[i], ver(0))
		if d := diffSnap(snapshot(fs, true), want); d != "" {
			t.Fatalf("order %d differs: %s", i, d)
		}
		// The per-segment counters equal the walk after every rebuild too,
		// whatever order the feeders raced in.
		var segs []core.SegmentID
		for s := range genSegs {
			segs = append(segs, core.SegmentID(s+1))
		}
		checkSegments(t, i, fs, segs...)
	}
}

// TestRebuildIncrementalEqualsRebuild is the core property: an index built
// incrementally by the runtime's own operations, a record logged per
// append, is identical to the rebuild of the log fed in a shuffled order —
// held pieces, retained pieces, markers with their clone specs, change
// versions, tags, segments and the entry count. The drop ranges are checked
// for soundness only: every record is covered in its segment by a range
// whose pair is at or below its, never more than eight per (file, segment),
// sorted and disjoint, and the gauge counts them all — a rebuild's ranges
// can differ from an incremental build's where the ninth-range merge chose
// another pair, since a carried-forward record is noted twice, at different
// times.
func TestRebuildIncrementalEqualsRebuild(t *testing.T) {
	for seed := range uint64(1000) {
		h := drive(t, seed+1, 20+int(rand.New(rand.NewPCG(seed, 7)).IntN(180)))
		order := slices.Clone(h.recs)
		rand.New(rand.NewPCG(seed, 11)).Shuffle(len(order), func(i, j int) {
			order[i], order[j] = order[j], order[i]
		})
		fs := rebuild(t, order, ver(0))
		if d := diffSnap(snapshot(fs, false), snapshot(&h.fs, false)); d != "" {
			t.Fatalf("seed %d: %s", seed, d)
		}
		checkRebuildAccounting(t, int(seed), fs, h.recs)
	}
}

// checkRebuildAccounting asserts, for one rebuild, that the per-segment
// counters equal a recomputed walk, that the drop ranges are sound and
// disjoint within their cap, and that the gauge counts exactly them.
func checkRebuildAccounting(t *testing.T, step int, fs *files, recs []Record) {
	t.Helper()
	var segs []core.SegmentID
	for i := range genSegs {
		segs = append(segs, core.SegmentID(i+1))
	}
	// The counters equal the walk, so the rebuilt accounting is exact.
	checkSegments(t, step, fs, segs...)
	// Every file's ranges are sorted and disjoint, none over the cap, and
	// the gauge counts them all.
	var sum int64
	for i := range fs.shards {
		s := &fs.shards[i]
		s.mu.Lock()
		entries := make([]*file, 0, len(s.m))
		for _, f := range s.m {
			entries = append(entries, f)
		}
		s.mu.Unlock()
		for _, f := range entries {
			f.mu.RLock()
			for _, sr := range f.segs {
				sum += int64(len(sr.ranges))
				lo := int64(0)
				for i, r := range sr.ranges {
					if i > 0 && (r.start < lo || sr.ranges[i-1].end > r.start) {
						t.Fatalf("step %d: segment %d ranges out of order or overlapping: %+v", step, sr.seg, sr.ranges)
					}
					lo = max(lo, r.end)
				}
				if len(sr.ranges) > maxDropRanges {
					t.Fatalf("step %d: segment %d keeps %d ranges", step, sr.seg, len(sr.ranges))
				}
			}
			f.mu.RUnlock()
		}
	}
	if got := fs.dropRangeCount(); got != sum {
		t.Fatalf("step %d: dropRangeCount() = %d, want %d", step, got, sum)
	}
	// Soundness: every fed record is covered in its own segment by a range
	// whose pair is at or below its own.
	for _, r := range recs {
		ok := fs.view(r.File, func(f *file) {
			for _, sr := range f.segs {
				if sr.seg != r.Segment {
					continue
				}
				for _, d := range sr.ranges {
					lo, hi := r.Off, r.Off+absLen(r)
					if r.Kind == RecTruncate || r.Kind == RecDelete {
						hi = math.MaxInt64
					}
					if d.start <= lo && d.end >= hi && d.low.Compare(r.Numbers) <= 0 {
						return
					}
				}
			}
			t.Fatalf("step %d: record kind %d at [%d, %d) in segment %d under-covered", step, r.Kind, r.Off, r.Off+r.Len, r.Segment)
		})
		if !ok {
			t.Fatalf("step %d: record of file with no entry after the rebuild", step)
		}
	}
}

// absLen is a fed record's length, the to-end removals excepted.
func absLen(r Record) int64 { return max(r.Len, 1) }
