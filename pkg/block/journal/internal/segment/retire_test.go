package segment_test

import (
	"errors"
	"io/fs"
	"log/slog"
	"runtime"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// retireEnv is one retirer over a real directory reached through the seam: a
// ledger, a spare maker, a pool of t and a descriptor cache on its own
// budget. retire drives a segment through the states a retirement needs — a
// spare made, taken as a segment and sealed — writing the "old life" bytes a
// recycle must keep (the middle) and the two slots it must zero.
type retireEnv struct {
	t    *testing.T
	seam *journaltest.Seam
	set  *segment.Set
	m    *segment.Maker
	c    *segment.Descriptors
	p    *segment.Pool
	r    *segment.Retirer
}

// noPool is the targets a retirer with no pool is built on: the negative
// content target says "no pool at all", so every retirement unlinks. Targets
// of zeroes would still build a pool — one that recycles nothing but kicks
// on every unlink.
var noPool = segment.Targets{Content: -1}

// retirer builds a retirer over a fresh seam. targets is the pool's; noPool
// means every retirement unlinks. readOnly is the R89 check, nil meaning
// writable. durable collects the durable-unlink signal's calls, when the
// test needs it.
func retirer(t *testing.T, targets segment.Targets, readOnly func() error) (*retireEnv, *durableCollector) {
	t.Helper()
	return retirerBuilt(t, targets, readOnly, 0, nil)
}

// retirerLogged is retirer with a logger the test reads back.
func retirerLogged(t *testing.T, targets segment.Targets, readOnly func() error, h slog.Handler) (*retireEnv, *durableCollector) {
	t.Helper()
	return retirerBuilt(t, targets, readOnly, 0, slog.New(h))
}

// retirerSeeded is retirer with the Maker seeded at last, the way open seeds
// it from the largest id the directory names.
func retirerSeeded(t *testing.T, targets segment.Targets, readOnly func() error, last core.SegmentID) (*retireEnv, *durableCollector) {
	t.Helper()
	return retirerBuilt(t, targets, readOnly, last, nil)
}

// retirerBuilt is retirer with every knob a test needs: the read-only check,
// the Maker's seed and a logger for the retirer and its ledger. The durable
// signal's slice is handed out guarded: the worker goroutine is the one that
// appends to it, so a test must read it through the collector's own lock.
func retirerBuilt(t *testing.T, targets segment.Targets, readOnly func() error, last core.SegmentID, log *slog.Logger) (*retireEnv, *durableCollector) {
	t.Helper()

	seam := newSeam(t)
	set := segment.NewSet(segCap, log)
	m := segment.NewMaker(seam.FS(), set, noMax, last, nil)
	c := segment.NewDescriptors(seam.FS(), format.SegmentName, core.NewDescriptorBudget(64))
	var p *segment.Pool
	if targets.Content >= 0 {
		p = segment.NewPool(m, targets, nil)
	}
	dc := &durableCollector{}
	r := segment.NewRetirer(segment.RetirerConfig{
		Set:      set,
		Cache:    c,
		Pool:     p,
		Maker:    m,
		FS:       seam.FS(),
		ReadOnly: readOnly,
		Durable:  dc.add,
		Log:      log,
	})
	return &retireEnv{t: t, seam: seam, set: set, m: m, c: c, p: p, r: r}, dc
}

// durableCollector keeps the durable signal's calls under a mutex: the
// worker goroutine is the one that makes them.
type durableCollector struct {
	mu   sync.Mutex
	recs []segment.Retirement
}

func (d *durableCollector) add(r segment.Retirement) {
	d.mu.Lock()
	d.recs = append(d.recs, r)
	d.mu.Unlock()
}

// len is how many signals have arrived so far.
func (d *durableCollector) len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.recs)
}

// all is a copy of the arrivals, for a test's assertions.
func (d *durableCollector) all() []segment.Retirement {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]segment.Retirement(nil), d.recs...)
}

// at is the arrival at i.
func (d *durableCollector) at(i int) segment.Retirement {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.recs[i]
}

// segment makes one spare, takes it into the set and seals it with the full
// word named, after writing the old life's bytes: a non-zero header slot, a
// non-zero middle and a non-zero trailer slot, the two slots a recycle must
// zero and the middle it must leave alone.
func (e *retireEnv) segment(full bool) core.SegmentID {
	e.t.Helper()

	id, err := e.m.Make()
	if err != nil {
		e.t.Fatalf("the segment's making: %v", err)
	}
	if err := e.set.Take(id); err != nil {
		e.t.Fatalf("the segment's takeover: %v", err)
	}
	e.oldLife(id)
	if err := e.set.Seal(id, full); err != nil {
		e.t.Fatalf("the segment's seal: %v", err)
	}
	return id
}

// oldLife writes the bytes of a segment's old life: a non-zero header slot,
// a non-zero middle and a non-zero trailer slot at the cap's end, so a test
// can tell what a recycle zeroed from what it kept.
func (e *retireEnv) oldLife(id core.SegmentID) {
	e.t.Helper()

	f, err := e.seam.FS().Open(format.SegmentName(id))
	if err != nil {
		e.t.Fatalf("the old life's open of %d: %v", id, err)
	}
	header := make([]byte, format.SegmentHeaderSize)
	middle := make([]byte, 64)
	trailer := make([]byte, format.TrailerSize)
	for i := range header {
		header[i] = 0xAA
	}
	for i := range middle {
		middle[i] = 0xBB
	}
	for i := range trailer {
		trailer[i] = 0xCC
	}
	if _, err := f.WriteAt(header, 0); err != nil {
		e.t.Fatalf("the old life's header write: %v", err)
	}
	if _, err := f.WriteAt(middle, segCap/2); err != nil {
		e.t.Fatalf("the old life's middle write: %v", err)
	}
	if _, err := f.WriteAt(trailer, segCap-format.TrailerSize); err != nil {
		e.t.Fatalf("the old life's trailer write: %v", err)
	}
	// The old life is synced before the test hands the segment over, so a
	// crash between a recycle's steps leaves its bytes — the crash checks
	// below read them — rather than the zeros the file was made of.
	if err := f.Sync(); err != nil {
		e.t.Fatalf("the old life's sync: %v", err)
	}
	if err := f.Close(); err != nil {
		e.t.Fatalf("the old life's close: %v", err)
	}
}

// ended makes one spare, takes it into the set and ends it, with the old
// life's bytes written as segment does.
func (e *retireEnv) ended() core.SegmentID {
	e.t.Helper()

	id, err := e.m.Make()
	if err != nil {
		e.t.Fatalf("the ended segment's making: %v", err)
	}
	if err := e.set.Take(id); err != nil {
		e.t.Fatalf("the ended segment's takeover: %v", err)
	}
	e.oldLife(id)
	if err := e.set.End(id); err != nil {
		e.t.Fatalf("the ended segment's ending: %v", err)
	}
	return id
}

// bytesAt reads len(p) bytes at off of the file name, failing the test on any
// read error.
func (e *retireEnv) bytesAt(name string, p []byte, off int64) {
	e.t.Helper()

	f, err := e.seam.FS().Open(name)
	if err != nil {
		e.t.Fatalf("the read of %s: %v", name, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.ReadAt(p, off); err != nil {
		e.t.Fatalf("the read of %s at %d: %v", name, off, err)
	}
}

// assertZeroed fails the test unless the header slot and the trailer slot of
// the file name hold zeros and its middle still holds the old life's bytes.
func (e *retireEnv) assertZeroed(name string) {
	e.t.Helper()

	header := make([]byte, format.SegmentHeaderSize)
	e.bytesAt(name, header, 0)
	for i, b := range header {
		if b != 0 {
			e.t.Fatalf("%s: byte %d of the header slot is %#x, want the zeros a recycle wrote", name, i, b)
		}
	}
	trailer := make([]byte, format.TrailerSize)
	e.bytesAt(name, trailer, segCap-format.TrailerSize)
	for i, b := range trailer {
		if b != 0 {
			e.t.Fatalf("%s: byte %d of the trailer slot is %#x, want the zeros a recycle wrote", name, i, b)
		}
	}
	middle := make([]byte, 64)
	e.bytesAt(name, middle, segCap/2)
	for i, b := range middle {
		if b != 0xBB {
			e.t.Fatalf("%s: byte %d of the middle is %#x, want the old life's bytes kept", name, i, b)
		}
	}
}

// TestRetireRecycleCallLogAndFootprint: a full-sealed segment retired with
// the pool below its target is recycled, and the seam sees exactly the order
// RFC 28 §2.2 names: one rename to the fresh name, one directory sync, two
// zero writes — the header slot [0,48) and the trailer slot [cap-32,cap) —
// and one file sync, no allocation, no remove and no other write. The
// footprint does not move, the spare count rises by one, the fresh spare
// lands ready in the pool's class, and freed_bytes rises by the cap — a
// recycle returns the cap to the spare pool, as the public counter's own
// comment says. It catches a recycle that zeroes before the rename, one that
// zero-fills the whole segment, and one that moves the accounted footprint.
func TestRetireRecycleCallLogAndFootprint(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// decision: the pool is never started, so the worker's own makes stay
	// out of the call log: a recycle is served before Start — open's
	// emptying retires segments before the pool starts, and the recycle
	// syncs its own rename — which is exactly what the call log wants to pin.
	e, durable := retirer(t, segment.Targets{Content: 1}, nil)
	old := e.segment(true)
	before := e.set.Counters()

	// The call counts from here on are the retirement's own: the segment's
	// making is behind them.
	syncDirs := e.seam.Calls(journaltest.OpSyncDir)
	removes := e.seam.Calls(journaltest.OpRemove)
	allocs := e.seam.Calls(journaltest.OpAllocate)

	res, err := e.r.Retire(old, segment.NotHeld)
	if err != nil {
		t.Fatalf("the retire of the full-sealed segment: %v", err)
	}
	if !res.Recycled {
		t.Fatalf("the retirement recycled nothing: %+v, want a recycle with the pool below its target", res)
	}
	if res.Fresh == 0 || res.Fresh == old {
		t.Fatalf("the recycle's fresh id is %d, want a fresh one above %d", res.Fresh, old)
	}
	fresh := format.SegmentName(res.Fresh)

	// The call log: one rename, one directory sync, the two zero writes and
	// one sync between them, in this order, and nothing else beside the close
	// of the fresh descriptor.
	if got := e.seam.CallsFor(journaltest.OpRename, format.SegmentName(old)); got != 1 {
		t.Fatalf("the rename of %d was called %d times, want once", old, got)
	}
	if got := e.seam.Calls(journaltest.OpSyncDir) - syncDirs; got != 1 {
		t.Fatalf("the recycle synced the directory %d times, want once", got)
	}
	if got := e.seam.CallsFor(journaltest.OpWrite, fresh); got != 2 {
		t.Fatalf("the recycle wrote %d times through the fresh name, want the two zero writes", got)
	}
	if got := e.seam.CallsFor(journaltest.OpSync, fresh); got != 1 {
		t.Fatalf("the recycle synced the zeroing %d times, want once", got)
	}
	if got := e.seam.Calls(journaltest.OpAllocate) - allocs; got != 0 {
		t.Fatalf("the recycle allocated %d times, want none: its blocks are already there", got)
	}
	if got := e.seam.Calls(journaltest.OpRemove) - removes; got != 0 {
		t.Fatalf("the recycle removed %d files, want none", got)
	}
	// The zeroed slots and the kept middle.
	e.assertZeroed(fresh)
	// The old name is gone.
	if slices.Contains(listNames(t, e.seam), format.SegmentName(old)) {
		t.Fatalf("after the recycle the directory still holds the old name %s", format.SegmentName(old))
	}

	// The footprint did not move; the spare pool rose by one.
	after := e.set.Counters()
	if after.UsedBytes != before.UsedBytes {
		t.Fatalf("across the recycle the footprint moved from %d to %d, want it standing", before.UsedBytes, after.UsedBytes)
	}
	if after.Spares != before.Spares+1 {
		t.Fatalf("after the recycle the ledger holds %d spares, want one more than %d", after.Spares, before.Spares)
	}
	if after.Segments != before.Segments-1 {
		t.Fatalf("after the recycle the set holds %d segments, want one less than %d", after.Segments, before.Segments)
	}
	if err := e.set.Check(); err != nil {
		t.Fatalf("%v", err)
	}

	// freed_bytes rose by the cap: a recycle returns the segment to the spare
	// pool.
	if c := e.r.Counters(); c.FreedBytes != uint64(segCap) {
		t.Fatalf("freed_bytes is %d after the recycle, want the cap %d", c.FreedBytes, segCap)
	}

	// Durable: the rename's directory sync returned.
	if !res.Durable {
		t.Fatal("the recycle's retirement is not durable, want the rename's directory sync back")
	}
	if durable.len() != 1 || durable.at(0).ID != old {
		t.Fatalf("the durable signal was called for %+v, want once for %d", durable.all(), old)
	}

	// The fresh spare sits ready in the pool's content class, and a started
	// pool hands it to a take: the worker's own makes are past the call log's
	// window by now, and the pool settles at its target on this spare alone.
	if e.p == nil {
		t.Fatal("the test's pool went missing")
	}
	e.p.Start()
	if id, err := e.p.Take(segment.Content); err != nil || id != res.Fresh {
		t.Fatalf("the take of the recycled spare: (%d, %v), want %d", id, err, res.Fresh)
	}
	// The take empties the class again, and the completed recycle spent its
	// reservation: the pool wants the next one. A recycled landing that
	// never paid its slot back leaves the class reading one-short-forever,
	// and every recycle after the first leaks its slot instead.
	if class, ok := e.p.WantRecycleForTest(); !ok || class != segment.Content {
		t.Fatalf("after the recycled spare was taken the pool wants no recycle: (%v, %v), want (content, true): a completed recycle must spend the slot it reserved", class, ok)
	}
	e.p.Stop()
	if got := e.seam.Calls(journaltest.OpAllocate) - allocs; got != 0 {
		t.Fatalf("the retirement and the take allocated %d times, want none beside the segment's own making", got)
	}
}

// TestRetireFreshIDEveryNameInPlace: the id the recycle drew exceeds every id
// the directory names, a `.spare.tmp` a crashed make left durable included,
// so no record on disk ever carries the id a recycle takes (R16).
func TestRetireFreshIDEveryNameInPlace(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// A `.spare.tmp` above every id the rest of the test draws is seeded
	// into the directory first — the shape a crashed make's leftover takes —
	// and the Maker is seeded above it, as open seeds it from the largest id
	// the directory names: the fresh id the recycle draws must pass the
	// stray, whatever the process's own makes were.
	stray := format.SpareName(core.SegmentID(4096))
	e, _ := retirerSeeded(t, segment.Targets{Content: 1}, nil, segment.HighestID([]string{stray}))
	if f, err := e.seam.FS().Create(stray); err != nil {
		t.Fatalf("the stray spare's creation: %v", err)
	} else if err := f.Close(); err != nil {
		t.Fatalf("the stray spare's close: %v", err)
	}
	if err := e.seam.SyncDir(); err != nil {
		t.Fatalf("the stray's directory sync: %v", err)
	}

	old := e.segment(true)
	res, err := e.r.Retire(old, segment.NotHeld)
	if err != nil {
		t.Fatalf("the retire: %v", err)
	}
	if !res.Recycled {
		t.Fatalf("the retirement recycled nothing: %+v", res)
	}
	if res.Fresh <= core.SegmentID(4096) {
		t.Fatalf("the recycle drew the fresh id %d, want one above the durable stray 4096", res.Fresh)
	}
	for _, name := range listNames(t, e.seam) {
		switch kind, id := format.ClassifyName(name); kind {
		case format.NameSegment, format.NameSpare:
			// The fresh name itself is the one id allowed at the fresh id;
			// nothing may stand above it.
			if id > res.Fresh {
				t.Fatalf("the directory holds %s at id %d, want every name at or below the recycle's fresh %d", name, id, res.Fresh)
			}
		}
	}
	e.p.Stop()
}

// TestRetireDecidesAtRetirement: whether a full-size segment is recycled is
// decided at the retirement itself, never at the seal — the pool's target
// moved between the two must change the answer (R42).
func TestRetireDecidesAtRetirement(t *testing.T) {
	prof.Watch(t, prof.Options{})

	t.Run("sealed at target, retired below it", func(t *testing.T) {
		// The pool fills its one-spare target first, so the seal happens
		// with the pool at target; the spare is then taken away and the
		// retirement finds the pool below it: a recycle.
		e, _ := retirer(t, segment.Targets{Content: 1}, nil)
		if e.p != nil {
			e.p.Start()
		}
		// Fill the pool's target with one spare made directly.
		if _, err := e.m.Make(); err != nil {
			t.Fatalf("the pool's own spare: %v", err)
		}
		// The maker's second spare is drawn into the pool's class by the
		// refill: adopt the one the fill made instead of racing it.
		awaitCounters(t, "the pool at its target", e.p,
			func(c segment.PoolCounters) bool { return c.Spares == 1 })

		old := e.segment(true)
		// Take the spare the pool holds, so the retirement finds the pool
		// below its target.
		if _, err := e.p.Take(segment.Content); err != nil {
			t.Fatalf("the take that empties the pool: %v", err)
		}
		res, err := e.r.Retire(old, segment.NotHeld)
		if err != nil {
			t.Fatalf("the retire below target: %v", err)
		}
		if !res.Recycled {
			t.Fatalf("the retire below target recycled nothing: %+v, want a recycle", res)
		}
		e.p.Stop()
	})

	t.Run("sealed below target, retired at it", func(t *testing.T) {
		// The seal happens with the pool empty — below its target — and the
		// retirement finds it full: an unlink. A recycle decided at the seal
		// would show here as a recycle.
		e, _ := retirer(t, segment.Targets{Content: 1}, nil)
		if e.p != nil {
			e.p.Start()
		}
		awaitCounters(t, "the pool at its target", e.p,
			func(c segment.PoolCounters) bool { return c.Spares == 1 })

		old := e.segment(true)
		res, err := e.r.Retire(old, segment.NotHeld)
		if err != nil {
			t.Fatalf("the retire at target: %v", err)
		}
		if res.Recycled {
			t.Fatalf("the retire at target recycled: %+v, want an unlink with the pool at its target", res)
		}
		if got := e.seam.Calls(journaltest.OpRemove); got != 1 {
			t.Fatalf("the unlink at target removed %d files, want one", got)
		}
		if c := e.set.Counters(); c.Spares != 1 || c.UsedBytes != segCap {
			t.Fatalf("after the unlink the ledger holds %+v, want the pool's one spare at the cap alone", c)
		}
		e.p.Stop()
	})
}

// TestRetireSealedEarlyIsUnlinked: a segment sealed early is unlinked when
// retired, never recycled — including one whose truncation failed, which
// stays Sealed and accounted at its cap with the full word false (R43, R35).
// The seam assertions are the decision's own: an executor that chose a recycle
// would rename the segment before anything else and write through the fresh
// name, so the retire of an early seal must rename and write nothing.
func TestRetireSealedEarlyIsUnlinked(t *testing.T) {
	prof.Watch(t, prof.Options{})

	assertNeverRecycled := func(t *testing.T, e *retireEnv, renames, writes int) {
		t.Helper()
		if got := e.seam.Calls(journaltest.OpRename); got != renames {
			t.Fatalf("the early seal's retire renamed %d times, want %d: an early seal is never a recycle's candidate", got, renames)
		}
		if got := e.seam.Calls(journaltest.OpWrite); got != writes {
			t.Fatalf("the early seal's retire wrote %d times, want %d: the recycle's zeroing it would have run never did", got, writes)
		}
	}

	t.Run("after a shrink", func(t *testing.T) {
		e, _ := retirer(t, segment.Targets{Content: 1}, nil)
		old := e.segment(false)
		if err := e.set.Shrink(old, segCap/2); err != nil {
			t.Fatalf("the early seal's truncation: %v", err)
		}
		renames := e.seam.Calls(journaltest.OpRename)
		writes := e.seam.Calls(journaltest.OpWrite)
		res, err := e.r.Retire(old, segment.NotHeld)
		if err != nil {
			t.Fatalf("the retire of the early-sealed segment: %v", err)
		}
		if res.Recycled {
			t.Fatalf("the early-sealed segment was recycled: %+v, want an unlink", res)
		}
		assertNeverRecycled(t, e, renames, writes)
		if res.Freed != segCap/2 {
			t.Fatalf("the early seal's unlink freed %d, want the shrunk size %d", res.Freed, segCap/2)
		}
		if c := e.set.Counters(); c.UsedBytes != 0 {
			t.Fatalf("after the unlink the footprint is %d, want 0", c.UsedBytes)
		}
	})

	t.Run("a failed truncation, at the cap, pool below target", func(t *testing.T) {
		// The seal named early and the truncation never ran: the segment
		// stays at its cap, a full-size file, and is still unlinked — the
		// seal's own word, not the size, is what a recycle trusts.
		e, _ := retirer(t, segment.Targets{Content: 1}, nil)
		old := e.segment(false)
		renames := e.seam.Calls(journaltest.OpRename)
		writes := e.seam.Calls(journaltest.OpWrite)
		res, err := e.r.Retire(old, segment.NotHeld)
		if err != nil {
			t.Fatalf("the retire of the failed truncation: %v", err)
		}
		if res.Recycled {
			t.Fatalf("a segment sealed early was recycled: %+v, want an unlink", res)
		}
		assertNeverRecycled(t, e, renames, writes)
		if res.Freed != segCap {
			t.Fatalf("the failed truncation's unlink freed %d, want the cap", res.Freed)
		}
	})
}

// TestRetireEndedIsUnlinkedNeverRecycled: an ended segment is retired only
// with the reason Ended, is unlinked and never recycled, even with the pool
// below its target; the wrong pair of reason and state is refused with
// ErrWrongReason, no seam call is made, and the claim is released so the
// right reason finds it free (R43, R82).
func TestRetireEndedIsUnlinkedNeverRecycled(t *testing.T) {
	prof.Watch(t, prof.Options{})

	t.Run("ended, unlinked with the pool below target", func(t *testing.T) {
		e, _ := retirer(t, segment.Targets{Content: 1}, nil)
		id := e.ended()

		calls := e.seam.Calls(journaltest.OpRemove)
		renames := e.seam.Calls(journaltest.OpRename)
		writes := e.seam.Calls(journaltest.OpWrite)
		res, err := e.r.Retire(id, segment.ReasonEnded)
		if err != nil {
			t.Fatalf("the retire of the ended segment: %v", err)
		}
		if res.Recycled {
			t.Fatalf("the ended segment was recycled: %+v, want an unlink", res)
		}
		// The decision's own proof: an ended segment is never a recycle's
		// candidate, so its retire renames and writes nothing.
		if got := e.seam.Calls(journaltest.OpRename) - renames; got != 0 {
			t.Fatalf("the ended segment's retire renamed %d times, want none: an ended segment is never recycled", got)
		}
		if got := e.seam.Calls(journaltest.OpWrite) - writes; got != 0 {
			t.Fatalf("the ended segment's retire wrote %d times, want none: the recycle's zeroing it would have run never did", got)
		}
		if got := e.seam.Calls(journaltest.OpRemove) - calls; got != 2 {
			// One for the segment's own name and one for the spare's, the
			// name a failed make's leftover may sit under.
			t.Fatalf("the ended segment's unlink removed %d files, want the two names of its id", got)
		}
		if res.Freed != segCap {
			t.Fatalf("the ended segment's unlink freed %d, want the cap", res.Freed)
		}
		if c := e.set.Counters(); c.UsedBytes != 0 || c.Segments != 0 {
			t.Fatalf("after the unlink the ledger holds %+v, want it empty", c)
		}
	})

	t.Run("ended with NotHeld: refused, no seam call, claim released", func(t *testing.T) {
		e, _ := retirer(t, segment.Targets{Content: 1}, nil)
		id := e.ended()

		fault := &countingFault{}
		e.seam.SetFault(fault)
		if _, err := e.r.Retire(id, segment.NotHeld); !errors.Is(err, segment.ErrWrongReason) {
			t.Fatalf("the retire of an ended segment with NotHeld: %v, want %v", err, segment.ErrWrongReason)
		}
		if fault.calls > 0 {
			t.Fatalf("the refused retire made %d seam calls, want none", fault.calls)
		}
		e.seam.SetFault(nil)

		// The claim is released: the right reason finds it free.
		if _, _, err := e.set.ClaimRetirement(id); err != nil {
			t.Fatalf("the claim after the refused retire was not free: %v", err)
		}
		e.set.Release(id)
		res, err := e.r.Retire(id, segment.ReasonEnded)
		if err != nil {
			t.Fatalf("the retire with the right reason after the refusal: %v", err)
		}
		if res.Recycled || res.Freed != segCap {
			t.Fatalf("the retire after the refusal recycled or freed wrong: %+v", res)
		}
	})

	t.Run("sealed with Ended: refused", func(t *testing.T) {
		e, _ := retirer(t, segment.Targets{Content: 1}, nil)
		id := e.segment(true)
		fault := &countingFault{}
		e.seam.SetFault(fault)
		if _, err := e.r.Retire(id, segment.ReasonEnded); !errors.Is(err, segment.ErrWrongReason) {
			t.Fatalf("the retire of a sealed segment with Ended: %v, want %v", err, segment.ErrWrongReason)
		}
		if fault.calls > 0 {
			t.Fatalf("the refused retire made %d seam calls, want none", fault.calls)
		}
		e.seam.SetFault(nil)
		e.set.Release(id)
		if _, err := e.r.Retire(id, segment.NotHeld); err != nil {
			t.Fatalf("the retire with the right reason: %v", err)
		}
	})
}

// countingFault counts the calls the seam consults it on, so a test can
// assert a refusal made none.
type countingFault struct {
	mu    sync.Mutex
	calls int
}

func (f *countingFault) At(string, []byte) (time.Duration, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return 0, 0, nil
}

// TestRetireRefusals: an Active segment, a spare of the pool and a segment
// already claimed are each refused without a seam call and without a double
// retire: the executor never names live storage a stream still appends to,
// a spare the pool owns, or a segment another path holds (R82).
func TestRetireRefusals(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, _ := retirer(t, segment.Targets{Content: 1}, nil)

	// An Active segment: refused ErrWrongState.
	active, err := e.m.Make()
	if err != nil {
		t.Fatalf("the active segment's making: %v", err)
	}
	if err := e.set.Take(active); err != nil {
		t.Fatalf("the active segment's takeover: %v", err)
	}
	// A spare of the pool.
	spare, err := e.m.Make()
	if err != nil {
		t.Fatalf("the pool spare's making: %v", err)
	}
	// A claimed segment.
	claimed := e.segment(true)
	if !e.set.Claim(claimed) {
		t.Fatal("the claim on the sealed segment was refused")
	}
	before := e.set.Counters()

	fault := &countingFault{}
	e.seam.SetFault(fault)
	if _, err := e.r.Retire(active, segment.NotHeld); !errors.Is(err, segment.ErrWrongState) {
		t.Fatalf("the retire of an Active segment: %v, want %v", err, segment.ErrWrongState)
	}
	if _, err := e.r.Retire(spare, segment.NotHeld); !errors.Is(err, segment.ErrWrongState) {
		t.Fatalf("the retire of a pool spare: %v, want %v", err, segment.ErrWrongState)
	}
	if _, err := e.r.Retire(claimed, segment.NotHeld); !errors.Is(err, segment.ErrClaimed) {
		t.Fatalf("the retire of a claimed segment: %v, want %v", err, segment.ErrClaimed)
	}
	if fault.calls > 0 {
		t.Fatalf("the refused retires made %d seam calls, want none", fault.calls)
	}
	e.seam.SetFault(nil)

	after := e.set.Counters()
	if after != before {
		t.Fatalf("the refused retires changed the counters: %+v, want %+v", after, before)
	}
	if c := e.r.Counters(); c.FreedBytes != 0 {
		t.Fatalf("the refused retires freed %d bytes, want none", c.FreedBytes)
	}
	// The live claim is untouched by the refusal: the executor took none it
	// could release, so the next retirement of the segment still meets the
	// claim it must.
	if _, _, err := e.set.ClaimRetirement(claimed); !errors.Is(err, segment.ErrClaimed) {
		t.Fatalf("the claim after the refused retire: %v, want %v", err, segment.ErrClaimed)
	}
}

// TestRetireAFailedMakeLeftover: a spare whose making failed and whose unlink
// failed is Ended in the set with a file under its spare's temporary name —
// the leftover a retirement unlinks under whichever of the id's two names it
// finds (R83).
func TestRetireAFailedMakeLeftover(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, tc := range []struct {
		name       string
		failsAfter string // the make's step whose failure leaves the leftover
	}{
		{"leftover under the spare's own name", journaltest.OpAllocate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seam := newSeam(t)
			set := segment.NewSet(segCap, nil)
			applier := &renameApplier{FS: seam.FS(), removeFails: true}
			m := segment.NewMaker(applier, set, noMax, 0, nil)

			// A make that fails at its rename, applied: the file lands under
			// the segment name, and the cleanup's removes fail, so the
			// leftover is Ended in the set.
			if _, err := m.Make(); err == nil {
				t.Fatal("the make whose rename applies and fails: no error")
			}
			id := core.SegmentID(1)
			if c := set.Counters(); c.Segments != 1 || c.UsedBytes != segCap {
				t.Fatalf("after the failed make the ledger holds %+v, want the leftover at its cap", c)
			}

			e := &retireEnv{t: t, seam: seam, set: set, m: m,
				c: segment.NewDescriptors(seam.FS(), format.SegmentName, core.NewDescriptorBudget(64))}
			e.r = segment.NewRetirer(segment.RetirerConfig{
				Set: set, Cache: e.c, Maker: m, FS: applier,
			})

			// The remove must stop failing for the retirement's unlink.
			applier.removeFails = false
			res, err := e.r.Retire(id, segment.ReasonEnded)
			if err != nil {
				t.Fatalf("the retire of the leftover: %v", err)
			}
			if res.Recycled || res.Freed != segCap {
				t.Fatalf("the leftover's retire recycled or freed wrong: %+v", res)
			}
			segs, spares := classify(listNames(t, seam))
			if len(segs)+len(spares) != 0 {
				t.Fatalf("after the leftover's retire the directory holds %v and %v, want nothing", segs, spares)
			}
			if c := set.Counters(); c.UsedBytes != 0 {
				t.Fatalf("after the leftover's retire the footprint is %d, want 0", c.UsedBytes)
			}
		})
	}
}

// TestRetireRecycleFailureIsAnUnlink: a recycle that fails at any of its
// steps — the rename, the directory sync after it, a zeroing write, the
// zeroing's sync, and a rename that applies and still fails — is an unlink
// under whichever of its two names the journal finds, never a spare: both
// names leave the directory, the pool's classes and reservations stand where
// they were, the footprint is lowered by the cap exactly once, and the
// retirement reports no recycle (R41).
func TestRetireRecycleFailureIsAnUnlink(t *testing.T) {
	prof.Watch(t, prof.Options{})

	type step struct {
		name  string
		rules func(e *retireEnv) []journaltest.Rule // the seam's failing rules
	}
	steps := []step{
		{"a rename that does not apply", func(e *retireEnv) []journaltest.Rule {
			return []journaltest.Rule{journaltest.Failing(journaltest.OpRename, "*", 1, syscall.EIO)}
		}},
		{"the directory sync after it", func(e *retireEnv) []journaltest.Rule {
			return []journaltest.Rule{journaltest.Failing(journaltest.OpSyncDir, "*", 1, syscall.EIO)}
		}},
		{"the first zeroing write", func(e *retireEnv) []journaltest.Rule {
			return []journaltest.Rule{journaltest.Failing(journaltest.OpWrite, "*", 1, syscall.EIO)}
		}},
		{"the zeroing's sync", func(e *retireEnv) []journaltest.Rule {
			return []journaltest.Rule{journaltest.Failing(journaltest.OpSync, "*", 3, syscall.EIO)}
		}},
	}

	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			e, _ := retirer(t, segment.Targets{Content: 1}, nil)

			// The failing rule must name the file it fails, which the test
			// cannot know for the rename: the segment's own name is the
			// rename's key, and the fresh name is the writes' and their
			// sync's.
			old := e.segment(true)
			before := e.set.Counters()

			rules := st.rules(e)
			if err := e.seam.Schedule(rules...); err != nil {
				t.Fatalf("Schedule: %v", err)
			}

			res, err := e.r.Retire(old, segment.NotHeld)
			// A fallback unlink that succeeds is a completed retirement: no
			// error, for a retry would count the retired segment a double
			// retire; the failed step is the Warn's to name.
			if err != nil {
				t.Fatalf("the recycle that failed at %s: %v, want nil: the fallback unlink retired the segment", st.name, err)
			}
			if res.Recycled {
				t.Fatalf("the failed recycle was reported as one: %+v", res)
			}
			if res.Freed != segCap {
				t.Fatalf("the failed recycle's unlink freed %d, want the cap %d", res.Freed, segCap)
			}

			// Both names are gone, whichever the failed rename left the
			// file under.
			segs, spares := classify(listNames(t, e.seam))
			if len(segs) != 0 || len(spares) != 0 {
				t.Fatalf("after the failed recycle the directory holds %v and %v, want neither name", segs, spares)
			}

			// The ledger lowered the footprint exactly once, and the pool
			// got no spare.
			if c := e.set.Counters(); c.UsedBytes != before.UsedBytes-segCap {
				t.Fatalf("after the failed recycle the footprint is %d, want the one cap %d lower", c.UsedBytes, before.UsedBytes-segCap)
			}
			if c := e.set.Counters(); c.Spares != before.Spares {
				t.Fatalf("after the failed recycle the ledger holds %d spares, want the %d it held", c.Spares, before.Spares)
			}
			if err := e.set.Check(); err != nil {
				t.Fatalf("%v", err)
			}

			// The pool's slot was given back, so a second recycle wants it
			// again: the reservation did not leak.
			if _, ok := e.p.WantRecycleForTest(); !ok {
				t.Fatal("after the failed recycle the pool wants no recycle, want its slot back")
			}
		})
	}

	t.Run("a rename that applies and still fails", func(t *testing.T) {
		// A rename that lands the file under the fresh name and still
		// returns EIO is the one storage move a seam rule cannot spell: the
		// cleanup must remove under both names, finding the file under the
		// fresh one. The plain seam makes the segment first, and the
		// failing-applier wrapper — installed for the retire alone — carries
		// the rename through and fails it.
		seam := newSeam(t)
		fsys := seam.FS()
		set := segment.NewSet(segCap, nil)
		m := segment.NewMaker(fsys, set, noMax, 0, nil)

		id, err := m.Make()
		if err != nil {
			t.Fatalf("the segment's making: %v", err)
		}
		if err := set.Take(id); err != nil {
			t.Fatalf("the segment's takeover: %v", err)
		}
		if err := set.Seal(id, true); err != nil {
			t.Fatalf("the segment's seal: %v", err)
		}
		before := set.Counters()

		applier := &appliedRename{FS: fsys}
		p := segment.NewPool(m, segment.Targets{Content: 1}, nil)
		cache := segment.NewDescriptors(applier, format.SegmentName, core.NewDescriptorBudget(64))
		r := segment.NewRetirer(segment.RetirerConfig{
			Set: set, Cache: cache, Pool: p, Maker: m, FS: applier,
		})

		res, err := r.Retire(id, segment.NotHeld)
		// The rename applied and then failed, so the fallback unlink found the
		// file under the fresh name and removed it: a completed retirement at
		// nil, not an error to retry.
		if err != nil {
			t.Fatalf("the recycle whose rename applied and failed returned %v, want nil: the fallback unlink removed the fresh name", err)
		}
		if res.Recycled {
			t.Fatalf("the failed recycle was reported as one: %+v", res)
		}
		if res.Freed != segCap {
			t.Fatalf("the fallback unlink freed %d, want the cap", res.Freed)
		}
		segs, spares := classify(listNames(t, seam))
		if len(segs) != 0 || len(spares) != 0 {
			t.Fatalf("after the applied-then-failed rename the directory holds %v and %v, want both names gone", segs, spares)
		}
		if c := set.Counters(); c.UsedBytes != before.UsedBytes-segCap {
			t.Fatalf("after the fallback unlink the footprint is %d, want one cap lower", c.UsedBytes)
		}
	})
}

// appliedRename calls its Rename through and then fails: the
// rename-half-done case the seam's own failing rule cannot spell, since a
// rule it applies refuses the call before anything moves. Everything except
// Rename the wrapped view answers untouched.
type appliedRename struct {
	storage.FS
}

func (a *appliedRename) Rename(oldName, newName string) error {
	if err := a.FS.Rename(oldName, newName); err != nil {
		return err
	}
	// decision: the EIO is returned after the rename applied, not instead of
	// it, because that is the case this wrapper exists to spell: storage
	// that moved the file and still reported a failure leaves the file under
	// the new name, so the cleanup must try both names.
	return syscall.EIO
}

// TestRetireCrashBetweenRenameAndZeroing: a crash between the recycle's
// rename and its zeroing leaves a file under the fresh name that holds the
// old life's bytes and no file under the old one — renamed first, so none of
// the old life verifies there; a crash after the zeroing sync leaves the two
// slots as zeros (RFC 28 §2.2).
func TestRetireCrashBetweenRenameAndZeroing(t *testing.T) {
	prof.Watch(t, prof.Options{})

	t.Run("held at the first zeroing write", func(t *testing.T) {
		e, _ := retirer(t, segment.Targets{Content: 1}, nil)
		id := e.segment(true)

		// Park the retirement at its first zeroing write: the rename and its
		// directory sync are past, the zeroing is not.
		h := e.seam.HoldFirst(t, journaltest.OpWrite, "*")
		resc := make(chan segment.Retirement, 1)
		errc := make(chan error, 1)
		go func() {
			res, err := e.r.Retire(id, segment.NotHeld)
			resc <- res
			errc <- err
		}()
		awaitReached(t, journaltest.OpWrite, h, errc)

		if err := e.seam.Crash(); err != nil {
			t.Fatalf("the crash between the rename and the zeroing: %v", err)
		}
		h.Release()
		<-resc
		if err := <-errc; err == nil {
			t.Fatal("the crashed recycle returned no error")
		}

		names := listNames(t, e.seam)
		segs, _ := classify(names)
		if len(segs) != 1 {
			t.Fatalf("after the crash the directory holds the segments %v, want the one renamed file", segs)
		}
		fresh := segs[0]
		if kind, id := format.ClassifyName(fresh); kind != format.NameSegment {
			t.Fatalf("the renamed file is %s, want a segment name", fresh)
		} else if id <= 1 {
			t.Fatalf("the renamed file is %s, want a fresh id above the old %d", fresh, 1)
		}
		// The old life's bytes, whole: header, middle and trailer, nothing
		// zeroed.
		header := make([]byte, format.SegmentHeaderSize)
		e.bytesAt(fresh, header, 0)
		trailer := make([]byte, format.TrailerSize)
		e.bytesAt(fresh, trailer, segCap-format.TrailerSize)
		for i, b := range header {
			if b != 0xAA {
				t.Fatalf("%s: byte %d of the header slot is %#x, want the old life's byte: the zeroing never ran", fresh, i, b)
			}
		}
		for i, b := range trailer {
			if b != 0xCC {
				t.Fatalf("%s: byte %d of the trailer slot is %#x, want the old life's byte: the zeroing never ran", fresh, i, b)
			}
		}
		middle := make([]byte, 64)
		e.bytesAt(fresh, middle, segCap/2)
		for i, b := range middle {
			if b != 0xBB {
				t.Fatalf("%s: byte %d of the middle is %#x, want the old life's byte", fresh, i, b)
			}
		}
		// The old name is absent.
		if slices.Contains(names, format.SegmentName(id)) {
			t.Fatalf("after the crash the old name %s survives", format.SegmentName(id))
		}
	})

	t.Run("after the zeroing's sync", func(t *testing.T) {
		e, _ := retirer(t, segment.Targets{Content: 1}, nil)
		id := e.segment(true)

		// The recycle runs whole; the crash lands after everything it did.
		if _, err := e.r.Retire(id, segment.NotHeld); err != nil {
			t.Fatalf("the recycle: %v", err)
		}
		if err := e.seam.Crash(); err != nil {
			t.Fatalf("the crash after the recycle: %v", err)
		}
		names := listNames(t, e.seam)
		segs, _ := classify(names)
		if len(segs) != 1 {
			t.Fatalf("after the crash the directory holds the segments %v, want the one recycled file", segs)
		}
		e.assertZeroed(segs[0])
	})
}

// TestRetireCrashAtEachDurableStateOfAnUnlink: the two states an unlink can
// crash in differ by the directory sync alone. Before it, the removal is not
// durable and the crash leaves the file back under its own name — the seam
// undoes an unsynced Remove deterministically — so the next open finds it.
// After it, the file is gone for good. Only these two outcomes may hold, and
// the test asserts each of them (R45).
func TestRetireCrashAtEachDurableStateOfAnUnlink(t *testing.T) {
	prof.Watch(t, prof.Options{})

	t.Run("crash before the directory sync leaves the file", func(t *testing.T) {
		e, _ := retirer(t, noPool, nil)
		id := e.segment(true)
		name := format.SegmentName(id)

		// Park the unlink after its remove: the hold on the next sync_dir
		// lets the remove return and nothing else.
		h := e.seam.HoldFirst(t, journaltest.OpSyncDir, "*")
		resc := make(chan segment.Retirement, 1)
		errc := make(chan error, 1)
		go func() {
			res, err := e.r.Retire(id, segment.NotHeld)
			resc <- res
			errc <- err
		}()
		awaitReached(t, journaltest.OpSyncDir, h, errc)

		if err := e.seam.Crash(); err != nil {
			t.Fatalf("the crash: %v", err)
		}
		if _, err := e.seam.Durable(name); errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("a crash before the directory sync left %s gone, want it back: the removal it never covered is the one the next open finds", name)
		} else if err != nil {
			t.Fatalf("Durable(%s): %v", name, err)
		}
		h.Release()
	})

	t.Run("crash after the directory sync finds the file gone", func(t *testing.T) {
		e, _ := retirer(t, noPool, nil)
		id := e.segment(true)
		name := format.SegmentName(id)

		res, err := e.r.Retire(id, segment.NotHeld)
		if err != nil {
			t.Fatalf("the unlink: %v", err)
		}
		if !res.Durable {
			t.Fatal("the unlink whose directory sync returned reported itself not durable, want durable")
		}
		if err := e.seam.Crash(); err != nil {
			t.Fatalf("the crash: %v", err)
		}
		if _, err := e.seam.Durable(name); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("a crash after the directory sync left %s durable (%v), want it gone: the sync is what made the removal stick", name, err)
		}
	})
}

// TestRetireTwoPathsRetireOneSegmentOnce: two retirers retreating one segment
// at once serialise on the ledger's claim — exactly one retires, the other
// meets the claim's refusal — and a third, sequential call meets the ledger's
// double retire, counted and logged at Warn. The footprint falls by the size
// exactly once and never below zero (R74).
func TestRetireTwoPathsRetireOneSegmentOnce(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The ledger logs its double retire through the retirer's own logger, so
	// the Warn the third retire owes is captured here.
	h := &logRecorder{}
	e, _ := retirerLogged(t, noPool, nil, h)
	id := e.segment(true)
	before := e.set.Counters()

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.r.Retire(id, segment.NotHeld)
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	won := 0
	for err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, segment.ErrClaimed):
		case errors.Is(err, segment.ErrNotInSet):
		default:
			t.Fatalf("a concurrent retire returned %v, want a success or the claim's refusal", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d of the two concurrent retires succeeded, want exactly one", won)
	}

	// The footprint fell by the size exactly once, and never below zero.
	if c := e.set.Counters(); c.UsedBytes != before.UsedBytes-segCap {
		t.Fatalf("after the two retires the footprint is %d, want exactly one size %d lower", c.UsedBytes, before.UsedBytes-segCap)
	}
	if c := e.r.Counters(); c.FreedBytes != uint64(segCap) {
		t.Fatalf("freed_bytes is %d after the two retires, want the one size %d", c.FreedBytes, segCap)
	}

	// A third, sequential retire subtracts nothing, counts and logs at Warn.
	// The race's own double retires — a losing claim that arrived after the
	// winner's whole retirement — are behind the baseline; the third retire
	// adds exactly one of its own, whatever the race left.
	raced := e.set.Counters()
	if _, err := e.r.Retire(id, segment.NotHeld); !errors.Is(err, segment.ErrNotInSet) {
		t.Fatalf("the third retire: %v, want %v", err, segment.ErrNotInSet)
	}
	if c := e.set.Counters(); c.DoubleRetires != raced.DoubleRetires+1 {
		t.Fatalf("the third retire counted %d double retires, want exactly one over %d", c.DoubleRetires, raced.DoubleRetires)
	}
	if c := e.set.Counters(); c.UsedBytes != 0 {
		t.Fatalf("after the third retire the footprint is %d, want 0: it never went below zero", c.UsedBytes)
	}
	if got := h.countAt(slog.LevelWarn, "retire found the segment out of the set"); got != int(raced.DoubleRetires)+1 {
		t.Fatalf("the third retire logged %d double-retire Warns, want one per double retire", got)
	}
	if c := e.r.Counters(); c.FreedBytes != uint64(segCap) {
		t.Fatalf("freed_bytes moved on the third retire to %d, want still %d", c.FreedBytes, segCap)
	}
}

// TestRetireWaitsForAReadInFlight: an unlink waits for the in-flight reads
// of its segment — the retire's own remove is not reached while a reference
// is held, and once the wait ends a new acquire finds the segment gone — and
// a read done under the held reference returns the pattern written (R47,
// R65, R76). It fails if Close(id) stops waiting.
func TestRetireWaitsForAReadInFlight(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, _ := retirer(t, noPool, nil)
	id := e.segment(true)
	name := format.SegmentName(id)

	// A reader holds a reference: the old life's pattern is what it reads.
	ref, err := e.c.Acquire(id)
	if err != nil {
		t.Fatalf("the reader's acquire: %v", err)
	}
	buf := make([]byte, format.SegmentHeaderSize)
	if _, err := ref.ReadAt(buf, 0); err != nil {
		t.Fatalf("the read under the reference: %v", err)
	}
	for i, b := range buf {
		if b != 0xAA {
			t.Fatalf("the read served byte %d as %#x, want the old life's byte", i, b)
		}
	}

	// The retire parks at its remove: it must not reach it while the
	// reference is held.
	h := e.seam.HoldFirst(t, journaltest.OpRemove, name)
	resc := make(chan segment.Retirement, 1)
	errc := make(chan error, 1)
	go func() {
		res, err := e.r.Retire(id, segment.NotHeld)
		resc <- res
		errc <- err
	}()

	select {
	case <-h.Reached():
		t.Fatal("the retire reached its remove while the reader still held the segment, want it waiting")
	case <-time.After(50 * time.Millisecond):
	}

	// The reference released, the drain ends and the retire reaches its
	// remove: the hold is what proves it arrived only now, and its release
	// is what lets the retire finish.
	ref.Release()
	awaitReached(t, journaltest.OpRemove, h, errc)
	h.Release()
	if res := <-resc; res.Recycled || res.Freed != segCap {
		t.Fatalf("the retire after the drain recycled or freed wrong: %+v", res)
	}
	if err := <-errc; err != nil {
		t.Fatalf("the retire after the drain: %v", err)
	}

	// A new acquire finds the segment gone, for good.
	if _, err := e.c.Acquire(id); !errors.Is(err, segment.ErrSegmentGone) {
		t.Fatalf("the acquire after the retire: %v, want %v", err, segment.ErrSegmentGone)
	}
}

// TestRetireFreedBytesAfterTheUnlink: freed_bytes rises by the accounted size
// after an unlink and only after it — a retirement parked at its remove still
// shows the old figure — and a recycle raises it by the cap once its own
// steps complete (RFC 1 §3.9).
func TestRetireFreedBytesAfterTheUnlink(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, _ := retirer(t, noPool, nil)
	id := e.segment(true)
	name := format.SegmentName(id)

	h := e.seam.HoldFirst(t, journaltest.OpRemove, name)
	resc := make(chan segment.Retirement, 1)
	errc := make(chan error, 1)
	go func() {
		res, err := e.r.Retire(id, segment.NotHeld)
		resc <- res
		errc <- err
	}()
	awaitReached(t, journaltest.OpRemove, h, errc)

	if c := e.r.Counters(); c.FreedBytes != 0 {
		t.Fatalf("while the unlink is held freed_bytes is %d, want 0: it rises only after the remove", c.FreedBytes)
	}
	h.Release()
	if err := <-errc; err != nil {
		t.Fatalf("the unlink: %v", err)
	}
	<-resc
	if c := e.r.Counters(); c.FreedBytes != uint64(segCap) {
		t.Fatalf("after the unlink freed_bytes is %d, want the cap %d", c.FreedBytes, segCap)
	}
}

// TestRetireNotDurable: a retirement whose directory sync fails reports not
// durable and stays that way: no later sync of the directory makes the failed
// one's removal durable — "a sync that failed MUST NOT satisfy, by a later
// success, a Sync for writes made before the failure" (RFC 1 §5.1) — so the
// durable signal never fires for it in this process, whatever the worker does,
// and a crash brings the file back under its own name. The retirement itself
// is done: the footprint is lowered and the retire returns nil. The next
// open's scan settles the file (R45, RFC 1 §5.1, §5.3).
func TestRetireNotDurable(t *testing.T) {
	prof.Watch(t, prof.Options{})

	h := &logRecorder{}
	e, durable := retirerLogged(t, noPool, nil, h)
	id := e.segment(true)
	name := format.SegmentName(id)

	// The unlink's directory sync fails once.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpSyncDir, "*", 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	res, err := e.r.Retire(id, segment.NotHeld)
	if err != nil {
		t.Fatalf("the retire whose sync fails: %v", err)
	}
	if res.Durable {
		t.Fatal("the retire whose directory sync failed reported itself durable, want not")
	}
	if res.Freed != segCap {
		t.Fatalf("the retire freed %d, want the cap: the storage moved whatever the sync did", res.Freed)
	}
	if c := e.set.Counters(); c.UsedBytes != 0 {
		t.Fatalf("after the retire the footprint is %d, want 0: the unlink happened", c.UsedBytes)
	}
	if durable.len() != 0 {
		t.Fatalf("the durable signal was called %d times on a not-durable retire, want none", durable.len())
	}
	if got := h.countAt(slog.LevelWarn, "not durable"); got < 1 {
		t.Fatalf("the failed directory sync logged %d Warns naming the durability, want one the operator can find", got)
	}

	// The fault is spent. Let the worker run passes over a healthy directory:
	// none of them may report the failed sync's removal durable, for no
	// later sync makes it so, and the signal must stay silent for it.
	segment.RetryEveryForTest(e.r, time.Millisecond)
	e.r.Start()
	e.r.KickForTest()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if durable.len() != 0 {
		t.Fatalf("after %s of passes over a healthy directory the not-durable retire was signalled %+v, want no signal: a later sync satisfies nothing (RFC 1 §5.1)",
			300*time.Millisecond, durable.all())
	}
	e.r.Stop()

	// The crash is the proof: the removal the failed sync covered is not
	// durable, so the file is back under its own name.
	if err := e.seam.Crash(); err != nil {
		t.Fatalf("the crash: %v", err)
	}
	if _, err := e.seam.Durable(name); errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("after the crash %s is gone, want it back: the failed directory sync made the removal durable", name)
	} else if err != nil {
		t.Fatalf("Durable(%s): %v", name, err)
	}
}

// TestRetireWorkerRetriesAFailedUnlink: a remove that fails leaves the
// segment in the set at its size, accounted, its claim released so a retry
// can claim again; once the fault clears and the id is queued, the worker's
// pass unlinks it, lowers the footprint once and calls the durable signal
// once. A failed recycle whose fallback unlink fails too retries and removes
// the fresh name with the old one.
func TestRetireWorkerRetriesAFailedUnlink(t *testing.T) {
	prof.Watch(t, prof.Options{})

	t.Run("a failed unlink, retried", func(t *testing.T) {
		e, durable := retirer(t, noPool, nil)
		id := e.segment(true)
		name := format.SegmentName(id)
		before := e.set.Counters()

		if err := e.seam.Schedule(journaltest.Failing(journaltest.OpRemove, name, 1, syscall.EIO)); err != nil {
			t.Fatalf("Schedule: %v", err)
		}
		if _, err := e.r.Retire(id, segment.NotHeld); err == nil {
			t.Fatal("the retire whose remove fails: no error")
		}
		// The segment stays in the set, accounted at its size.
		if c := e.set.Counters(); c != before {
			t.Fatalf("after the failed unlink the ledger holds %+v, want the untouched %+v", c, before)
		}
		// The claim is released: a retry can take it.
		if _, _, err := e.set.ClaimRetirement(id); err != nil {
			t.Fatalf("the claim after the failed unlink: %v, want it free", err)
		}
		e.set.Release(id)

		// Queued and the fault spent, the worker's pass retires it: the
		// durable signal is what the pass ends on, so it is what the test
		// waits for.
		segment.RetryEveryForTest(e.r, time.Millisecond)
		e.r.Start()
		e.r.Enqueue(id, segment.NotHeld)
		deadline := time.Now().Add(10 * time.Second)
		for durable.len() == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("the worker never retried the failed unlink: the ledger holds %+v and the signal %+v",
					e.set.Counters(), durable.all())
			}
			runtime.Gosched()
		}
		if durable.len() != 1 || durable.at(0).ID != id || durable.at(0).Freed != segCap {
			t.Fatalf("the retried retire's signal was called for %+v, want once for %d at the cap", durable.all(), id)
		}
		if c := e.r.Counters(); c.FreedBytes != uint64(segCap) {
			t.Fatalf("freed_bytes is %d after the retry, want the cap once", c.FreedBytes)
		}
		e.r.Stop()
	})

	t.Run("a failed recycle whose fallback unlink fails too", func(t *testing.T) {
		e, _ := retirer(t, segment.Targets{Content: 1}, nil)
		id := e.segment(true)
		oldName := format.SegmentName(id)

		// The recycle's first zeroing write fails, so the fallback unlink
		// owns both names — its own and the fresh one — and both removes
		// fail now, so only the retry can take the segment out.
		if err := e.seam.Schedule(
			journaltest.Failing(journaltest.OpWrite, "*", 1, syscall.EIO),
			journaltest.Failing(journaltest.OpRemove, "*", 2, syscall.EIO),
		); err != nil {
			t.Fatalf("Schedule: %v", err)
		}
		res, err := e.r.Retire(id, segment.NotHeld)
		if err == nil {
			t.Fatalf("the retire whose recycle and fallback both fail: no error, %+v", res)
		} else if !errors.Is(err, syscall.EIO) {
			t.Fatalf("the failed recycle led with %v, want the EIO", err)
		}
		if got := e.seam.CallsFor(journaltest.OpRemove, oldName); got < 1 {
			t.Fatalf("the fallback unlink never tried %s", oldName)
		}
		// The fresh name is recorded, so the retry's unlink removes it too.
		names := listNames(t, e.seam)
		segs, _ := classify(names)
		if len(segs) != 1 {
			t.Fatalf("after the failed recycle the directory holds the segments %v, want the one renamed file", segs)
		}

		// The retry removes both names and retires the segment at last.
		segment.RetryEveryForTest(e.r, time.Millisecond)
		e.r.Start()
		e.r.Enqueue(id, segment.NotHeld)
		deadline := time.Now().Add(10 * time.Second)
		for e.set.Counters().UsedBytes != 0 {
			if time.Now().After(deadline) {
				t.Fatalf("the worker never retried: the ledger holds %+v", e.set.Counters())
			}
			runtime.Gosched()
		}
		segs, spares := classify(listNames(t, e.seam))
		if len(segs) != 0 || len(spares) != 0 {
			t.Fatalf("after the retried recycle-fallback the directory holds %v and %v, want both names gone", segs, spares)
		}
		if c := e.r.Counters(); c.FreedBytes != uint64(segCap) {
			t.Fatalf("freed_bytes is %d after the retry, want the cap once", c.FreedBytes)
		}
		e.r.Stop()
	})

	t.Run("a recycle that failed then landed leaves no extra names behind", func(t *testing.T) {
		e, durable := retirer(t, segment.Targets{Content: 1}, nil)
		id := e.segment(true)

		// The recycle's rename fails without applying and both of the
		// fallback unlink's removes fail after it: the fresh name is recorded
		// for the retry, and the segment stays claimed by the queue.
		if err := e.seam.Schedule(
			journaltest.Failing(journaltest.OpRename, "*", 1, syscall.EIO),
			journaltest.Failing(journaltest.OpRemove, "*", 2, syscall.EIO),
		); err != nil {
			t.Fatalf("Schedule: %v", err)
		}
		if _, err := e.r.Retire(id, segment.NotHeld); err == nil {
			t.Fatal("the retire whose recycle and fallback both fail: no error")
		}
		if got := e.r.ExtraNamesForTest(id); got != 1 {
			t.Fatalf("after the failed recycle the executor records %d extra names, want the fresh one for the retry", got)
		}

		// The fault is spent. The queued retry recycles the segment
		// successfully under a fresh id, and a recycle that landed clears
		// the recorded name with it: nothing of the segment is owed a
		// retry any more, and the map stays bounded by what is. The wait is
		// the durable signal — the recycle's last step — for the ledger's
		// spare rises before the executor's own cleanup has run.
		segment.RetryEveryForTest(e.r, time.Millisecond)
		e.r.Start()
		e.r.Enqueue(id, segment.NotHeld)
		deadline := time.Now().Add(10 * time.Second)
		for durable.len() == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("the worker never retried the recycle: the ledger holds %+v and the signal %+v", e.set.Counters(), durable.all())
			}
			runtime.Gosched()
		}
		if got := durable.at(0); got.ID != id || !got.Recycled {
			t.Fatalf("the retry's retirement was %+v, want the recycle of %d", got, id)
		}
		if got := e.r.ExtraNamesForTest(id); got != 0 {
			t.Fatalf("after the retry landed its recycle the executor still records %d extra names for %d, want none: the map is bounded by the retirements still owed a retry", got, id)
		}
		e.r.Stop()
	})
}

// readOnlyErr is the error the R89 checks refuse with.
var errReadOnly = errors.New("the journal is read-only")

// TestRetireReadOnly: while the read-only check refuses, a retirement makes
// no ledger call and no seam call, and the worker's pass retires nothing,
// leaving the queue as it is; once the check clears, a kick drains the queue
// (R89).
func TestRetireReadOnly(t *testing.T) {
	prof.Watch(t, prof.Options{})

	ro := &readOnlyFlag{}
	e, _ := retirer(t, noPool, ro.check)
	id := e.segment(true)
	before := e.set.Counters()

	ro.set(true)
	fault := &countingFault{}
	e.seam.SetFault(fault)
	if _, err := e.r.Retire(id, segment.NotHeld); err == nil {
		t.Fatal("the read-only retire succeeded, want the check's own refusal")
	} else if !errors.Is(err, errReadOnly) {
		t.Fatalf("the read-only retire refused with %v, want the check's own error", err)
	}
	if fault.calls > 0 {
		t.Fatalf("the read-only retire made %d seam calls, want none", fault.calls)
	}

	// A queued id over a read-only journal: the worker's passes change
	// nothing, and the queue survives.
	segment.RetryEveryForTest(e.r, time.Millisecond)
	e.r.Start()
	e.r.Enqueue(id, segment.NotHeld)
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if c := e.set.Counters(); c != before {
		t.Fatalf("over the read-only interval the ledger moved to %+v, want %+v", c, before)
	}

	// The check clears: a kick drains the queue.
	e.seam.SetFault(nil)
	ro.set(false)
	e.r.KickForTest()
	deadline = time.Now().Add(10 * time.Second)
	for e.set.Counters().UsedBytes != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the queue never drained once the journal was writable: the ledger holds %+v", e.set.Counters())
		}
		runtime.Gosched()
	}
	e.r.Stop()
}

// readOnlyFlag is a test's read-only check: a flag under its own mutex.
type readOnlyFlag struct {
	mu  sync.Mutex
	off bool
}

func (f *readOnlyFlag) set(on bool) {
	f.mu.Lock()
	f.off = on
	f.mu.Unlock()
}

func (f *readOnlyFlag) check() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.off {
		return errReadOnly
	}
	return nil
}

// TestRetireWorkerLifecycle: Start launches one worker a Stop joins, leaving
// no goroutine behind; a second Start is a no-op; a second Stop is a no-op;
// and an Enqueue after Stop is a drop. The profiler at the top fails the
// test on the leaked worker (R70 groundwork).
func TestRetireWorkerLifecycle(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, durable := retirer(t, noPool, nil)
	segment.RetryEveryForTest(e.r, time.Millisecond)

	e.r.Start()
	e.r.Start()
	id := e.segment(true)
	e.r.Enqueue(id, segment.NotHeld)
	deadline := time.Now().Add(10 * time.Second)
	for durable.len() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the worker never retired the queued segment: %+v", durable.all())
		}
		runtime.Gosched()
	}

	e.r.Stop()
	e.r.Stop()

	// After Stop, an Enqueue is a drop: no retirement happens, whatever the
	// test yields.
	id2 := e.segment(true)
	before := e.set.Counters()
	e.r.Enqueue(id2, segment.NotHeld)
	for range 1 << 16 {
		runtime.Gosched()
	}
	if c := e.set.Counters(); c != before {
		t.Fatalf("an Enqueue after Stop retired something: %+v, want %+v", c, before)
	}
}

// TestRetireAFailedRecycleFellBackIsARetirement: a recycle that fails at a
// step and falls back to an unlink that succeeds is a completed retirement,
// not an error — "a recycle that fails is an unlink" (RFC 28 §2.2, R41), and
// the unlink is what retired the segment. It returns nil with Recycled false,
// frees the cap and reports the unlink's durability; the worker's pass drops
// it, and a queued fallback costs the ledger no double retire, so R74's
// counter stays clean of every transient zeroing failure (the Warn the
// executor writes names the step for the operator instead).
func TestRetireAFailedRecycleFellBackIsARetirement(t *testing.T) {
	prof.Watch(t, prof.Options{})

	t.Run("direct", func(t *testing.T) {
		h := &logRecorder{}
		e, durable := retirerLogged(t, segment.Targets{Content: 1}, nil, h)
		id := e.segment(true)

		// The recycle's first zeroing write fails, so the fallback unlink
		// takes the segment out under both of its names.
		if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, "*", 1, syscall.EIO)); err != nil {
			t.Fatalf("Schedule: %v", err)
		}
		before := e.set.Counters()

		res, err := e.r.Retire(id, segment.NotHeld)
		if err != nil {
			t.Fatalf("the retire whose recycle fell back: %v, want nil: the fallback unlink retired the segment", err)
		}
		if res.Recycled {
			t.Fatalf("the fallen-back recycle was reported as one: %+v, want an unlink", res)
		}
		if res.Freed != segCap {
			t.Fatalf("the fallen-back recycle freed %d, want the cap", res.Freed)
		}
		if !res.Durable {
			t.Fatal("the fallen-back recycle is not durable, want the fallback unlink's sync back")
		}
		if c := e.set.Counters(); c.UsedBytes != before.UsedBytes-segCap || c.DoubleRetires != 0 {
			t.Fatalf("after the fallback the ledger holds %+v, want only the one cap gone from %+v and no double retire", c, before)
		}
		segs, spares := classify(listNames(t, e.seam))
		if len(segs) != 0 || len(spares) != 0 {
			t.Fatalf("after the fallback the directory holds %v and %v, want both names gone", segs, spares)
		}
		if durable.len() != 1 || durable.at(0).ID != id {
			t.Fatalf("the fallback's durable signal was called for %+v, want once for %d", durable.all(), id)
		}
		if got := h.countAt(slog.LevelWarn, "recycle"); got != 1 {
			t.Fatalf("the failed recycle step logged %d Warns, want the one that names it", got)
		}
	})

	t.Run("queued, costs no double retire", func(t *testing.T) {
		h := &logRecorder{}
		e, durable := retirerLogged(t, segment.Targets{Content: 1}, nil, h)
		id := e.segment(true)
		before := e.set.Counters()

		if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, "*", 1, syscall.EIO)); err != nil {
			t.Fatalf("Schedule: %v", err)
		}
		segment.RetryEveryForTest(e.r, time.Millisecond)
		e.r.Enqueue(id, segment.NotHeld)
		e.r.Start()

		// The queued recycle falls back, the fallback unlink succeeds, and
		// the id is done: no pass ever retires it again, so the
		// double-retire counter never moves.
		deadline := time.Now().Add(10 * time.Second)
		for durable.len() == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("the queued fallback never signalled: %+v, ledger %+v", durable.all(), e.set.Counters())
			}
			runtime.Gosched()
		}
		// Let further passes run over the finished id: none may touch it.
		deadline = time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(deadline) {
			runtime.Gosched()
		}
		e.r.Stop()

		if c := e.set.Counters(); c.DoubleRetires != before.DoubleRetires {
			t.Fatalf("after the queued fallback the ledger counted %d double retires, want the %d it started with: a fallen-back recycle is one retirement, not a retry",
				c.DoubleRetires, before.DoubleRetires)
		}
		if durable.len() != 1 {
			t.Fatalf("the queued fallback signalled %d times, want once: the worker must drop a fell-back id, not requeue it", durable.len())
		}
		if c := e.set.Counters(); c.UsedBytes != before.UsedBytes-segCap {
			t.Fatalf("after the queued fallback the footprint is %d, want one cap below %d", c.UsedBytes, before.UsedBytes)
		}
	})
}

// TestRetireWorkerKeepsAClaimedSegmentQueued: an id another path holds the
// claim of is neither retired nor dropped by the pass — it stays queued for a
// later one, and once the claim is released the next pass retires it. It
// catches a pass that mistakes ErrClaimed for a done hand-over.
func TestRetireWorkerKeepsAClaimedSegmentQueued(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, durable := retirer(t, noPool, nil)
	id := e.segment(true)
	before := e.set.Counters()

	if !e.set.Claim(id) {
		t.Fatal("the claim on the sealed segment was refused")
	}
	segment.RetryEveryForTest(e.r, time.Millisecond)
	e.r.Enqueue(id, segment.NotHeld)
	e.r.Start()

	// Passes run over the claimed segment: none retires it and none drops
	// it, for the claim's holder may release at any moment.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if c := e.set.Counters(); c != before {
		t.Fatalf("over the claimed interval the ledger moved to %+v, want the untouched %+v", c, before)
	}
	if durable.len() != 0 {
		t.Fatalf("a claimed segment was retired behind the claim: %+v", durable.all())
	}

	// Released, the queued id is the next pass's own.
	e.set.Release(id)
	e.r.KickForTest()
	deadline = time.Now().Add(10 * time.Second)
	for durable.len() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the released claim's queued id was never retired: ledger %+v, signals %+v", e.set.Counters(), durable.all())
		}
		runtime.Gosched()
	}
	if got := durable.at(0); got.ID != id || got.Recycled {
		t.Fatalf("the released claim's retirement was %+v, want the unlink of %d", got, id)
	}
	e.r.Stop()
}

// TestRetireWorkerDropsAWrongReasonNotAQueue: a queued id handed over with
// the wrong reason is dropped, not requeued — it can never retire as handed,
// and a requeued one would Warn and retry forever, and Enqueue's own
// dedup-by-id would keep ignoring the corrected hand-over. Once dropped, the
// same id enqueued with the right reason is retired.
func TestRetireWorkerDropsAWrongReasonNotAQueue(t *testing.T) {
	prof.Watch(t, prof.Options{})

	h := &logRecorder{}
	e, durable := retirerLogged(t, noPool, nil, h)
	id := e.ended()
	before := e.set.Counters()

	segment.RetryEveryForTest(e.r, time.Millisecond)
	e.r.Start()
	e.r.Enqueue(id, segment.NotHeld)

	// The wrong pair runs and is dropped: the Warn names it, and passes over
	// the finished id change nothing, for a requeued id would retry every
	// pass and a dropped one is done.
	if got := awaitLoggedMax(t, h, slog.LevelWarn, "dropped a segment", 5*time.Second); got < 1 {
		t.Fatalf("the wrongly handed-over id was never dropped: %d Warns, want at least one naming the drop", got)
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if c := e.set.Counters(); c != before {
		t.Fatalf("over the wrong-reason interval the ledger moved to %+v, want the untouched %+v: a dropped id retires nothing and a requeued one retries for ever", c, before)
	}

	// The dropped id is gone from the queue, so the corrected hand-over is
	// accepted and retires.
	e.r.Enqueue(id, segment.ReasonEnded)
	deadline = time.Now().Add(10 * time.Second)
	for durable.len() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the corrected hand-over was never retired: ledger %+v, signals %+v", e.set.Counters(), durable.all())
		}
		runtime.Gosched()
	}
	if got := durable.at(0); got.ID != id || got.Recycled {
		t.Fatalf("the corrected hand-over's retirement was %+v, want the unlink of %d", got, id)
	}
	e.r.Stop()
}

// awaitLoggedMax waits up to d for the handler to hold a record at level
// whose message contains sub, returning how many it holds when it gives up.
func awaitLoggedMax(t *testing.T, h slog.Handler, level slog.Level, sub string, d time.Duration) int {
	t.Helper()

	recs, ok := h.(*logRecorder)
	if !ok {
		t.Fatalf("awaitLoggedMax: the handler is not the test's recorder")
	}
	deadline := time.Now().Add(d)
	for {
		got := recs.countAt(level, sub)
		if got >= 1 {
			return got
		}
		if time.Now().After(deadline) {
			return got
		}
		runtime.Gosched()
	}
}

// TestRetireForgetsTheCacheEntry: a retirement forgets the segment's cache
// entry — on the unlink path and on the recycle's — so the entry map stays
// bounded by the live segments and no stray acquire of a retired id leaves a
// tombstone behind. It catches a retirement that never drops the cache's
// record of the segment (the map would grow one stale entry per retired
// segment, per recycle).
func TestRetireForgetsTheCacheEntry(t *testing.T) {
	prof.Watch(t, prof.Options{})

	t.Run("unlink", func(t *testing.T) {
		e, _ := retirer(t, noPool, nil)
		id := e.segment(true)
		forgetEntryThenRetire(t, e, id, segment.NotHeld)
	})

	t.Run("recycle", func(t *testing.T) {
		e, _ := retirer(t, segment.Targets{Content: 1}, nil)
		id := e.segment(true)
		res := forgetEntryThenRetire(t, e, id, segment.NotHeld)
		if !res.Recycled {
			t.Fatalf("the pool below its target did not recycle: %+v, want a recycle", res)
		}
	})
}

// forgetEntryThenRetire gives the cache a live entry for the segment, retires
// it under the reason named, and asserts the cache holds nothing after: the
// map is bounded by the live segments whatever path the retirement took.
func forgetEntryThenRetire(t *testing.T, e *retireEnv, id core.SegmentID, reason segment.Reason) segment.Retirement {
	t.Helper()

	// A reader acquires and releases, so the cache holds a live entry for
	// the segment before the retirement runs.
	ref, err := e.c.Acquire(id)
	if err != nil {
		t.Fatalf("the reader's acquire: %v", err)
	}
	ref.Release()
	if got := segment.EntryCount(e.c); got != 1 {
		t.Fatalf("before the retire the cache holds %d entries, want the one for %d", got, id)
	}

	res, err := e.r.Retire(id, reason)
	if err != nil {
		t.Fatalf("the retire: %v", err)
	}
	if got := segment.EntryCount(e.c); got != 0 {
		t.Fatalf("after the retire the cache holds %d entries, want none: the entry map is bounded by the live segments", got)
	}
	return res
}

// TestRetireUnlinkKicksTheRefill: an unlink wakes the pool's refill on its
// own, so a class left short by a failed make fills again once a retirement
// runs. Nothing else kicks between the failed fill and the retire: the
// take-and-hold paths are quiet, so the unlink's own kick is the only wake
// the worker gets. It catches an unlink that leaves the refill asleep.
func TestRetireUnlinkKicksTheRefill(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// A pool whose headroom alone holds a target, its fill's one make made to
	// fail: the worker waits for a kick with the headroom short, and the make
	// it failed is named at Warn — the logger the test reads it back through.
	h := &logRecorder{}
	seam, set, m, p := poolOf(t, segment.Targets{Content: 0, Headroom: 1, Reserve: 0}, noMax, 0, slog.New(h))
	if err := seam.Schedule(journaltest.Failing(journaltest.OpAllocate, "*", 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	p.Start()
	awaitLogged(t, h, slog.LevelWarn, "did not make a spare", 1)
	if c := p.Counters(); c.HeadroomBytes != 0 {
		t.Fatalf("after the failed fill the headroom holds %d bytes, want none", c.HeadroomBytes)
	}

	// The fault clears; nothing has kicked since the failed make.
	if err := seam.Schedule(); err != nil {
		t.Fatalf("the cleared Schedule: %v", err)
	}
	r := segment.NewRetirer(segment.RetirerConfig{
		Set:   set,
		Cache: segment.NewDescriptors(seam.FS(), format.SegmentName, core.NewDescriptorBudget(64)),
		Pool:  p,
		Maker: m,
		FS:    seam.FS(),
		Log:   slog.New(h),
	})

	// An ended segment over the same ledger — the maker works over the
	// cleared seam — retired by an unlink whose own kick is the refill's
	// only wake.
	id, err := m.Make()
	if err != nil {
		t.Fatalf("the ended segment's making: %v", err)
	}
	if err := set.Take(id); err != nil {
		t.Fatalf("the ended segment's takeover: %v", err)
	}
	if err := set.End(id); err != nil {
		t.Fatalf("the ended segment's ending: %v", err)
	}
	if _, err := r.Retire(id, segment.ReasonEnded); err != nil {
		t.Fatalf("the ended segment's retire: %v", err)
	}
	awaitCounters(t, "the headroom filled by the unlink's kick", p,
		func(c segment.PoolCounters) bool { return c.HeadroomBytes == segCap })
	p.Stop()
	r.Stop()
}

// TestRetireStopDropsTheRestOfAnInterruptedPass: a Stop that lands while a
// pass is mid-queue joins the in-flight retirement only — the ids the pass
// was still to run are dropped with the retirer, for epic 9 re-hands every
// not-held segment at the next open, and draining them can only spend storage
// time at close. It catches a pass that runs every copied id regardless,
// which would make Stop wait out each id's reader drain and removal.
func TestRetireStopDropsTheRestOfAnInterruptedPass(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, durable := retirer(t, noPool, nil)
	first := e.segment(true)
	second := e.segment(true)

	// Both ids are queued, and the first retire's unlink is parked at its
	// remove, so the pass is provably mid-flight when Stop lands.
	segment.RetryEveryForTest(e.r, time.Millisecond)
	e.r.Enqueue(first, segment.NotHeld)
	e.r.Enqueue(second, segment.NotHeld)
	remove := e.seam.HoldFirst(t, journaltest.OpRemove, format.SegmentName(first))
	e.r.Start()
	<-remove.Reached()

	// Stop runs beside the parked retirement and joins it — the release
	// below lets it finish — and must not run the second id. It is called
	// from its own goroutine because it blocks until the joinee returns;
	// waiting until the retirer is stopped proves the Stop landed while the
	// first retirement was still in flight, parked.
	stopped := make(chan struct{})
	go func() {
		e.r.Stop()
		close(stopped)
	}()
	for !e.r.StoppedForTest() {
		runtime.Gosched()
	}
	remove.Release()
	<-stopped

	// The first retirement finished; the second was dropped, still in the
	// set at its size, and neither signals again.
	if durable.len() != 1 || durable.at(0).ID != first {
		t.Fatalf("after the interrupted pass's Stop the signals hold %+v, want exactly the first, %d", durable.all(), first)
	}
	c := e.set.Counters()
	if c.Segments != 1 || c.UsedBytes != segCap {
		t.Fatalf("after the interrupted pass's Stop the ledger holds %+v, want the dropped second segment alone at the cap", c)
	}
	if _, err := e.r.Retire(second, segment.NotHeld); err != nil {
		t.Fatalf("the dropped second segment refuses a fresh retire: %v", err)
	}
}
