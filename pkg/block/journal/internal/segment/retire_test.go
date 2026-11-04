package segment_test

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
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

// retirer builds a retirer over a fresh seam. targets is the pool's; no pool
// means every retirement unlinks. readOnly is the R89 check, nil meaning
// writable. durable collects the durable-unlink signal's calls, when the
// test needs it.
func retirer(t *testing.T, targets segment.Targets, readOnly func() error) (*retireEnv, *[]segment.Retirement) {
	t.Helper()
	return retirerSeeded(t, targets, readOnly, 0)
}

// retirerSeeded is retirer with the Maker seeded at last, the way open seeds
// it from the largest id the directory names.
func retirerSeeded(t *testing.T, targets segment.Targets, readOnly func() error, last core.SegmentID) (*retireEnv, *[]segment.Retirement) {
	t.Helper()

	seam := newSeam(t)
	set := segment.NewSet(segCap, nil)
	m := segment.NewMaker(seam.FS(), set, noMax, last, nil)
	c := segment.NewDescriptors(seam.FS(), format.SegmentName, core.NewDescriptorBudget(64))
	var p *segment.Pool
	if targets.Content >= 0 {
		p = segment.NewPool(m, targets, nil)
	}
	var durable []segment.Retirement
	r := segment.NewRetirer(segment.RetirerConfig{
		Set:      set,
		Cache:     c,
		Pool:      p,
		Maker:     m,
		FS:        seam.FS(),
		ReadOnly:  readOnly,
		Durable:   func(d segment.Retirement) { durable = append(durable, d) },
	})
	return &retireEnv{t: t, seam: seam, set: set, m: m, c: c, p: p, r: r}, &durable
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
	if len(*durable) != 1 || (*durable)[0].ID != old {
		t.Fatalf("the durable signal was called for %+v, want once for %d", *durable, old)
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
func TestRetireSealedEarlyIsUnlinked(t *testing.T) {
	prof.Watch(t, prof.Options{})

	t.Run("after a shrink", func(t *testing.T) {
		e, _ := retirer(t, segment.Targets{Content: 1}, nil)
		old := e.segment(false)
		if err := e.set.Shrink(old, segCap/2); err != nil {
			t.Fatalf("the early seal's truncation: %v", err)
		}
		res, err := e.r.Retire(old, segment.NotHeld)
		if err != nil {
			t.Fatalf("the retire of the early-sealed segment: %v", err)
		}
		if res.Recycled {
			t.Fatalf("the early-sealed segment was recycled: %+v, want an unlink", res)
		}
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
		res, err := e.r.Retire(old, segment.NotHeld)
		if err != nil {
			t.Fatalf("the retire of the failed truncation: %v", err)
		}
		if res.Recycled {
			t.Fatalf("a segment sealed early was recycled: %+v, want an unlink", res)
		}
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
		res, err := e.r.Retire(id, segment.ReasonEnded)
		if err != nil {
			t.Fatalf("the retire of the ended segment: %v", err)
		}
		if res.Recycled {
			t.Fatalf("the ended segment was recycled: %+v, want an unlink", res)
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
