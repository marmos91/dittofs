//go:build journal_conformance

package conformance

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
	"github.com/marmos91/dittofs/pkg/sim"
)

// The checks here fail by leaving storage, pins or index entries the journal
// cannot give back, or a call that never returns.

// callWait bounds, in real time, how long a check waits for one journal call
// that must not block: a removal or repack beside a parked offer, a repack
// beside a stream of readers, any call of a concurrent run.
//
// decision: a call blocked on a lock reports nothing to the seam or the
// clock, so only a real-time wait sees it never return; a correct journal
// returns in milliseconds. The ceiling: a call slower than this on a loaded
// runner fails without being wrong. Overturned once the journal reports the
// waits it is in, when a check can see a call waiting instead of timing it.
const callWait = 10 * time.Second

// within runs call on a goroutine of its own and returns its error. If call
// has not returned within callWait, it abandons the fixture's journal and
// fails the test, since closing the journal at cleanup would wait for the
// blocked call and hang the run instead.
func within(t *testing.T, f *journalv2test.Fixture, what string, call func() error) error {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		return err
	case <-time.After(callWait):
		f.Abandon()
		t.Fatalf("%s did not return within %v", what, callWait)
		return nil
	}
}

// drawVersion draws a content version from the journal, for a fill.
func drawVersion(t *testing.T, f *journalv2test.Fixture) journalv2.Version {
	t.Helper()

	v, err := f.J.Draw()
	stub(t, "Draw", err)
	if err != nil {
		t.Fatalf("draw a version: %v", err)
	}
	return v
}

// fillRange fills p at off of file id as content of version v, reading the
// file's change version first as a caller that found the range missing does.
func fillRange(t *testing.T, h *journalv2.Handle, id journalv2.FileID, off int64, p []byte, v journalv2.Version) {
	t.Helper()

	_, _, _, asOf, err := h.ReadAt(t.Context(), id, off, make([]byte, len(p)))
	stub(t, "ReadAt", err)
	if err != nil {
		t.Fatalf("read before a fill: %v", err)
	}
	err = journalv2test.RetrySpare(func() error { return h.Fill(t.Context(), id, off, p, asOf, v) })
	stub(t, "Fill", err)
	if err != nil {
		t.Fatalf("fill %d bytes at %d: %v", len(p), off, err)
	}
}

// freeExtent releases extent e of file id, which must succeed.
func freeExtent(t *testing.T, h *journalv2.Handle, id journalv2.FileID, e journalv2.Extent) {
	t.Helper()

	err := h.Release(t.Context(), id, []journalv2.Extent{e})
	stub(t, "Release", err)
	if err != nil {
		t.Fatalf("release %+v: %v", e, err)
	}
}

// readBack reads n bytes at off of file id and returns them with what the
// read reported missing.
func readBack(t *testing.T, h *journalv2.Handle, id journalv2.FileID, off, n int64) ([]byte, []journalv2.Extent) {
	t.Helper()

	buf := make([]byte, n)
	_, missing, _, _, err := h.ReadAt(t.Context(), id, off, buf)
	stub(t, "ReadAt", err)
	if err != nil {
		t.Fatalf("read %d bytes at %d: %v", n, off, err)
	}
	return buf, missing
}

// indexEntries returns the index entries of file id.
func indexEntries(t *testing.T, f *journalv2test.Fixture, id journalv2.FileID) []journalv2.IndexEntry {
	t.Helper()

	es, err := f.J.Inspect().File(id)
	stub(t, "Inspect.File", err)
	if err != nil {
		t.Fatalf("inspect a file: %v", err)
	}
	return es
}

// segmentOf returns the segment backing the held byte at off of file id.
func segmentOf(t *testing.T, f *journalv2test.Fixture, id journalv2.FileID, off int64) journalv2.SegmentID {
	t.Helper()

	for _, e := range indexEntries(t, f, id) {
		if e.Kind == journalv2.EntryHeld && e.Extent.Off <= off && off < e.Extent.Off+e.Extent.Len {
			return e.Location.Segment
		}
	}
	t.Fatalf("byte %d of the file is not held", off)
	return 0
}

// sealNow lets the journal's clock pass the idle-seal age, and returns once
// segment seg, past the idle-seal size, is sealed.
func sealNow(t *testing.T, f *journalv2test.Fixture, seg journalv2.SegmentID) {
	t.Helper()

	f.Advance(2 * journalv2test.IdleSealAge)
	eventually(t, fmt.Sprintf("segment %d was not sealed once idle", seg), func() bool {
		s, ok := segmentInfo(f, seg)
		return ok && s.Sealed
	})
}

// wantPieces fails the test unless the held entries of es over within cover
// it whole, end to end, and no two of them that touch, at one version and one
// offloaded bit, lie in one segment: one piece of a record is one entry, so
// such a pair is a piece cut in two, or two records where one was owed. It
// returns how many entries cover within.
//
// Entries of different segments, or of different versions or bits, may touch:
// a record does not span segments, and only content alike is coalesced.
func wantPieces(t *testing.T, es []journalv2.IndexEntry, within journalv2.Extent, what string) int {
	t.Helper()

	var in []journalv2.IndexEntry
	for _, e := range es {
		if e.Kind == journalv2.EntryHeld && e.Extent.Off < within.Off+within.Len && within.Off < e.Extent.Off+e.Extent.Len {
			in = append(in, e)
		}
	}
	slices.SortFunc(in, func(a, b journalv2.IndexEntry) int { return cmp.Compare(a.Extent.Off, b.Extent.Off) })
	end := within.Off
	for i, e := range in {
		if (i == 0 && e.Extent.Off > end) || (i > 0 && e.Extent.Off != end) {
			t.Fatalf("%s: the held entries over %+v do not run end to end at %d: %+v", what, within, end, in)
		}
		end = e.Extent.Off + e.Extent.Len
		if i == 0 {
			continue
		}
		if p := in[i-1]; p.Version == e.Version && p.Offloaded == e.Offloaded && p.Location.Segment == e.Location.Segment {
			t.Fatalf("%s: entries %+v and %+v touch in segment %d at one version and bit, want them one entry", what, p.Extent, e.Extent, e.Location.Segment)
		}
	}
	if end < within.Off+within.Len {
		t.Fatalf("%s: the held entries over %+v end at %d: %+v", what, within, end, in)
	}
	return len(in)
}

// parkWait bounds, in real time, how long a stalled offer callback stays
// parked if the check never lets it go.
//
// decision: every journal call a check makes beside a parked callback is
// bounded by callWait on its own, so the callback is not what ends a call
// that waits for it; parkWait only stops a call the check did not bound, such
// as a read of Stats, from hanging the run. It is far above callWait so a
// sequence of calls beside the callback, each within callWait, never outlasts
// it. The ceiling: a check whose calls beside one parked callback take longer
// than this in all fails without being wrong. Overturned as callWait is.
const parkWait = 10 * callWait

// stall parks offer callbacks as a slow upload would, until the check lets
// them go. A callback parked for parkWait lets itself go and records that it
// did, so a journal call that waits for it returns, late, instead of hanging
// the run, and the check reads the lateness as the failure.
type stall struct {
	reached chan struct{} // closed once the first callback parks
	release chan struct{}
	waiting atomic.Int32
	expired atomic.Bool
	first   sync.Once
	let     sync.Once
}

// newStall returns a stall that lets every callback go when the test ends.
func newStall(t *testing.T) *stall {
	s := &stall{reached: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(s.letGo)
	return s
}

// park blocks a callback until letGo, or until parkWait has passed.
func (s *stall) park() {
	s.waiting.Add(1)
	s.first.Do(func() { close(s.reached) })
	select {
	case <-s.release:
	case <-time.After(parkWait):
		s.expired.Store(true)
	}
}

// letGo frees every parked callback, and every later one at once.
func (s *stall) letGo() { s.let.Do(func() { close(s.release) }) }

// wantParked fails the test if a callback gave up waiting: what, done while
// it was parked, waited for it.
func (s *stall) wantParked(t *testing.T, what string) {
	t.Helper()

	if s.expired.Load() {
		t.Fatalf("%s waited for the parked offer callback, which let itself go after %v", what, parkWait)
	}
}

// waitParked returns once n callbacks are parked at s, and fails the test if
// an offer returns first or they are not all parked within callWait, then
// abandoning the fixture's journal, whose offers may be blocked short of their
// callbacks.
func waitParked(t *testing.T, f *journalv2test.Fixture, s *stall, n int32, offers <-chan error) {
	t.Helper()

	deadline := time.Now().Add(callWait)
	for s.waiting.Load() < n {
		select {
		case err := <-offers:
			wantOffer(t, err)
			t.Fatalf("an offer returned before its callback parked, with %d of %d parked", s.waiting.Load(), n)
		default:
		}
		if time.Now().After(deadline) {
			f.Abandon()
			t.Fatalf("%d of %d offer callbacks parked within %v", s.waiting.Load(), n, callWait)
		}
		time.Sleep(time.Millisecond)
	}
}

// wantOffer fails the test on err, an offload's error, naming the journal
// call that failed when the callback's error wraps a callError.
func wantOffer(t *testing.T, err error) {
	t.Helper()

	var ce *callError
	if errors.As(err, &ce) {
		stub(t, ce.call, ce.err)
	}
	stub(t, "Offload", err)
	if err != nil {
		t.Fatalf("offload: %v", err)
	}
}

// joinOffers waits for n offers to return on done, each within callWait, and
// fails the test on any error. An offer that does not return abandons the
// fixture's journal.
func joinOffers(t *testing.T, f *journalv2test.Fixture, done <-chan error, n int) {
	t.Helper()

	for range n {
		select {
		case err := <-done:
			wantOffer(t, err)
		case <-time.After(callWait):
			f.Abandon()
			t.Fatalf("an offer did not return within %v of its callback being let go", callWait)
		}
	}
}

// pinPeak samples PinnedSegments until the function it returns is called,
// and that function returns the highest value seen. The sampling also stops
// when the test ends. A sample that does not return within callWait of the
// stop fails the test and abandons the fixture's journal.
//
// decision: the gauge is sampled every 100 µs of real time, beside the exact
// reading the checks take while every callback is parked. Its ceiling: a
// pin taken and dropped between two samples goes unseen. Overturned if Stats
// reports the gauge's high mark, when the checks read that instead.
func pinPeak(t *testing.T, f *journalv2test.Fixture) func() int {
	j := f.J
	stop, peak := make(chan struct{}), make(chan int, 1)
	go func() {
		high := 0
		for {
			high = max(high, j.Stats().PinnedSegments)
			select {
			case <-stop:
				peak <- high
				return
			case <-time.After(100 * time.Microsecond):
			}
		}
	}()
	var once sync.Once
	var high int
	end := func() int {
		once.Do(func() {
			close(stop)
			select {
			case high = <-peak:
			case <-time.After(callWait):
				f.Abandon()
				t.Errorf("a read of Stats did not return within %v", callWait)
			}
		})
		return high
	}
	t.Cleanup(func() { end() })
	return end
}

// TestRFC1B_Coalescing checks that repack copies adjacent held extents of one
// file, at one version and one offloaded bit, as one record and one entry,
// and that content filled back in large calls takes one index entry per
// record piece, not one per write it replaced.
//
// The segments are the fixture's 1 MiB cap, in a journal of 256 MiB rather
// than the fixture's 64 MiB, since the 64 MiB written and the 64 MiB filled
// back can both be on disk until the released segments are retired.
func TestRFC1B_Coalescing(t *testing.T) {
	f := journalv2test.NewWith(t, func(s *journalv2.Settings) { s.MaxBytes = 4 * journalv2test.FixtureMaxBytes })

	// Nine fills at one version, in a new journal's first segment of their
	// stream, the last one released so the segment has something to recover.
	h, id := f.Share(1), journalv2test.File(1)
	const piece = 16 << 10
	v := drawVersion(t, f)
	for i := range int64(9) {
		fillRange(t, h, id, i*piece, pattern(fmt.Sprint("piece", i), piece), v)
	}
	run := journalv2.Extent{Off: 0, Len: 8 * piece}
	freeExtent(t, h, id, journalv2.Extent{Off: run.Len, Len: piece})
	syncNow(t, h, id)
	seg := segmentOf(t, f, id, 0)
	var before int
	for _, e := range indexEntries(t, f, id) {
		if e.Kind == journalv2.EntryHeld && e.Location.Segment == seg {
			before++
		}
	}
	if before < 2 {
		t.Fatalf("eight fills left %d held entries in segment %d, want several to coalesce", before, seg)
	}
	repackAll(t, f, journalv2.RepackScope{Segments: []journalv2.SegmentID{seg}})
	if !retired(f, seg) {
		t.Fatalf("segment %d is still owned after its repack", seg)
	}
	if after := wantPieces(t, indexEntries(t, f, id), run, "the repacked fills"); after >= before {
		t.Fatalf("repack copied %d adjacent filled extents of one version into %d entries, want fewer", before, after)
	}
	for i := range int64(8) {
		got, missing := readBack(t, h, id, i*piece, piece)
		if len(missing) != 0 || !bytes.Equal(got, pattern(fmt.Sprint("piece", i), piece)) {
			t.Fatalf("fill %d reads missing %+v or other bytes after the repack", i, missing)
		}
	}

	// 64 MiB in 64 KiB writes, offloaded, released and filled back.
	h, id = f.Share(0), journalv2test.File(0)
	const total, chunk, fetch = 64 << 20, 64 << 10, 4 << 20
	var first, last journalv2.Version
	for off := int64(0); off < total; off += chunk {
		last = write(t, h, id, off, pattern(fmt.Sprint("small", off), chunk))
		if off == 0 {
			first = last
		}
	}
	syncNow(t, h, id)
	whole := journalv2.Extent{Off: 0, Len: total}
	err := h.MarkOffloaded(t.Context(), id, []journalv2.Extent{whole}, first, last)
	stub(t, "MarkOffloaded", err)
	if err != nil {
		t.Fatalf("mark the file offloaded: %v", err)
	}
	freeExtent(t, h, id, whole)

	v = drawVersion(t, f)
	for off := int64(0); off < total; off += fetch {
		fillRange(t, h, id, off, pattern(fmt.Sprint("fetched", off), fetch), v)
	}
	es := indexEntries(t, f, id)
	var entries int
	for off := int64(0); off < total; off += fetch {
		entries += wantPieces(t, es, journalv2.Extent{Off: off, Len: fetch}, fmt.Sprintf("the fill at %d", off))
		got, missing := readBack(t, h, id, off, fetch)
		if len(missing) != 0 || !bytes.Equal(got, pattern(fmt.Sprint("fetched", off), fetch)) {
			t.Fatalf("the fill at %d reads missing %+v or other bytes", off, missing)
		}
	}
	if entries >= total/chunk {
		t.Fatalf("fills of 4 MiB take %d entries, want fewer than the %d writes they replaced", entries, total/chunk)
	}
}

// indexBound is the index entry bound the index-bound check runs under.
const indexBound = 10_000

// TestRFC1B_IndexBoundRefuses checks that the write that would pass the index
// entry bound is refused naming the index, at once and with no I/O, and
// counted; that a truncate and a release still succeed at the bound, and a
// write succeeds again once offload, release and repack lowered the count;
// and that punches inside one held extent are refused naming the index once
// the count stands an eighth of the bound above it, while a truncate and a
// delete still succeed.
//
// The segments are the fixture's 1 MiB cap, in a journal of 128 MiB rather
// than the fixture's 64 MiB, so 10^4 scattered 4 KiB writes, twice over, fit
// beside what repack has not yet returned.
func TestRFC1B_IndexBoundRefuses(t *testing.T) {
	f := journalv2test.NewWith(t, func(s *journalv2.Settings) {
		s.ExtentLimit = indexBound
		s.MaxBytes = 2 * journalv2test.FixtureMaxBytes
	})
	const size, gap = 4 << 10, 8 << 10
	if got := f.J.Stats().ExtentLimit; got != indexBound {
		t.Fatalf("ExtentLimit = %d, want the %d it was set to", got, indexBound)
	}

	// The file the punches go into: one 512 KiB write, made first so it is
	// likely one record, though a repack may cut it where its copy crosses a
	// segment boundary.
	holed := journalv2test.File(2)
	write(t, f.Share(1), holed, 0, pattern("holed", 512<<10))

	// scatter writes 4 KiB a gap apart, so no two merge, round robin over two
	// files on two shares, until the index refuses one.
	scatter := func(first int, from int64) []written {
		t.Helper()
		var ws []written
		for i := int64(0); ; i++ {
			if i > 2*indexBound {
				t.Fatalf("%d scattered writes under an index bound of %d were never refused", i, indexBound)
			}
			k := int(i % 2)
			id, off := journalv2test.File(first+k), from+i/2*gap
			v, err := tryWrite(t, f.Share(k), id, off, pattern(fmt.Sprint("scatter", first, i), size))
			stub(t, "WriteAt", err)
			if err != nil {
				refusal(t, err, "a write past the index bound", journalv2.LimitIndex, journalv2.UnitEntries)
				return ws
			}
			ws = append(ws, written{k, id, journalv2.Extent{Off: off, Len: size}, v})
		}
	}

	ws := scatter(0, 0)
	if got := f.J.Stats().ExtentCount; got > indexBound {
		t.Fatalf("ExtentCount = %d once content writes were refused, past the bound %d", got, indexBound)
	}
	refusals := f.J.Stats().Counters.IndexRefusals
	err := refusedAtOnce(t, f, "a write at the index bound", func() error {
		_, err := f.Share(0).WriteAt(t.Context(), journalv2test.File(0), 1<<40, pattern("at-bound", size), 0)
		return err
	})
	refusal(t, err, "a write at the index bound", journalv2.LimitIndex, journalv2.UnitEntries)
	if got := f.J.Stats().Counters.IndexRefusals; got != refusals+1 {
		t.Fatalf("IndexRefusals moved by %d over one write refused at the index bound, want 1", got-refusals)
	}

	// A truncate, and a release of whole extents, at the bound.
	var kept, released []written
	for _, w := range ws {
		if w.share == 0 {
			released = append(released, w)
		} else {
			kept = append(kept, w)
		}
	}
	tr, err := f.Share(1).Truncate(t.Context(), journalv2test.File(1), kept[len(kept)/2].extent.Off)
	stub(t, "Truncate", err)
	if err != nil {
		t.Fatalf("a truncate at the index bound: %v", err)
	}
	f.Share(1).Settle(journalv2test.File(1), tr)
	evict(t, f.Share(0), released[:1])

	// Offload, release and repack the rest of file 0.
	markFile(t, f, released[1:])
	for _, w := range released[1:] {
		freeExtent(t, f.Share(0), w.id, w.extent)
	}
	syncNow(t, f.Share(0), journalv2test.File(0))
	repackAll(t, f, journalv2.RepackScope{})
	write(t, f.Share(0), journalv2test.File(0), 1<<40, pattern("after", size))

	// Back to the bound, then punches of one byte inside the one held extent.
	scatter(4, 1<<41)
	// The largest held piece of the file, which must take every punch.
	var e journalv2.Extent
	for _, en := range indexEntries(t, f, holed) {
		if en.Kind == journalv2.EntryHeld && en.Extent.Len > e.Len {
			e = en.Extent
		}
	}
	const punches = 100_000
	if e.Len < 2*punches+2 {
		t.Fatalf("the largest held extent of the 512 KiB file is %+v, too short for %d punches", e, punches)
	}
	ceiling := int64(indexBound + indexBound/8)
	// decision: a punch accepted after one was refused fails the check, which
	// holds only while nothing lowers the count during the loop. Only Settle
	// and Forget drop removal markers, and only Release, a removal or a
	// repack's coalescing drops a held entry; the loop makes none of those
	// calls, and neither the fixture nor the journal's own background work
	// (idle seals, retiring emptied segments, making spares) makes them. The
	// ceiling: a journal that coalesced markers or entries on its own would
	// fail here without being wrong. Overturned if the journal ever lowers the
	// count without being called, when the check reads the count after a
	// refusal and accepts a punch only below the ceiling.
	accepted, refused := 0, 0
	for i := range int64(punches) {
		_, err := f.Share(1).Deallocate(t.Context(), holed, journalv2.Extent{Off: e.Off + 1 + 2*i, Len: 1})
		stub(t, "Deallocate", err)
		if err == nil {
			if refused > 0 {
				t.Fatalf("punch %d succeeded after %d were refused, with nothing lowering the count", i, refused)
			}
			if got := f.J.Stats().ExtentCount; got > ceiling+2 {
				t.Fatalf("ExtentCount = %d after %d punches, past the bound %d and an eighth of it", got, accepted+1, indexBound)
			}
			accepted++
			continue
		}
		refusal(t, err, "a punch past an eighth of the bound above it", journalv2.LimitIndex, journalv2.UnitEntries)
		if refused == 0 {
			if got := f.J.Stats().ExtentCount; got < ceiling-2 {
				t.Fatalf("a punch was refused with ExtentCount = %d, short of the bound %d and an eighth of it", got, indexBound)
			}
		}
		refused++
	}
	if accepted == 0 || refused == 0 {
		t.Fatalf("%d punches succeeded and %d were refused, want some of each", accepted, refused)
	}
	_, err = f.Share(1).Truncate(t.Context(), holed, 4096)
	stub(t, "Truncate", err)
	if err != nil {
		t.Fatalf("a truncate with punches refused: %v", err)
	}
	_, err = f.Share(0).Delete(t.Context(), journalv2test.File(4))
	stub(t, "Delete", err)
	if err != nil {
		t.Fatalf("a delete with punches refused: %v", err)
	}
}

// TestRFC1B_PinnedSegmentsBounded checks that an offer over dirty extents of
// one file, one per segment across 1,000 segments, ends at the bound on the
// segments one offer pins, never pins more, and leaves repack free to retire
// the segments it did not pin while its callback is parked.
//
// The segments are the fixture's 1 MiB cap, each sealed once idle at about
// 80 KiB, in a journal holding 256 MiB.
func TestRFC1B_PinnedSegmentsBounded(t *testing.T) {
	f := journalv2test.NewWith(t, func(s *journalv2.Settings) { s.MaxBytes = 4 * journalv2test.FixtureMaxBytes })
	h, id := f.Share(0), journalv2test.File(0)
	const segments, size, bait = 1000, 64 << 10, 16 << 10

	// Each segment: a 64 KiB dirty extent and a 16 KiB one released, so it
	// holds something repack can recover.
	segs := make([]journalv2.SegmentID, segments)
	seen := map[journalv2.SegmentID]bool{}
	var through journalv2.Version
	for i := range int64(segments) {
		off := i << 20
		b := journalv2.Extent{Off: off + 512<<10, Len: bait}
		bv := write(t, h, id, b.Off, pattern(fmt.Sprint("bait", i), bait))
		through = write(t, h, id, off, pattern(fmt.Sprint("dirty", i), size))
		mark(t, h, id, b, bv)
		freeExtent(t, h, id, b)
		segs[i] = segmentOf(t, f, id, off)
		if seen[segs[i]] {
			t.Fatalf("segment %d holds two of the scattered dirty extents", segs[i])
		}
		seen[segs[i]] = true
		sealNow(t, f, segs[i])
	}

	peak := pinPeak(t, f)
	st := newStall(t)
	var offered []journalv2.Extent
	done := make(chan error, 1)
	go func() {
		done <- h.Offload(t.Context(), id, through, 64<<20, 0, func(_ context.Context, o journalv2.Offer, _ func([]journalv2.Extent) error) error {
			for _, d := range o.Dirty {
				offered = append(offered, d.Extent)
			}
			st.park()
			return nil
		})
	}()
	waitParked(t, f, st, 1, done)

	pinned := map[journalv2.SegmentID]bool{}
	for _, e := range offered {
		pinned[segs[e.Off>>20]] = true
	}
	if len(pinned) != journalv2test.OfferPins {
		t.Fatalf("the offer took extents of %d segments of %d, want it to end at the bound of %d", len(pinned), segments, journalv2test.OfferPins)
	}
	if got := f.J.Stats().PinnedSegments; got != len(pinned) {
		t.Fatalf("PinnedSegments = %d with one offer parked over %d segments", got, len(pinned))
	}

	var free []journalv2.SegmentID
	for _, s := range segs {
		if !pinned[s] {
			free = append(free, s)
		}
	}
	// Every segment is in the scope, so a repack that took a pinned one is seen.
	repackAll(t, f, journalv2.RepackScope{Segments: segs})
	st.wantParked(t, "a repack beside the parked offer")
	for _, s := range free {
		if !retired(f, s) {
			t.Fatalf("segment %d, which the offer did not pin, is still owned after its repack", s)
		}
	}
	for s := range pinned {
		if _, ok := segmentInfo(f, s); !ok {
			t.Fatalf("segment %d, pinned by the parked offer, was retired", s)
		}
	}

	st.letGo()
	joinOffers(t, f, done, 1)
	if got := peak(); got > journalv2test.OfferPins {
		t.Fatalf("PinnedSegments reached %d over one offer, past the bound of %d", got, journalv2test.OfferPins)
	}
}

// TestRFC1B_PinsBoundedAcrossOffers checks that eight offers parked at once,
// each over a file with dirty extents one per segment across 200 segments,
// pin no more segments together than the journal-wide bound plus one each,
// and that each still offers an extent.
//
// The 1,600 segments are the fixture's 1 MiB cap, each sealed once idle at
// one 4 KiB extent.
func TestRFC1B_PinsBoundedAcrossOffers(t *testing.T) {
	const offers, perFile, size = 8, 200, 4 << 10
	if journalv2test.JournalPins+offers >= offers*min(journalv2test.OfferPins, perFile) {
		t.Fatalf("%d offers capped at %d segments each cannot pin past the journal-wide bound of %d plus one each: the check cannot fail", offers, min(journalv2test.OfferPins, perFile), journalv2test.JournalPins)
	}
	f := journalv2test.NewWith(t, func(s *journalv2.Settings) { s.IdleSealBytes = size })

	hs := make([]*journalv2.Handle, offers)
	throughs := make([]journalv2.Version, offers)
	seen := map[journalv2.SegmentID]bool{}
	for i := range int64(perFile) {
		for k := range offers {
			hs[k] = f.Share(k % 2)
			id, off := journalv2test.File(k), i<<20
			throughs[k] = write(t, hs[k], id, off, pattern(fmt.Sprint("pin", k, i), size))
			seg := segmentOf(t, f, id, off)
			if seen[seg] {
				t.Fatalf("segment %d holds two of the scattered dirty extents", seg)
			}
			seen[seg] = true
			sealNow(t, f, seg)
		}
	}

	peak := pinPeak(t, f)
	st := newStall(t)
	took := make([]int, offers)
	done := make(chan error, offers)
	for k := range offers {
		go func() {
			done <- hs[k].Offload(t.Context(), journalv2test.File(k), throughs[k], 1<<30, 0, func(_ context.Context, o journalv2.Offer, _ func([]journalv2.Extent) error) error {
				took[k] = len(o.Dirty)
				st.park()
				return nil
			})
		}()
	}
	waitParked(t, f, st, offers, done)

	bound := journalv2test.JournalPins + offers
	if got := f.J.Stats().PinnedSegments; got < offers || got > bound {
		t.Fatalf("PinnedSegments = %d with %d offers parked over segments of their own, want at least one each and at most %d", got, offers, bound)
	}
	for k, n := range took {
		if n == 0 {
			t.Fatalf("offer %d offered nothing while dirty extents of its file remained", k)
		}
	}
	// A callback that let itself go before the reading was not parked for it.
	st.wantParked(t, "the reading of eight parked offers")
	st.letGo()
	joinOffers(t, f, done, offers)
	if got := peak(); got > bound {
		t.Fatalf("PinnedSegments reached %d over %d offers, past the journal-wide bound plus one each, %d", got, offers, bound)
	}
}

// failWindow makes the next sync of segment seg fail, and every read of it
// fail too, so a journal can resolve the window only from a copy it kept.
func failWindow(t *testing.T, f *journalv2test.Fixture, seg journalv2.SegmentID) {
	t.Helper()

	name := segmentName(seg)
	eio := sim.FaultAction{Kind: sim.Fail, Err: syscall.EIO}
	fault, err := sim.NewSchedule(1, nil,
		sim.Rule{Op: journalv2test.OpSync, Key: name, Count: 1, Action: eio},
		sim.Rule{Op: journalv2test.OpRead, Key: name, Action: eio},
		sim.Rule{Op: journalv2test.OpReadDirect, Key: name, Action: eio},
	)
	if err != nil {
		t.Fatalf("fail segment %d's sync: %v", seg, err)
	}
	f.Seam.SetFault(fault)
}

// TestRFC1B_FailedWindowDoesNotWedge checks that after a failed sync window
// of a file, a later write to it syncs in a new segment and its Sync succeeds,
// the failed write is either reported lost or kept, and an offload of the file
// proceeds.
//
// decision: a journal resolves a failed window by failing it or by
// re-appending it from a copy it kept, and the check cannot choose which. It
// fails every read of the failed segment, so a journal that would re-append
// from there fails the window instead; one that kept a copy re-appends, and
// the check then asserts the write survived, not that it was lost. Overturned
// if the journal's choice becomes observable or settable.
func TestRFC1B_FailedWindowDoesNotWedge(t *testing.T) {
	f := journalv2test.New(t)

	// kept is each write of a failed window whose Sync succeeded: a crash at
	// the end must find it, since reading it before only shows it readable.
	var kept []int
	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		a := journalv2.Extent{Off: 0, Len: 4096}
		pa := pattern(fmt.Sprint("window", k), int(a.Len))
		write(t, h, id, a.Off, pa)
		seg := segmentOf(t, f, id, a.Off)
		gen := f.J.Stats().LossGeneration
		failWindow(t, f, seg)
		err := h.Sync(t.Context(), id)
		f.Seam.SetFault(nil)
		stub(t, "Sync", err)
		failed := errors.Is(err, journalv2.ErrSyncFailed)
		if err != nil && !failed {
			t.Fatalf("share %d: the failed sync returned %v, want a SyncError or a re-append", k, err)
		}

		b := journalv2.Extent{Off: 64 << 10, Len: 4096}
		vb := write(t, h, id, b.Off, pattern(fmt.Sprint("after", k), int(b.Len)))
		bSeg := segmentOf(t, f, id, b.Off)
		if bSeg == seg {
			t.Fatalf("share %d: the write after a failed sync was appended to the failed segment %d", k, seg)
		}
		syncs := f.Seam.CallsFor(journalv2test.OpSync, segmentName(bSeg))
		if err := h.Sync(t.Context(), id); err != nil {
			stub(t, "Sync", err)
			t.Fatalf("share %d: the Sync after a failed window: %v; the file is wedged", k, err)
		}
		if f.Seam.CallsFor(journalv2test.OpSync, segmentName(bSeg)) == syncs {
			t.Fatalf("share %d: Sync returned without syncing segment %d, which holds the write", k, bSeg)
		}
		if s, ok := segmentInfo(f, seg); ok && !s.Failed {
			t.Fatalf("share %d: segment %d, whose sync failed, is not reported Failed", k, seg)
		}

		got, missing := readBack(t, h, id, a.Off, a.Len)
		if failed {
			if !slices.Equal(joinedExtents(missing), []journalv2.Extent{a}) {
				t.Fatalf("share %d: the write of the failed window reads missing %+v, want %+v", k, missing, a)
			}
			lost := false
			for _, ev := range f.J.Stats().Losses {
				lost = lost || ev.File == id && ev.Reason == journalv2.LossFailedWindow && ev.Extent.Off < a.Off+a.Len && a.Off < ev.Extent.Off+ev.Extent.Len
			}
			if !lost {
				t.Fatalf("share %d: no loss event names the write of the failed window: %+v", k, f.J.Stats().Losses)
			}
			if f.J.Stats().LossGeneration <= gen {
				t.Fatalf("share %d: LossGeneration did not rise over a failed window", k)
			}
		} else if len(missing) != 0 || !bytes.Equal(got, pa) {
			t.Fatalf("share %d: Sync succeeded over a failed sync, and the write reads missing %+v or other bytes", k, missing)
		} else {
			kept = append(kept, k)
		}

		var offered []journalv2.Extent
		err = h.Offload(t.Context(), id, vb, 1<<20, 0, func(_ context.Context, o journalv2.Offer, report func([]journalv2.Extent) error) error {
			for _, d := range o.Dirty {
				offered = append(offered, d.Extent)
			}
			if err := report(offered); err != nil {
				return called("report", err)
			}
			return nil
		})
		wantOffer(t, err)
		var n int64
		for _, e := range offered {
			n += max(0, min(e.Off+e.Len, b.Off+b.Len)-max(e.Off, b.Off))
		}
		if n != b.Len {
			t.Fatalf("share %d: the offload after a failed window offered %d bytes of the later write, want all %d", k, n, b.Len)
		}
	}

	if len(kept) == 0 {
		return
	}
	f.Crash()
	for _, k := range kept {
		got, missing := readBack(t, f.Share(k), journalv2test.File(k), 0, 4096)
		if len(missing) != 0 || !bytes.Equal(got, pattern(fmt.Sprint("window", k), 4096)) {
			t.Fatalf("share %d: the write of a failed window, whose later Sync succeeded, reads missing %+v or other bytes after a crash", k, missing)
		}
	}
}

// TestRFC1B_SyncFailure checks that a failed sync is reported, never recorded
// as success, and that later writes on the same stream are still synced, by
// Sync and by the sync timer.
//
// decision: a Sync over the failed sync either returns a SyncError or, where
// the journal re-appended the window elsewhere, succeeds; the check cannot
// choose which. Either way Stats records the failure, and a Sync that
// succeeded must have made the write durable, which a crash at the end
// checks. Overturned if the journal's choice becomes observable or settable.
func TestRFC1B_SyncFailure(t *testing.T) {
	f := journalv2test.New(t)

	// durable is every write whose Sync succeeded, with its bytes.
	type kept struct {
		w written
		p []byte
	}
	var durable []kept
	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		pf := pattern(fmt.Sprint("failed", k), 4096)
		v := write(t, h, id, 0, pf)
		seg := segmentOf(t, f, id, 0)
		failures := f.J.Stats().Counters.SyncFailures
		f.FailNext(journalv2test.OpSync, segmentName(seg), 1)
		err := h.Sync(t.Context(), id)
		f.Seam.SetFault(nil)
		stub(t, "Sync", err)
		if err != nil && (!errors.Is(err, journalv2.ErrSyncFailed) || !errors.Is(err, syscall.EIO)) {
			t.Fatalf("share %d: a Sync whose sync failed with EIO returned %v, want a SyncError wrapping it", k, err)
		}
		if err == nil {
			durable = append(durable, kept{written{k, id, journalv2.Extent{Off: 0, Len: 4096}, v}, pf})
		}
		s := f.J.Stats()
		if s.Counters.SyncFailures <= failures || s.LastSyncError == nil || !errors.Is(s.LastSyncError.Err, syscall.EIO) {
			t.Fatalf("share %d: SyncFailures moved by %d and LastSyncError is %+v after a sync failed with EIO, want it recorded", k, s.Counters.SyncFailures-failures, s.LastSyncError)
		}

		// Syncs are counted on the segment holding the write, only once the
		// write returned, so neither another stream's sync nor one that began
		// before the write stands in for a sync of it.
		pn := pattern(fmt.Sprint("next", k), 4096)
		v = write(t, h, id, 64<<10, pn)
		next := segmentName(segmentOf(t, f, id, 64<<10))
		syncs := f.Seam.CallsFor(journalv2test.OpSync, next)
		syncNow(t, h, id)
		if f.Seam.CallsFor(journalv2test.OpSync, next) == syncs {
			t.Fatalf("share %d: a Sync after a failed one returned without a sync of %s, which holds the write", k, next)
		}
		durable = append(durable, kept{written{k, id, journalv2.Extent{Off: 64 << 10, Len: 4096}, v}, pn})

		// The timer's sync is seen where it lands, in the image a crash keeps
		// of the write's segment: that the write was synced, not that some
		// sync happened.
		pt := pattern(fmt.Sprint("timer", k), 4096)
		v = write(t, h, id, 128<<10, pt)
		timer := segmentName(segmentOf(t, f, id, 128<<10))
		f.Advance(journalv2.SyncBound)
		eventually(t, fmt.Sprintf("share %d: the write after a failed sync was not made durable in %s within the sync bound", k, timer), func() bool {
			image, err := f.Seam.Durable(timer)
			return err == nil && bytes.Contains(image, pt)
		})
		durable = append(durable, kept{written{k, id, journalv2.Extent{Off: 128 << 10, Len: 4096}, v}, pt})
	}

	f.Crash()
	for _, d := range durable {
		w := d.w
		got, missing := readBack(t, f.Share(w.share), w.id, w.extent.Off, w.extent.Len)
		if len(missing) != 0 || !bytes.Equal(got, d.p) {
			t.Fatalf("share %d: the write at %d, synced, reads missing %+v or other bytes after a crash", w.share, w.extent.Off, missing)
		}
	}
}

// relayWait bounds, in real time, how long relay keeps a read in the seam
// when no other read arrives to take its place.
//
// decision: a guard that holds new readers back while reclamation waits stops
// the relay, and only a real-time wait lets the last read in it go. The
// ceiling: a guard that lets reclamation in only after a pause in reads this
// long passes. Overturned once the journal reports its guard waits to the
// seam, when the relay can end at the wait instead.
const relayWait = 20 * time.Millisecond

// relay is a fault that keeps each read of one file in the seam until the
// next read of it arrives, or relayWait passes, so while readers keep coming
// some read of the file is always in flight inside the journal.
//
// decision: holds cannot do this. A released hold frees every read parked at
// it at once, and with every reader parked none can reach the next hold first,
// so the reads in flight drop to none at each rotation, which is exactly the
// gap a guard that waits for no readers needs.
type relay struct {
	key     string
	mu      sync.Mutex
	next    chan struct{} // closed when the next read arrives
	relayed atomic.Int64  // reads let go by the arrival of another
}

func newRelay(key string) *relay { return &relay{key: key, next: make(chan struct{})} }

func (r *relay) At(op string, key []byte) sim.FaultAction {
	if (op != journalv2test.OpRead && op != journalv2test.OpReadDirect) || string(key) != r.key {
		return sim.FaultAction{}
	}
	r.mu.Lock()
	close(r.next)
	r.next = make(chan struct{})
	wait := r.next
	r.mu.Unlock()
	select {
	case <-wait:
		r.relayed.Add(1)
	case <-time.After(relayWait):
	}
	return sim.FaultAction{}
}

// TestRFC1B_ReclaimNotStarved checks that a repack of a segment under a
// sustained stream of reads of it still retires it within a bounded time,
// with every read served the right bytes throughout. The reads are relayed at
// the seam, so one is in flight inside the journal at every moment while they
// keep coming: a guard that lets reclamation in only once no reader holds it
// never does.
func TestRFC1B_ReclaimNotStarved(t *testing.T) {
	f := journalv2test.New(t)
	const readers = 4

	for k := range 2 {
		h, id := f.Share(k), journalv2test.File(k)
		seg, es := soleSegment(t, f, h, id)
		var kept []journalv2.Extent
		for i, e := range es {
			if i%2 == 0 {
				kept = append(kept, e.Extent)
				continue
			}
			freeExtent(t, h, id, e.Extent)
		}
		syncNow(t, h, id)
		// The bytes soleSegment wrote over e.
		want := func(e journalv2.Extent) []byte {
			start := e.Off - e.Off%soleChunk
			return pattern(fmt.Sprint("sole", start), soleChunk)[e.Off-start : e.Off-start+e.Len]
		}

		stop := make(chan struct{})
		var reads atomic.Int64
		failed := make(chan error, readers)
		var wg sync.WaitGroup
		for r := range readers {
			wg.Go(func() {
				for i := r; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					e := kept[i%len(kept)]
					buf := make([]byte, e.Len)
					_, missing, _, _, err := h.ReadAt(t.Context(), id, e.Off, buf)
					if err != nil {
						failed <- called("ReadAt", err)
						return
					}
					if len(missing) != 0 || !bytes.Equal(buf, want(e)) {
						failed <- fmt.Errorf("a read of %+v under repack reported missing %+v or served other bytes", e, missing)
						return
					}
					reads.Add(1)
				}
			})
		}
		// join stops the readers and waits for them, for at most callWait: a
		// reader blocked in the journal abandons it, as within does.
		joined := false
		join := func() {
			if joined {
				return
			}
			joined = true
			close(stop)
			all := make(chan struct{})
			go func() { wg.Wait(); close(all) }()
			select {
			case <-all:
			case <-time.After(callWait):
				f.Abandon()
				t.Errorf("share %d: the readers did not return within %v of being stopped", k, callWait)
			}
		}
		defer join()
		readerFailed := func() {
			t.Helper()
			select {
			case rerr := <-failed:
				join()
				var ce *callError
				if errors.As(rerr, &ce) {
					stub(t, ce.call, ce.err)
				}
				t.Fatalf("share %d: %v", k, rerr)
			default:
			}
		}
		eventually(t, "the readers did not get going", func() bool { return reads.Load() >= 64 || len(failed) > 0 })
		readerFailed()

		// The repack starts only once the readers' reads are being relayed,
		// so it meets the load from its first attempt at the segment, and
		// every read relayed so far is a reader's.
		relayed := newRelay(segmentName(seg.ID))
		f.Seam.SetFault(relayed)
		eventually(t, "the readers' reads were not relayed at the seam", func() bool { return relayed.relayed.Load() >= 64 || len(failed) > 0 })
		readerFailed()
		repacked := make(chan error, 1)
		var freed int64
		go func() {
			var err error
			freed, err = f.J.Repack(t.Context(), journalv2test.FixtureMaxBytes, journalv2.RepackScope{Segments: []journalv2.SegmentID{seg.ID}})
			repacked <- err
		}()
		var err error
		select {
		case err = <-repacked:
		case <-time.After(callWait):
			f.Abandon()
			join()
			t.Fatalf("share %d: a repack of a segment under %d readers did not return within %v", k, readers, callWait)
		}
		join()
		f.Seam.SetFault(nil)
		readerFailed()
		stub(t, "Repack", err)
		if err != nil {
			t.Fatalf("share %d: repack under readers: %v", k, err)
		}
		if freed <= 0 || !retired(f, seg.ID) {
			t.Fatalf("share %d: the repack under readers freed %d and left segment %d owned: %v", k, freed, seg.ID, !retired(f, seg.ID))
		}
		for _, e := range kept {
			got, missing := readBack(t, h, id, e.Off, e.Len)
			if len(missing) != 0 || !bytes.Equal(got, want(e)) {
				t.Fatalf("share %d: %+v reads missing %+v or other bytes after the repack", k, e, missing)
			}
		}
	}
}

// TestRFC1B_RemovalsDoNotWaitOnOffload checks that while an offer's callback
// is parked, a delete of another file whose records share a segment with the
// offered ones, and a truncate and a deallocate of the offered file, each
// return without waiting for it and take effect for reads, and that the
// deleted file's storage is recovered once the pass has returned.
//
// decision: the deleted file is whichever of eight files, written
// interleaved with the offered one, came to share a segment with it, since
// which stream a file's records go to is the journal's choice. Its ceiling: a
// journal that never places those eight beside the offered file fails the
// check at its set-up. Overturned if a file's stream becomes observable, so
// the check can pick a file on the offered one's.
func TestRFC1B_RemovalsDoNotWaitOnOffload(t *testing.T) {
	f := journalv2test.New(t)
	h, x := f.Share(0), journalv2test.File(0)
	other := f.Share(1)
	const size, others = 64 << 10, 8

	// X's writes interleaved with those of eight files of the other share, so
	// one of them shares a segment with X whatever stream each file is on.
	var through journalv2.Version
	for i := range int64(8) {
		through = write(t, h, x, i*size, pattern(fmt.Sprint("x", i), size))
		for c := range others {
			write(t, other, journalv2test.File(10+c), i*size, pattern(fmt.Sprint("o", c, i), size))
		}
	}
	syncNow(t, h, x)
	ids := make([]journalv2.FileID, others)
	for c := range others {
		ids[c] = journalv2test.File(10 + c)
	}
	syncNow(t, other, ids...)

	xSegs := map[journalv2.SegmentID]bool{}
	for _, e := range indexEntries(t, f, x) {
		xSegs[e.Location.Segment] = true
	}
	var y journalv2.FileID
	var ySegs []journalv2.SegmentID
	for _, id := range ids {
		for _, e := range indexEntries(t, f, id) {
			if e.Kind == journalv2.EntryHeld && xSegs[e.Location.Segment] {
				y = id
			}
		}
		if y == id {
			for _, e := range indexEntries(t, f, id) {
				if !slices.Contains(ySegs, e.Location.Segment) {
					ySegs = append(ySegs, e.Location.Segment)
				}
			}
			break
		}
	}
	if len(ySegs) == 0 {
		t.Fatal("none of eight files written interleaved with the offered one shares a segment with it")
	}

	st := newStall(t)
	done := make(chan error, 1)
	go func() {
		done <- h.Offload(t.Context(), x, through, 1<<20, 0, func(context.Context, journalv2.Offer, func([]journalv2.Extent) error) error {
			st.park()
			return nil
		})
	}()
	waitParked(t, f, st, 1, done)

	yUsed := f.J.Stats().Shares[journalv2test.Tag(1)].UsedBytes
	err := within(t, f, "a delete beside a parked offer", func() error {
		_, err := other.Delete(t.Context(), y)
		return err
	})
	stub(t, "Delete", err)
	if err != nil {
		t.Fatalf("delete beside a parked offer: %v", err)
	}
	err = within(t, f, "a truncate beside a parked offer", func() error {
		_, err := h.Truncate(t.Context(), x, 4*size)
		return err
	})
	stub(t, "Truncate", err)
	if err != nil {
		t.Fatalf("truncate of the offered file beside its parked offer: %v", err)
	}
	gone := journalv2.Extent{Off: size, Len: size}
	err = within(t, f, "a deallocate beside a parked offer", func() error {
		_, err := h.Deallocate(t.Context(), x, gone)
		return err
	})
	stub(t, "Deallocate", err)
	if err != nil {
		t.Fatalf("deallocate of the offered file beside its parked offer: %v", err)
	}
	st.wantParked(t, "a delete, a truncate and a deallocate")

	if _, missing := readBack(t, other, y, 0, 8*size); !slices.Equal(joinedExtents(missing), []journalv2.Extent{{Off: 0, Len: 8 * size}}) {
		t.Fatalf("the deleted file reads missing %+v beside the parked offer, want all of it", missing)
	}
	if _, missing := readBack(t, h, x, 0, 8*size); !slices.Equal(joinedExtents(missing), []journalv2.Extent{gone, {Off: 4 * size, Len: 4 * size}}) {
		t.Fatalf("the offered file reads missing %+v after its truncate and deallocate, want %+v and from %d on", missing, gone, 4*size)
	}
	// A repack of the deleted file's segments beside the parked offer must
	// leave those holding its offered bytes.
	repackAll(t, f, journalv2.RepackScope{Segments: ySegs})
	st.wantParked(t, "a repack beside the parked offer")
	for _, s := range ySegs {
		if xSegs[s] {
			if _, ok := segmentInfo(f, s); !ok {
				t.Fatalf("segment %d, holding bytes of the parked offer, was retired under it", s)
			}
		}
	}

	st.letGo()
	joinOffers(t, f, done, 1)
	repackAll(t, f, journalv2.RepackScope{Segments: ySegs})
	for _, s := range ySegs {
		if !retired(f, s) {
			t.Fatalf("segment %d, which held the deleted file, is still owned after the pass returned and a repack of it", s)
		}
	}
	// The delete's own record is a few dozen bytes on disk.
	if got := f.J.Stats().Shares[journalv2test.Tag(1)].UsedBytes; got > yUsed-8*size+4096 {
		t.Fatalf("the other share uses %d bytes after its deleted file's segments were repacked, %d before: want its %d bytes returned", got, yUsed, 8*size)
	}
}

// joinedExtents returns es sorted by offset with touching extents joined.
func joinedExtents(es []journalv2.Extent) []journalv2.Extent {
	out := slices.SortedFunc(slices.Values(es), func(a, b journalv2.Extent) int { return cmp.Compare(a.Off, b.Off) })
	j := 0
	for i, e := range out {
		if i > 0 && out[j-1].Off+out[j-1].Len == e.Off {
			out[j-1].Len += e.Len
			continue
		}
		out[j] = e
		j++
	}
	return out[:j]
}

// worker is one goroutine of the concurrent run: since when its call in
// flight has run, zero between calls, and the first error it met.
type worker struct {
	name  string
	since atomic.Int64
	err   error
}

// named returns err as the error of the journal call named call, or nil.
func named(call string, err error) error {
	if err == nil {
		return nil
	}
	return called(call, err)
}

// do runs call as w's call in flight.
func (w *worker) do(call func() error) error {
	w.since.Store(time.Now().UnixNano())
	defer w.since.Store(0)
	return call()
}

// watchCalls returns true once finished is closed. If a worker's call has
// been in flight for longer than bound first, it fails the test with every
// goroutine's stack written to the test's artifact directory, calls abandon,
// and returns false.
func watchCalls(t *testing.T, workers []*worker, finished <-chan struct{}, bound time.Duration, abandon func()) bool {
	t.Helper()

	for {
		select {
		case <-finished:
			return true
		case <-time.After(10 * time.Millisecond):
		}
		for _, w := range workers {
			if s := w.since.Load(); s != 0 && time.Since(time.Unix(0, s)) > bound {
				buf := make([]byte, 1<<22)
				buf = buf[:runtime.Stack(buf, true)]
				where := filepath.Join(t.ArtifactDir(), "goroutines.txt")
				if err := os.WriteFile(where, buf, 0o600); err != nil {
					where = "nowhere: " + err.Error()
				}
				t.Errorf("a %s call blocked for more than %v with readers, writers, fills, releases and repack running at once; every goroutine's stack written to %s", w.name, bound, where)
				abandon()
				return false
			}
		}
	}
}

// TestRFC1B_NoDeadlock checks that readers, writers, fills, releases and
// repack running at once over the same files and segments all finish, with
// no call blocked past a bound, and leave an index that verifies against the
// segments.
//
// decision: a call blocked past callWait fails the test with every
// goroutine's stack written to its artifact directory, and the journal is
// abandoned unclosed. Its cleanup would close the journal, which waits for the
// blocked call, so the test would hang until the suite's timeout took the
// whole binary down, every later test's result with it. The abandoned
// journal's goroutines and descriptors are then reported by the profiler too.
// Overturned if the journal's Close gains a bound of its own.
//
// decision: the bound is callWait of real time, not of the journal's clock. A
// call blocked on a lock waits on no timer, so moving the test clock neither
// ends the wait nor shows it, and the run is left on a clock that never moves.
// Overturned as callWait is.
func TestRFC1B_NoDeadlock(t *testing.T) {
	f := journalv2test.New(t)
	hs := []*journalv2.Handle{f.Share(0), f.Share(1)}
	const files, span, chunk, rounds = 4, 1 << 20, 16 << 10, 200
	ids := make([]journalv2.FileID, files)
	for k := range files {
		ids[k] = journalv2test.File(k)
		for off := int64(0); off < span; off += chunk {
			write(t, hs[k%2], ids[k], off, pattern(fmt.Sprint("seed", k, off), chunk))
		}
		syncNow(t, hs[k%2], ids[k])
	}
	j := f.J

	// done counts, per kind, the calls that changed something: a release
	// that released, a fill that filled, a repack that freed. The run goes on
	// until each kind has, so it never passes on calls that found nothing to
	// do while the others ran.
	done := map[string]*atomic.Int64{"release": {}, "fill": {}, "repack": {}}
	allDid := func() bool {
		for _, n := range done {
			if n.Load() == 0 {
				return false
			}
		}
		return true
	}

	// pick returns a file's handle, the file and a chunk of it.
	pick := func(r *rand.Rand) (*journalv2.Handle, journalv2.FileID, journalv2.Extent) {
		k := r.IntN(files)
		return hs[k%2], ids[k], journalv2.Extent{Off: r.Int64N(span/chunk) * chunk, Len: chunk}
	}
	kinds := map[string]func(r *rand.Rand, w *worker) error{
		"write": func(r *rand.Rand, w *worker) error {
			h, id, e := pick(r)
			return w.do(func() error {
				_, err := tryWrite(t, h, id, e.Off, pattern(fmt.Sprint("w", r.Uint64()), chunk))
				return named("WriteAt", err)
			})
		},
		"read": func(r *rand.Rand, w *worker) error {
			h, id, e := pick(r)
			return w.do(func() error {
				_, _, _, _, err := h.ReadAt(t.Context(), id, e.Off, make([]byte, e.Len))
				return named("ReadAt", err)
			})
		},
		"release": func(r *rand.Rand, w *worker) error {
			h, id, e := pick(r)
			return w.do(func() error {
				_, missing, held, _, err := h.ReadAt(t.Context(), id, e.Off, make([]byte, e.Len))
				if err != nil || len(missing) != 0 || len(held) != 1 {
					return named("ReadAt", err) // nothing whole to release
				}
				if err := h.MarkOffloaded(t.Context(), id, []journalv2.Extent{e}, held[0].Version, held[0].Version); err != nil {
					return named("MarkOffloaded", err)
				}
				err = h.Release(t.Context(), id, []journalv2.Extent{e})
				if errors.Is(err, journalv2.ErrNotOffloaded) {
					return nil // overwritten since the mark
				}
				if err == nil {
					done["release"].Add(1)
				}
				return named("Release", err)
			})
		},
		"fill": func(r *rand.Rand, w *worker) error {
			h, id, e := pick(r)
			return w.do(func() error {
				_, missing, _, asOf, err := h.ReadAt(t.Context(), id, e.Off, make([]byte, e.Len))
				if err != nil || len(missing) == 0 {
					return named("ReadAt", err) // nothing to fill
				}
				v, err := j.Draw()
				if err != nil {
					return named("Draw", err)
				}
				err = journalv2test.RetrySpare(func() error {
					return h.Fill(t.Context(), id, e.Off, pattern(fmt.Sprint("f", r.Uint64()), chunk), asOf, v)
				})
				if errors.Is(err, journalv2.ErrStaleFill) {
					return nil // written since the read
				}
				if err == nil {
					done["fill"].Add(1)
				}
				return named("Fill", err)
			})
		},
		"repack": func(_ *rand.Rand, w *worker) error {
			return w.do(func() error {
				freed, err := j.Repack(t.Context(), 2*journalv2test.FixtureSegmentCap, journalv2.RepackScope{})
				if freed > 0 {
					done["repack"].Add(1)
				}
				return named("Repack", err)
			})
		},
	}

	// Every worker starts at once, and each goes past its rounds, up to
	// maxRounds, until every kind has changed something.
	const maxRounds = 20 * rounds
	start := make(chan struct{})
	var workers []*worker
	var wg sync.WaitGroup
	for _, name := range slices.Sorted(maps.Keys(kinds)) {
		for n := range 2 {
			w := &worker{name: fmt.Sprint(name, n)}
			workers = append(workers, w)
			r := rand.New(rand.NewPCG(uint64(len(workers)), 1))
			wg.Go(func() {
				<-start
				for i := 0; i < rounds || (i < maxRounds && !allDid()); i++ {
					if err := kinds[name](r, w); err != nil {
						w.err = err
						return
					}
				}
			})
		}
	}
	close(start)
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	if !watchCalls(t, workers, finished, callWait, f.Abandon) {
		return
	}

	for _, w := range workers {
		if w.err == nil {
			continue
		}
		var ce *callError
		if errors.As(w.err, &ce) {
			stub(t, ce.call, ce.err)
		}
		t.Fatalf("%s: %v", w.name, w.err)
	}
	for _, name := range slices.Sorted(maps.Keys(done)) {
		if done[name].Load() == 0 {
			t.Fatalf("no %s call changed anything in %d rounds of each worker beside the others", name, maxRounds)
		}
	}
	rep, err := f.J.Inspect().Verify(t.Context())
	stub(t, "Verify", err)
	if err != nil {
		t.Fatalf("verify after the concurrent run: %v", err)
	}
	if len(rep.Unsupported)+len(rep.Unindexed)+len(rep.BadFooters) != 0 || rep.FootprintMismatch != 0 {
		t.Fatalf("the index does not verify after the concurrent run: %+v", rep)
	}
}

// TestRFC1B_BackgroundJoined checks that closing the journal with a repack in
// flight waits for the repack to finish or fail cleanly, never finding a
// segment closed under it, before Close returns; that nothing of the closed
// journal reaches storage afterwards; and that the segment's held content is
// intact after a crash. The profiler then checks that none of the journal's
// goroutines outlive the test.
//
// decision: that nothing reaches storage after Close is watched for
// backgroundWait of real time, with the clock moved past every timer the
// journal arms. Its ceiling: work scheduled later than that goes unseen here,
// and is left to the profiler's check of the goroutines it runs on.
// Overturned once the fixture can wait on a point the journal reports to the
// seam.
//
// decision: Close is let begin, and seen to have begun, while the repack is
// parked in its first copy, and the copy is let go only then. A Close that
// closes the segments the parked copy writes to fails that copy with the
// seam's ErrClosed, which the check reads as the repack's failure. Its
// ceiling: a Close that waits a while before closing them, and so closes them
// only once the repack happened to finish, is not caught. Overturned if the
// journal reports its shutdown steps to the seam.
func TestRFC1B_BackgroundJoined(t *testing.T) {
	f := journalv2test.New(t)
	h, id := f.Share(0), journalv2test.File(0)
	seg, es := soleSegment(t, f, h, id)
	var kept []journalv2.Extent
	for i, e := range es {
		if i%2 == 0 {
			kept = append(kept, e.Extent)
			continue
		}
		freeExtent(t, h, id, e.Extent)
	}
	syncNow(t, h, id)

	copied := f.Seam.Hold(t, journalv2test.OpWrite, "*.seg")
	repacked := make(chan error, 1)
	go func() {
		_, err := f.J.Repack(t.Context(), journalv2test.FixtureMaxBytes, journalv2.RepackScope{Segments: []journalv2.SegmentID{seg.ID}})
		repacked <- err
	}()
	select {
	case <-copied.Reached():
	case err := <-repacked:
		stub(t, "Repack", err)
		t.Fatalf("a repack of a segment half released returned (%v) without writing a copy", err)
	case <-time.After(callWait):
		copied.Release()
		f.Abandon()
		t.Fatalf("a repack of a segment half released wrote no copy within %v", callWait)
	}

	// let is closed just before the copy is let go. The repack cannot return
	// before then, so a Close that returns before it did not wait for the
	// repack. Close returning after the copy was let go is not ordered against
	// the repack's caller, so nothing is read from that.
	j := f.J
	let := make(chan struct{})
	var early atomic.Bool
	closed := make(chan error, 1)
	go func() {
		err := j.Close()
		select {
		case <-let:
		default:
			early.Store(true)
		}
		closed <- err
	}()
	// stuck lets the parked copy go, abandons the journal, whose Close or
	// repack is blocked, and fails the test.
	stuck := func(format string, args ...any) {
		t.Helper()
		copied.Release()
		f.Abandon()
		t.Fatalf(format, args...)
	}
	// Close has begun once a new call is refused. Each probe is a call of its
	// own, bounded by what is left of callWait, since one that blocks behind
	// the shutdown would otherwise keep the check from ever letting the copy go.
	for deadline := time.Now().Add(callWait); ; time.Sleep(time.Millisecond) {
		select {
		case err := <-closed:
			stub(t, "Close", err)
			t.Fatalf("Close returned (%v) while a repack was parked in its copy", err)
		default:
		}
		probe := make(chan error, 1)
		go func() {
			_, err := j.Draw()
			probe <- err
		}()
		var err error
		select {
		case err = <-probe:
		case <-time.After(time.Until(deadline)):
			stuck("a Draw did not return within %v of Close with a repack parked", callWait)
		}
		if errors.Is(err, journalv2.ErrClosed) {
			break
		}
		if time.Now().After(deadline) {
			stuck("calls were not refused within %v of Close", callWait)
		}
	}
	close(let)
	copied.Release()

	var err error
	select {
	case err = <-repacked:
	case <-time.After(callWait):
		stuck("the repack in flight at Close did not return within %v of its copy being let go", callWait)
	}
	if !errors.Is(err, journalv2.ErrClosed) {
		stub(t, "Repack", err)
		if err != nil {
			t.Fatalf("the repack in flight at Close returned %v, want it to finish or fail with ErrClosed", err)
		}
	}
	select {
	case err = <-closed:
	case <-time.After(callWait):
		stuck("Close did not return within %v of the repack in flight", callWait)
	}
	stub(t, "Close", err)
	if err != nil {
		t.Fatalf("close with a repack in flight: %v", err)
	}
	if early.Load() {
		t.Fatal("Close returned while the repack in flight was still parked in its copy")
	}

	after := f.Seam.Hold(t, "*", "*")
	f.Advance(2*journalv2test.IdleSealAge + journalv2.SyncBound)
	select {
	case <-after.Reached():
		t.Fatal("the closed journal made a call on its storage")
	case <-time.After(backgroundWait):
	}
	after.Release()

	f.Crash()
	h = f.Share(0)
	for _, e := range kept {
		start := e.Off - e.Off%soleChunk
		got, missing := readBack(t, h, id, e.Off, e.Len)
		if len(missing) != 0 || !bytes.Equal(got, pattern(fmt.Sprint("sole", start), soleChunk)[e.Off-start:e.Off-start+e.Len]) {
			t.Fatalf("%+v reads missing %+v or other bytes after a close with a repack in flight", e, missing)
		}
	}
}

// removalRecords is how many header-only records each half of the drop check
// issues.
const removalRecords = 100_000

// TestRFC1B_RemovalRecordsDroppedPerExtent checks that repack drops a file's
// removal and release records once no other segment holds an older record of
// the same file over their extent: beside another file's filled content at an
// old version, kept for the whole run, and beside the same file's own old
// content outside their extent. A share's records on disk then stay within a
// bound across 10^5 such records.
//
// The segments are the fixture's 1 MiB cap.
//
// decision: the bound on each share's records on disk stands in for the
// headroom staying above half. The headroom is drawn only at the journal's
// limit, and holding the journal there across 10^5 records means refilling
// it after every few hundred, as the releases retire the segments they
// emptied; below the limit the headroom never moves, so a check of it could
// not fail. Records a journal keeps take the share's usage past the bound well
// before they would drain the headroom. Overturned if the headroom is drawn
// below the limit too, when the check reads it again.
func TestRFC1B_RemovalRecordsDroppedPerExtent(t *testing.T) {
	f := journalv2test.New(t)

	// What a share's records may take on disk after a repack: the records in
	// the segments still active, which repack cannot take, and those still
	// needed. Kept, 10^5 header-only records take over ten times this.
	const keptBound = 1 << 20
	checkpoint := func(tag journalv2.ShareTag, base int64, issued int) {
		t.Helper()
		repackAll(t, f, journalv2.RepackScope{})
		s := f.J.Stats()
		if got := s.Shares[tag].UsedBytes - base; got > keptBound {
			t.Fatalf("share %d's records take %d bytes more after %d removal records and a repack, want at most %d: they are kept", tag, got, issued, keptBound)
		}
	}
	const every = removalRecords / 5

	// X's filled content, at a version older than anything below.
	fillRange(t, f.Share(0), journalv2test.File(0), 0, pattern("x-old", 64<<10), drawVersion(t, f))

	h, y := f.Share(1), journalv2test.File(1)
	base := f.J.Stats().Shares[journalv2test.Tag(1)].UsedBytes
	for i := range removalRecords / 3 {
		off := int64(i) * 1024
		e := journalv2.Extent{Off: off, Len: 512}
		mark(t, h, y, e, write(t, h, y, off, pattern(fmt.Sprint("y", i), 512)))
		freeExtent(t, h, y, e)
		_, err := h.Deallocate(t.Context(), y, journalv2.Extent{Off: off, Len: 1024})
		stub(t, "Deallocate", err)
		if err != nil {
			t.Fatalf("deallocate %d of file Y: %v", i, err)
		}
		tr, err := h.Truncate(t.Context(), y, off)
		stub(t, "Truncate", err)
		if err != nil {
			t.Fatalf("truncate %d of file Y: %v", i, err)
		}
		h.Settle(y, tr)
		if (3*i+3)%every < 3 {
			checkpoint(journalv2test.Tag(1), base, 3*i+3)
		}
	}
	checkpoint(journalv2test.Tag(1), base, removalRecords)

	// Z's own filled content over its first MiB, and removals far above it.
	zTag := journalv2test.Tag(2)
	z, zid := limitedShare(t, f, zTag, 0), journalv2test.File(2)
	fillRange(t, z, zid, 0, pattern("z-old", 1<<20), drawVersion(t, f))
	base = f.J.Stats().Shares[zTag].UsedBytes
	for i := range removalRecords / 2 {
		off := int64(1<<30) + int64(i)*8192
		e := journalv2.Extent{Off: off, Len: 512}
		mark(t, z, zid, e, write(t, z, zid, off, pattern(fmt.Sprint("z", i), 512)))
		freeExtent(t, z, zid, e)
		d, err := z.Deallocate(t.Context(), zid, journalv2.Extent{Off: off, Len: 1024})
		stub(t, "Deallocate", err)
		if err != nil {
			t.Fatalf("deallocate %d of file Z: %v", i, err)
		}
		z.Settle(zid, d)
		if (2*i+2)%every < 2 {
			checkpoint(zTag, base, 2*i+2)
		}
	}
	checkpoint(zTag, base, removalRecords)
	if got, missing := readBack(t, z, zid, 0, 1<<20); len(missing) != 0 || !bytes.Equal(got, pattern("z-old", 1<<20)) {
		t.Fatalf("Z's old filled content reads missing %+v or other bytes after the run", missing)
	}
}

// TestRFC1B_RepackAimedAtShare checks that a repack pass aimed at a share at
// its limit frees storage from segments holding that share's records, not
// from sparser segments of another share elsewhere, and lowers the share's
// usage; and that a pass aimed at two segments repacks those two and no other.
//
// decision: A's writes are interleaved with twice as much of B, released, so
// A's segments mostly hold B's unreclaimed bytes, but only where the journal
// puts A's and B's files on one stream; that is its choice. The check holds
// either way: B's other segments are released to one extent in twenty, sparser
// than any of A's, so a pass that chose for itself takes them first. Overturned
// if a file's stream becomes observable, so the check can put both on one.
func TestRFC1B_RepackAimedAtShare(t *testing.T) {
	f := journalv2test.New(t)
	a, b := limitedShare(t, f, limitedA, 2<<20), f.Share(1)
	const size = 64 << 10

	// B's sparser segments: 6 MiB, of which one extent in twenty stays held.
	var sparse []written
	for i := range int64(96) {
		id, off := journalv2test.File(10+int(i%2)), i/2*size
		sparse = append(sparse, written{1, id, journalv2.Extent{Off: off, Len: size}, write(t, b, id, off, pattern(fmt.Sprint("sparse", i), size))})
	}
	// A filled to its limit, interleaved with twice as much of B.
	var aws, bws []written
	for i := int64(0); ; i++ {
		if i > 64 {
			t.Fatalf("%d writes into a share limited to 2 MiB were never refused", i)
		}
		id, off := journalv2test.File(int(i%2)), i/2*size
		v, err := tryWrite(t, a, id, off, pattern(fmt.Sprint("a", i), size))
		stub(t, "WriteAt", err)
		if err != nil {
			refusal(t, err, "a write past share A's limit", journalv2.LimitShare, journalv2.UnitBytes)
			break
		}
		aws = append(aws, written{-1, id, journalv2.Extent{Off: off, Len: size}, v})
		for c := range int64(2) {
			bid := journalv2test.File(12 + int(c))
			bws = append(bws, written{1, bid, journalv2.Extent{Off: i * size, Len: size}, write(t, b, bid, i*size, pattern(fmt.Sprint("b", c, i), size))})
		}
	}
	aSegs := map[journalv2.SegmentID]bool{}
	for _, id := range []journalv2.FileID{journalv2test.File(0), journalv2test.File(1)} {
		for _, e := range indexEntries(t, f, id) {
			aSegs[e.Location.Segment] = true
		}
	}

	for i, w := range sparse {
		if i%20 != 0 {
			evict(t, b, []written{w})
		}
	}
	evict(t, b, bws)
	// Every other write of each of A's files, which alternate: half of each
	// file, so no segment of A's is emptied and retired on its own.
	for i, w := range aws {
		if (i/2)%2 == 0 {
			evict(t, a, []written{w})
		}
	}
	syncNow(t, a, journalv2test.File(0), journalv2test.File(1))
	syncNow(t, b, journalv2test.File(10), journalv2test.File(11), journalv2test.File(12), journalv2test.File(13))
	f.Advance(2 * journalv2test.IdleSealAge)
	eventually(t, "the segments holding share A's records were not sealed once idle", func() bool {
		for s := range aSegs {
			if info, ok := segmentInfo(f, s); ok && !info.Sealed {
				return false
			}
		}
		return true
	})

	// othersRepacked fails the test if a segment of before outside want was
	// retired holding held or retained bytes: one retired holding none was
	// retired on its own, not repacked.
	othersRepacked := func(before []journalv2.SegmentInfo, want func(journalv2.SegmentID) bool, what string) {
		t.Helper()
		for _, s := range before {
			if !want(s.ID) && retired(f, s.ID) && s.HeldBytes+s.RetainedBytes > 0 {
				t.Fatalf("%s repacked segment %d, holding %d held bytes, outside its scope", what, s.ID, s.HeldBytes)
			}
		}
	}

	before := f.J.Inspect().Segments()
	used := f.J.Stats().Shares[limitedA].UsedBytes
	freed, err := f.J.Repack(t.Context(), journalv2test.FixtureSegmentCap, journalv2.RepackScope{Share: limitedA})
	stub(t, "Repack", err)
	if err != nil {
		t.Fatalf("repack aimed at share A: %v", err)
	}
	if freed <= 0 {
		t.Fatal("a repack aimed at share A, over its segments half released, freed nothing")
	}
	othersRepacked(before, func(s journalv2.SegmentID) bool { return aSegs[s] }, "a pass aimed at share A")
	if got := f.J.Stats().Shares[limitedA].UsedBytes; got >= used {
		t.Fatalf("share A uses %d bytes after a pass aimed at it, %d before: want less", got, used)
	}

	// The two owned segments holding the most: the last a pass left to choose
	// for itself would take.
	before = f.J.Inspect().Segments()
	var cands []journalv2.SegmentInfo
	for _, s := range before {
		if s.Sealed && s.HeldBytes > 0 {
			cands = append(cands, s)
		}
	}
	if len(cands) < 2 {
		t.Fatalf("%d sealed segments hold anything, want two to aim a pass at", len(cands))
	}
	slices.SortFunc(cands, func(x, y journalv2.SegmentInfo) int { return cmp.Compare(y.HeldBytes, x.HeldBytes) })
	two := []journalv2.SegmentID{cands[0].ID, cands[1].ID}
	freed, err = f.J.Repack(t.Context(), journalv2test.FixtureMaxBytes, journalv2.RepackScope{Segments: two})
	stub(t, "Repack", err)
	if err != nil {
		t.Fatalf("repack aimed at two segments: %v", err)
	}
	for _, s := range two {
		if !retired(f, s) {
			t.Fatalf("segment %d is still owned after a pass aimed at it", s)
		}
	}
	if freed < cands[0].Allocated+cands[1].Allocated {
		t.Fatalf("a pass aimed at two segments freed %d, want at least their %d", freed, cands[0].Allocated+cands[1].Allocated)
	}
	othersRepacked(before, func(s journalv2.SegmentID) bool { return slices.Contains(two, s) }, "a pass aimed at two segments")
}

// descriptorSlack is how many descriptors past the journal's bound the
// process may hold open while a journal runs.
//
// decision: the process's count also holds what the journal opens besides
// segment descriptors, its directory, its format file and the spares it is
// making, so it is held to the bound plus this, not to the bound alone. The
// ceiling: a journal holding up to this many segment descriptors past its
// bound passes the process's half of the check, and only Stats catches it.
// Overturned once Stats reports the descriptors it holds by kind.
const descriptorSlack = 8

// TestRFC1B_Descriptors checks that a journal with more segments than its
// descriptor bound reads every one of them, reopening the descriptors it
// closed, while the descriptors it holds stay within the bound by Stats and
// by the process's own count.
//
// The journal is given a budget of 16 descriptors, against 40 segments at the
// fixture's 1 MiB cap.
func TestRFC1B_Descriptors(t *testing.T) {
	const budget, size = 16, 256 << 10
	// The process's count is taken before the journal opens, so what it opens
	// at start counts against its bound too. A first listing goes before it,
	// so descriptors the runtime opens on its first file are not counted.
	prof.Descriptors()
	base, counted := prof.Descriptors()
	if !counted {
		t.Log("the process's descriptors cannot be listed here; only Stats bounds them")
	}
	f := journalv2test.NewWithOptions(t, nil, journalv2.WithDescriptorBudget(journalv2.NewDescriptorBudget(budget)))
	bound := f.J.Stats().DescriptorLimit
	if bound != budget {
		t.Fatalf("DescriptorLimit = %d under a budget of %d, want the budget", bound, budget)
	}
	bounded := func(what string) {
		t.Helper()
		s := f.J.Stats()
		if s.OpenDescriptors > s.DescriptorLimit {
			t.Fatalf("%s: OpenDescriptors = %d, past DescriptorLimit %d", what, s.OpenDescriptors, s.DescriptorLimit)
		}
		if !counted {
			return
		}
		n, ok := prof.Descriptors()
		if !ok {
			t.Fatalf("%s: the process's descriptors, listed before the journal opened, can no longer be listed", what)
		}
		if n-base > bound+descriptorSlack {
			t.Fatalf("%s: the process holds %d descriptors more than before the journal opened, past its bound of %d and %d more", what, n-base, bound, descriptorSlack)
		}
	}

	var ws []written
	for i := int64(0); f.J.Stats().Segments <= 2*bound+8; i++ {
		if i*size > journalv2test.FixtureMaxBytes {
			t.Fatalf("%d segments after %d bytes written, want more than %d", f.J.Stats().Segments, i*size, 2*bound+8)
		}
		k := int(i % 2)
		id, off := journalv2test.File(k), i/2*size
		v := write(t, f.Share(k), id, off, pattern(fmt.Sprint("desc", i), size))
		ws = append(ws, written{k, id, journalv2.Extent{Off: off, Len: size}, v})
		bounded("while writing")
	}
	for _, k := range []int{0, 1} {
		syncNow(t, f.Share(k), journalv2test.File(k))
	}

	reopens := f.J.Stats().Counters.SegmentReopens
	for i, w := range ws {
		buf := make([]byte, size)
		_, missing, _, _, err := f.Share(w.share).ReadAt(t.Context(), w.id, w.extent.Off, buf)
		stub(t, "ReadAt", err)
		if err != nil {
			t.Fatalf("read write %d back with more segments than descriptors: %v", i, err)
		}
		if len(missing) != 0 || !bytes.Equal(buf, pattern(fmt.Sprint("desc", i), size)) {
			t.Fatalf("write %d reads back missing %+v or other bytes with more segments than descriptors", i, missing)
		}
		bounded("while reading")
	}
	if got := f.J.Stats().Counters.SegmentReopens; got <= reopens {
		t.Fatalf("SegmentReopens moved by %d over reads of %d segments under a bound of %d, want reopens counted", got-reopens, f.J.Stats().Segments, bound)
	}
}
