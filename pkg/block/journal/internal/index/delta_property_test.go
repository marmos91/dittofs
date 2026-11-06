package index

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// deltaFile is one file of a deltaWorld: its owner, the retaining writes not
// yet lifted, and the running sums of the deltas its updates returned.
type deltaFile struct {
	id     core.FileID
	tag    core.ShareTag
	window []WindowRecord
	sum    heldTotals
	onList bool
}

// deltaWorld drives random mutations at one index, drawing every record's
// home from a slot allocator as ModelIndex.place does, and keeps what each
// returned delta says. Its checks are the property under test: a delta is
// what its update changed and nothing else.
type deltaWorld struct {
	t    *testing.T
	fs   *files
	rng  *rand.Rand
	seed uint64
	base core.SegmentID // the segment before this world's first
	last core.SegmentID // highest segment placed
	// slots, seq and version are this world's counters; worlds sharing an
	// index take disjoint segment ranges, and a file belongs to one world.
	slots   uint32
	seq     uint64
	version uint64
	step    int
	// byTag is the running sum of every delta this world saw, by the tag the
	// delta carried.
	byTag map[core.ShareTag]heldTotals
	// fail reports a violated check and stops the goroutine running it:
	// t.Fatalf by default, which a world run off the test goroutine replaces.
	fail func(string)
}

func (w *deltaWorld) place() RecordAt {
	r := RecordAt{Segment: w.base + 1 + core.SegmentID(w.slots/16), Offset: (w.slots % 16) * 4096}
	w.slots++
	w.last = max(w.last, r.Segment)
	return r
}

// owned reports whether the index holds an entry of f's that a tag owns. An
// update that leaves a file holding nothing drops the entry and its owner with
// it, so it is asked of the index rather than remembered.
func (w *deltaWorld) owned(f *deltaFile) bool {
	var tagged bool
	w.fs.view(f.id, func(x *file) { tagged = x.tagged })
	return tagged
}

// add adds d to the sums by the tag d carried.
func (w *deltaWorld) add(d HeldDelta) {
	if w.byTag == nil {
		w.byTag = map[core.ShareTag]heldTotals{}
	}
	t := w.byTag[d.Tag]
	t.held += d.HeldBytes
	t.dirty += d.DirtyBytes
	t.retained += d.RetainedBytes
	w.byTag[d.Tag] = t
}

func (w *deltaWorld) next() uint64 { w.version++; return w.version }

func (w *deltaWorld) extent() core.Extent {
	if w.rng.IntN(4) == 0 {
		return whole
	}
	return ext(int64(w.rng.IntN(4096)), 1+int64(w.rng.IntN(2048)))
}

func (w *deltaWorld) fatalf(f *deltaFile, format string, args ...any) {
	w.t.Helper()
	msg := fmt.Sprintf("seed %d step %d file %v: "+format, append([]any{w.seed, w.step, f.id}, args...)...)
	if w.fail != nil {
		w.fail(msg)
	}
	w.t.Fatal(msg)
}

// through runs fn in one update on f after noting a record under tag, as every
// journal call does, and returns the update's delta.
func (w *deltaWorld) through(f *deltaFile, tag core.ShareTag, fn func(*file) error) (HeldDelta, error) {
	r := w.place()
	w.seq++
	n := Numbers{Version: ver(w.version), Seq: core.Seq(w.seq)}
	return w.fs.update(f.id, func(x *file) error {
		if err := x.recorded(r.Segment, ext(0, 1), n, tag, f.id); err != nil {
			return err
		}
		return fn(x)
	})
}

// kinds of operation, each a mutator of the table test.
const (
	opWrite = iota
	opWriteRetain
	opFill
	opTruncate
	opDeallocate
	opDelete
	opClone
	opRelease
	opMark
	opUnmark
	opLift
	opFailWindow
	opRepoint
	opRival
)

// weighted is the op mix: the common ones appear more than once so held,
// offloaded and retained content all build up between the removals.
var weighted = []int{
	opWrite, opWrite, opWrite, opWrite, opWriteRetain, opWriteRetain, opWriteRetain,
	opFill, opFill, opTruncate, opDeallocate, opDeallocate, opDelete, opClone,
	opRelease, opRelease, opRelease, opMark, opMark, opMark, opMark, opUnmark, opUnmark,
	opLift, opLift, opFailWindow, opFailWindow, opRepoint, opRival,
}

// run does one operation of kind on f and checks its delta, pinning the
// refusals the operation expects.
func (w *deltaWorld) run(f *deltaFile, kind int) {
	t := w.t
	t.Helper()
	var d HeldDelta
	var err error
	wantTag := f.tag
	switch kind {
	case opWrite, opWriteRetain:
		e, r, v := w.extent(), w.place(), w.next()
		if e == whole {
			e = ext(0, 1+int64(w.rng.IntN(4096)))
		}
		p := Placed{Extent: e, Record: r, Version: ver(v)}
		d, err = w.through(f, f.tag, func(x *file) error {
			if kind == opWrite {
				x.write(p)
			} else {
				x.writeRetain(p)
			}
			return nil
		})
		if kind == opWriteRetain && err == nil {
			f.window = append(f.window, WindowRecord{Record: r, Version: ver(v)})
		}
	case opFill:
		e, r, v := w.extent(), w.place(), w.rng.Uint64()%(w.version+1)
		if e == whole {
			e = ext(0, 1+int64(w.rng.IntN(4096)))
		}
		d, err = w.fs.fill(f.tag, f.id, Placed{Extent: e, Record: r, Version: ver(v)}, w.fs.asOf(f.id))
		if err != nil {
			w.fatalf(f, "fill refused: %v", err)
		}
		w.check(f, d, err, wantTag)
		w.seq++
		d, err = w.fs.update(f.id, func(x *file) error {
			return x.recorded(r.Segment, e, Numbers{Version: ver(v), Seq: core.Seq(w.seq)}, f.tag, f.id)
		})
		if d != (HeldDelta{}) {
			w.fatalf(f, "the claim after a fill returned %+v, want the zero delta", d)
		}
		wantTag = f.tag
	case opTruncate:
		size, v := int64(w.rng.IntN(4096)), ver(w.next())
		d, err = w.through(f, f.tag, func(x *file) error { x.truncate(size, v); return nil })
	case opDeallocate:
		e, v := w.extent(), ver(w.next())
		d, err = w.through(f, f.tag, func(x *file) error { return x.deallocate(e, v) })
	case opDelete:
		v := ver(w.next())
		d, err = w.through(f, f.tag, func(x *file) error { x.delete(v); return nil })
	case opClone:
		spec, v := core.CloneSpec{DstOff: int64(w.rng.IntN(4096)), Len: 1 + int64(w.rng.IntN(2048))}, ver(w.next())
		d, err = w.through(f, f.tag, func(x *file) error { return x.cloneTarget(spec, v) })
	case opRelease:
		e := w.extent()
		d, err = w.through(f, f.tag, func(x *file) error { _, err := x.held.release(e, nil); return err })
		if err != nil && !errors.Is(err, ErrNotOffloaded) {
			w.fatalf(f, "release: %v", err)
		}
		if err != nil && d != (HeldDelta{}) {
			w.fatalf(f, "a refused release returned %+v", d)
		}
		err = nil
	case opMark:
		e := w.extent()
		d, err = w.through(f, f.tag, func(x *file) error { x.held.markOffloaded(e, ver(0), ver(w.version)); return nil })
	case opUnmark:
		e, v := w.extent(), ver(w.rng.Uint64()%(w.version+1))
		d, err = w.through(f, f.tag, func(x *file) error { x.held.unmark(e, v); return nil })
	case opLift:
		var bys []core.Version
		w.fs.view(f.id, func(x *file) {
			for _, r := range x.retained.list(nil) {
				bys = append(bys, r.By)
			}
		})
		if len(bys) == 0 {
			return
		}
		by := bys[w.rng.IntN(len(bys))]
		d, err = w.through(f, f.tag, func(x *file) error { x.retained.drop(whole, bySuperseder(by), nil); return nil })
		f.window = filterWindow(f.window, func(r WindowRecord) bool { return r.Version != by })
	case opFailWindow:
		if len(f.window) == 0 {
			return
		}
		k := 1 + w.rng.IntN(len(f.window))
		fail := append([]WindowRecord(nil), f.window[len(f.window)-k:]...)
		w.rng.Shuffle(len(fail), func(i, j int) { fail[i], fail[j] = fail[j], fail[i] })
		f.window = f.window[:len(f.window)-k]
		d, err = w.through(f, f.tag, func(x *file) error { x.failWindow(fail, nil); return nil })
	case opRepoint:
		var from RecordAt
		var found bool
		for _, e := range w.fs.inspect(f.id) {
			if e.Kind == EntryHeld {
				from, found = e.Record, true
				break
			}
		}
		if !found {
			return
		}
		to, e := w.place(), w.extent()
		d, err = w.through(f, f.tag, func(x *file) error { x.held.repoint(e, from, to, 0); x.retained.repoint(e, from, to, 0); return nil })
	case opRival:
		if !w.owned(f) {
			return
		}
		// A refused call notes no record: recorded under the rival's tag would
		// mark the file rivalled and refuse its owner from then on.
		d, err = w.fs.update(f.id, func(x *file) error { return x.claim(f.tag+1, f.id) })
		var other *core.OtherShareError
		if !errors.As(err, &other) || d != (HeldDelta{}) {
			w.fatalf(f, "a rival's claim = (%+v, %v), want the zero delta and an OtherShareError", d, err)
		}
		return
	}
	w.check(f, d, err, wantTag)
}

func filterWindow(ws []WindowRecord, keep func(WindowRecord) bool) []WindowRecord {
	out := ws[:0]
	for _, r := range ws {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

// check adds d to f's running sums, holds the transition to the list it
// implies, and compares the sums with a read-out walk of the file.
func (w *deltaWorld) check(f *deltaFile, d HeldDelta, err error, wantTag core.ShareTag) {
	w.t.Helper()
	if err != nil {
		w.fatalf(f, "update failed: %v", err)
	}
	if d != (HeldDelta{}) && (d.File != f.id || d.Tag != wantTag) {
		w.fatalf(f, "delta %+v names file %v tag %v, want this file under tag %v", d, d.File, d.Tag, wantTag)
	}
	w.add(d)
	f.sum.held += d.HeldBytes
	f.sum.dirty += d.DirtyBytes
	f.sum.retained += d.RetainedBytes
	switch d.Dirty {
	case BecameDirty:
		if f.onList {
			w.fatalf(f, "BecameDirty for a file already on the dirty list: %+v", d)
		}
		f.onList = true
	case BecameClean:
		if !f.onList {
			w.fatalf(f, "BecameClean for a file not on the dirty list: %+v", d)
		}
		f.onList = false
	}
	got := walkTotals(w.fs, f.id)
	if want := (fileTotals{f.sum.held, f.sum.dirty, f.sum.retained, f.onList}); got != want {
		w.fatalf(f, "the deltas sum to %+v, the read-out says %+v (last delta %+v)", want, got, d)
	}
}

// TestDeltaSumMatchesCounters applies a random mutation sequence over four
// files and two tags, and after every operation compares the running sum of
// the deltas with the counters read back: per file from the read-out, over
// all files from the per-segment counters and the dirty-file list.
func TestDeltaSumMatchesCounters(t *testing.T) {
	seeds := 200
	if testing.Short() {
		seeds = 20
	}
	for seed := uint64(1); seed <= uint64(seeds); seed++ {
		fs := &files{}
		w := &deltaWorld{t: t, fs: fs, rng: rand.New(rand.NewPCG(seed, 3796)), seed: seed}
		var fl []*deltaFile
		for i := range byte(4) {
			fl = append(fl, &deltaFile{id: fid(i + 1), tag: core.ShareTag(1 + i/2*2)})
		}
		for w.step = 0; w.step < 300; w.step++ {
			f := fl[w.rng.IntN(len(fl))]
			w.run(f, weighted[w.rng.IntN(len(weighted))])

			var sum heldTotals
			dirtyFiles := map[core.FileID]bool{}
			for _, x := range fl {
				sum.held += x.sum.held
				sum.dirty += x.sum.dirty
				sum.retained += x.sum.retained
				if x.onList {
					dirtyFiles[x.id] = true
				}
			}
			if err := checkDeltaSums(fs, w.last, sum, w.byTag, dirtyFiles); err != nil {
				t.Fatalf("seed %d step %d: %v", seed, w.step, err)
			}
		}
	}
}
