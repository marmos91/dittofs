package segment_test

import (
	"errors"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// quietPool kicks the pool and yields enough for a pass that would make a
// spare to have made it: the refill worker is a goroutine the test cannot
// hold, so a check that nothing was made runs after a bounded stretch of
// passes, as the recycle-reservation check does.
func quietPool(p *segment.Pool) {
	for range 16 {
		p.Kick()
		for range 1 << 10 {
			runtime.Gosched()
		}
	}
}

// awaitSpares fails t unless the pool's ready spares reach want within a
// bounded count of yields.
func awaitSpares(t *testing.T, p *segment.Pool, want int64) {
	t.Helper()

	awaitCounters(t, "the spares ready", p, func(c segment.PoolCounters) bool { return c.Spares == want })
}

// TestRefillWaitsForQueuedRecycle: with a full-sealed segment queued for a
// retirement the worker will run, the pool below its target makes no spare;
// the retirement recycles the segment into the slot, and the footprint does
// not move. It catches a refill that makes a spare for a slot a queued
// recycle is about to fill.
func TestRefillWaitsForQueuedRecycle(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The read-only check holds the retirement: the segment stays queued
	// for as long as the flag is up, so the pool runs against a queue that
	// cannot drain.
	var ro readOnlyFlag
	ro.set(true)
	e, _ := retirer(t, segment.Targets{Content: 1}, ro.check)
	id := e.segment(true)
	e.r.Start()
	e.r.Enqueue(id, segment.NotHeld)
	used := e.set.Counters().UsedBytes
	creates := e.seam.Calls(journaltest.OpCreate)

	e.p.Start()
	quietPool(e.p)
	if got := e.seam.Calls(journaltest.OpCreate); got != creates {
		t.Fatalf("the pool created %d files while a full-sealed segment waited in the retirement queue, want none", got-creates)
	}

	ro.set(false)
	e.r.KickForTest()
	awaitSpares(t, e.p, 1)
	if got := e.seam.Calls(journaltest.OpCreate); got != creates {
		t.Fatalf("the recycle's spare cost %d created files, want none", got-creates)
	}
	if got := e.set.Counters().UsedBytes; got != used {
		t.Fatalf("the recycle moved the footprint from %d to %d, want it unchanged", used, got)
	}
	e.r.Stop()
	e.p.Stop()
}

// TestRefillMakesOnceQueueDrains: a content class short by two with one
// full-sealed segment queued gets exactly one recycle and one make: the
// recycle fills a slot, and the queue's emptying wakes the pool for the slot
// it did not fill. It catches a queue that empties without a kick, which
// leaves the pool one short until something unrelated wakes it.
func TestRefillMakesOnceQueueDrains(t *testing.T) {
	prof.Watch(t, prof.Options{})

	var ro readOnlyFlag
	ro.set(true)
	e, _ := retirer(t, segment.Targets{Content: 2}, ro.check)
	id := e.segment(true)
	e.r.Start()
	e.r.Enqueue(id, segment.NotHeld)
	creates := e.seam.Calls(journaltest.OpCreate)

	e.p.Start()
	quietPool(e.p)
	if got := e.seam.Calls(journaltest.OpCreate); got != creates {
		t.Fatalf("the pool created %d files while a full-sealed segment waited in the retirement queue, want none", got-creates)
	}

	ro.set(false)
	e.r.KickForTest()
	awaitSpares(t, e.p, 2)
	if got := e.seam.Calls(journaltest.OpCreate); got != creates+1 {
		t.Fatalf("the class reached its target with %d makes, want exactly one beside the recycle", got-creates)
	}
	e.r.Stop()
	e.p.Stop()
}

// TestRefillIgnoresEarlySealedQueued: a segment sealed early is unlinked,
// never recycled, so a queue holding only one makes the pool grow as usual.
// It catches counting every not-held entry as a recycle to wait for.
func TestRefillIgnoresEarlySealedQueued(t *testing.T) {
	prof.Watch(t, prof.Options{})

	var ro readOnlyFlag
	ro.set(true)
	e, _ := retirer(t, segment.Targets{Content: 1}, ro.check)
	id := e.segment(false)
	e.r.Start()
	e.r.Enqueue(id, segment.NotHeld)

	e.p.Start()
	awaitSpares(t, e.p, 1)
	e.r.Stop()
	e.p.Stop()
}

// TestRefillIgnoresStoppedOrUnstartedRetirer: a queue nobody drains is no
// recycle to wait for. A retirer that was never started, one stopped with a
// full segment queued, and one stopped while the pool waited on it all leave
// the pool making its spare.
func TestRefillIgnoresStoppedOrUnstartedRetirer(t *testing.T) {
	prof.Watch(t, prof.Options{})

	t.Run("never started", func(t *testing.T) {
		e, _ := retirer(t, segment.Targets{Content: 1}, nil)
		id := e.segment(true)
		e.r.Enqueue(id, segment.NotHeld)

		e.p.Start()
		awaitSpares(t, e.p, 1)
		e.p.Stop()
		e.r.Stop()
	})

	t.Run("stopped before the pool starts", func(t *testing.T) {
		var ro readOnlyFlag
		ro.set(true)
		e, _ := retirer(t, segment.Targets{Content: 1}, ro.check)
		id := e.segment(true)
		e.r.Start()
		e.r.Enqueue(id, segment.NotHeld)
		e.r.Stop()

		e.p.Start()
		awaitSpares(t, e.p, 1)
		e.p.Stop()
	})

	t.Run("stopped while the pool waits", func(t *testing.T) {
		var ro readOnlyFlag
		ro.set(true)
		e, _ := retirer(t, segment.Targets{Content: 1}, ro.check)
		id := e.segment(true)
		e.r.Start()
		e.r.Enqueue(id, segment.NotHeld)
		creates := e.seam.Calls(journaltest.OpCreate)

		e.p.Start()
		quietPool(e.p)
		if got := e.seam.Calls(journaltest.OpCreate); got != creates {
			t.Fatalf("the pool created %d files while the queue held a full-sealed segment, want none", got-creates)
		}
		e.r.Stop()
		awaitSpares(t, e.p, 1)
		e.p.Stop()
	})
}

// TestRollENOSPCAsksRetirementQueue: a roll that finds no spare after a make
// failed with ENOSPC answers ENOSPC only when no full-size retired segment is
// to hand. With one queued, the recycle is what clears the shortage, so the
// roll is refused `spare` and counted; once nothing is queued it is the
// device's ENOSPC, uncounted. It catches a roll that answers ENOSPC while a
// recycle could supply the spare.
func TestRollENOSPCAsksRetirementQueue(t *testing.T) {
	prof.Watch(t, prof.Options{})

	var ro readOnlyFlag
	ro.set(true)
	var r *segment.Retirer
	var id core.SegmentID
	e := rollBuild(t, streamCap, segment.Targets{Content: 1}, func(e rollEnv) {
		// A full-sealed segment on disk, made before the fault so its make
		// succeeds, with the pool not yet started; the test queues it once
		// the pool's make has failed, for a pool that finds it queued makes
		// nothing and never learns the device is full.
		var err error
		id, err = e.maker.Make()
		if err != nil {
			t.Fatalf("the segment's making: %v", err)
		}
		if err := e.set.Take(id); err != nil {
			t.Fatalf("the segment's takeover: %v", err)
		}
		if err := e.set.Seal(id, true); err != nil {
			t.Fatalf("the segment's seal: %v", err)
		}
		r = segment.NewRetirer(segment.RetirerConfig{
			Set: e.set, Cache: e.cache, Pool: e.pool, Maker: e.maker, FS: e.seam.FS(), ReadOnly: ro.check,
		})
		t.Cleanup(r.Stop)

		if err := e.seam.Schedule(journaltest.Failing(journaltest.OpAllocate, "*", 0, syscall.ENOSPC)); err != nil {
			t.Fatalf("Schedule: %v", err)
		}
	}).withStream(t, 1)
	awaitDeviceFull(t, true, e.pool)
	// The retirement is held by the read-only check, so the segment stays
	// queued for the roll below to find.
	r.Start()
	r.Enqueue(id, segment.NotHeld)

	payload := streamTestBody(64, 0x3c)
	h, body := streamWriteHeader(streamFileID(1), 0, payload, 7, core.Version{Counter: 1})

	_, err := e.s.AppendRoll(h, body, segment.Roll{})
	assertSpareRefusal(t, "the roll with a recycle queued", err, 1)
	if errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("the roll with a full-sealed segment queued answered ENOSPC: %v; the recycle clears it", err)
	}
	if got := e.pool.Counters().SpareRefusals; got != 1 {
		t.Fatalf("the refusal was counted %d times, want once", got)
	}

	r.Stop()
	_, err = e.s.AppendRoll(h, body, segment.Roll{})
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("the roll with nothing queued = %v, want the ENOSPC the make left", err)
	}
	if got := e.pool.Counters().SpareRefusals; got != 1 {
		t.Fatalf("the ENOSPC answer counted a refusal: the pool counts %d, want it still 1", got)
	}
}

// groupEnv is a segment set closed through a Group: the roll environment's
// ledger, pool and cache, one stream set, a started retirer and the group over
// all of them.
type groupEnv struct {
	rollEnv
	ss *segment.Streams
	r  *segment.Retirer
	g  *segment.Group
}

// groupBuild builds a group over a fresh seam. Its stream, retirer and pool
// are stopped by the test's own cleanups too, so a test that never closes the
// group still leaves nothing running.
func groupBuild(t *testing.T) groupEnv {
	t.Helper()

	e := rollBuild(t, streamCap, segment.Targets{Content: 2}, nil)
	awaitSpares(t, e.pool, 2)
	ss := segment.NewStreams(1, streamIdentity, e.set, e.pool, e.cache)
	r := segment.NewRetirer(segment.RetirerConfig{
		Set: e.set, Cache: e.cache, Pool: e.pool, Maker: e.maker, FS: e.seam.FS(),
	})
	r.Start()
	t.Cleanup(r.Stop)
	t.Cleanup(func() { _ = ss.Close() })
	g := segment.NewGroup(segment.GroupConfig{Streams: ss, Pool: e.pool, Retirer: r, Cache: e.cache})
	return groupEnv{rollEnv: e, ss: ss, r: r, g: g}
}

// appendOne appends a 64-byte write through the group's stream and returns
// where it landed.
func (e groupEnv) appendOne(t *testing.T, fid int) segment.Placed {
	t.Helper()

	h, body := streamWriteHeader(streamFileID(fid), 0, streamTestBody(64, 0x5a), 7, core.Version{Counter: 1})
	p, err := e.ss.Stream(0).Append(h, body)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return p
}

// TestGroupCloseRefusesAppendsAndReads: once the group is closed, an append
// and a read both answer the journal's closed error. It catches a close that
// leaves reads served by a stream that is no longer appending.
func TestGroupCloseRefusesAppendsAndReads(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := groupBuild(t)
	p := e.appendOne(t, 1)

	// The streams alone are closed first, the cache still open: the read is
	// refused by the stream, not by a cache that happens to be shut.
	if err := e.ss.Close(); err != nil {
		t.Fatalf("the streams' Close = %v, want nil", err)
	}
	if _, err := e.ss.Stream(0).ReadAt(p.Loc.Segment, make([]byte, 16), p.Loc.Off); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the read after the streams closed = %v, want core.ErrClosed", err)
	}
	if err := e.g.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}

	h, body := streamWriteHeader(streamFileID(2), 0, streamTestBody(64, 0x5a), 7, core.Version{Counter: 2})
	if _, err := e.ss.Stream(0).Append(h, body); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the append after Close = %v, want core.ErrClosed", err)
	}
	buf := make([]byte, 16)
	n, err := e.ss.Stream(0).ReadAt(p.Loc.Segment, buf, p.Loc.Off)
	if !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the read after Close = (%d, %v), want core.ErrClosed", n, err)
	}
}

// TestGroupCloseRacesAppendsAndReads: appenders and readers loop while Close
// runs. Every result is a success or the closed error, and no goroutine is
// left once Close returns. It catches a close that frees storage under a
// reader, which surfaces as ErrSegmentGone or a failed read.
func TestGroupCloseRacesAppendsAndReads(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := groupBuild(t)
	p := e.appendOne(t, 1)
	s := e.ss.Stream(0)

	// A roll the pool cannot serve at that instant is refused `spare`, which
	// is a legitimate answer under a busy appender set, not a close artefact.
	ok := func(err error) bool {
		var ns *core.NoSpaceError
		return err == nil || errors.Is(err, core.ErrClosed) || errors.As(err, &ns)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	stop := make(chan struct{})
	for i := range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			h, body := streamWriteHeader(streamFileID(10+i), 0, streamTestBody(64, 0x5a), 7, core.Version{Counter: 1})
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := s.Append(h, body); !ok(err) {
					t.Errorf("an append racing Close = %v, want success or closed", err)
					return
				} else if err != nil && errors.Is(err, core.ErrClosed) {
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			buf := make([]byte, 16)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := s.ReadAt(p.Loc.Segment, buf, p.Loc.Off); err != nil {
					if !errors.Is(err, core.ErrClosed) {
						t.Errorf("a read racing Close = %v, want success or closed", err)
					}
					return
				}
			}
		}()
	}
	close(start)
	time.Sleep(time.Millisecond)
	if err := e.g.Close(); err != nil {
		t.Errorf("Close under load = %v, want nil", err)
	}
	close(stop)
	wg.Wait()
}

// TestGroupCloseWaitsForHeldReader: a reader holding a reference keeps its
// segment's descriptor open through Close, which returns only once the
// reference is released, and then closes the descriptor. It catches a close
// that shuts a descriptor under a reader.
func TestGroupCloseWaitsForHeldReader(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := groupBuild(t)
	p := e.appendOne(t, 1)
	name := streamSegName(p.Loc.Segment)

	ref, err := e.cache.Acquire(p.Loc.Segment)
	if err != nil {
		t.Fatalf("the reader's acquire: %v", err)
	}
	// A failed check must not strand the reference, which would hang the
	// cleanups' own shutdown.
	var release sync.Once
	t.Cleanup(func() { release.Do(ref.Release) })
	closesBefore := e.seam.CallsFor(journaltest.OpClose, name)

	done := make(chan error, 1)
	go func() { done <- e.g.Close() }()

	select {
	case err := <-done:
		t.Fatalf("Close returned (%v) while a reader held a reference, want it waiting", err)
	case <-time.After(50 * time.Millisecond):
	}
	if got := e.seam.CallsFor(journaltest.OpClose, name); got != closesBefore {
		t.Fatalf("the segment's descriptor was closed %d times under a held reference, want %d", got, closesBefore)
	}
	buf := make([]byte, format.RecordHeaderSize)
	if _, err := ref.ReadAt(buf, p.Loc.Off); err != nil {
		t.Fatalf("the read through the held reference: %v", err)
	}

	release.Do(ref.Release)
	if err := <-done; err != nil {
		t.Fatalf("Close after the release = %v, want nil", err)
	}
	if got := e.seam.CallsFor(journaltest.OpClose, name); got <= closesBefore {
		t.Fatalf("the descriptor was never closed after the release: %d closes, had %d", got, closesBefore)
	}
}

// TestGroupCloseStopsWorkersBeforeDescriptors: a retirement parked at its
// remove keeps Close from shutting the cache, so no descriptor closes until
// the retirement finishes. It catches a close that shuts the cache before the
// workers are joined.
func TestGroupCloseStopsWorkersBeforeDescriptors(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := groupBuild(t)
	p := e.appendOne(t, 1)
	name := streamSegName(p.Loc.Segment)

	// A segment sealed early is unlinked, never recycled, so its retirement
	// parks at the seam's remove.
	id, err := e.maker.Make()
	if err != nil {
		t.Fatalf("the segment's making: %v", err)
	}
	if err := e.set.Take(id); err != nil {
		t.Fatalf("the segment's takeover: %v", err)
	}
	if err := e.set.Seal(id, false); err != nil {
		t.Fatalf("the segment's seal: %v", err)
	}
	hold := e.seam.HoldFirst(t, journaltest.OpRemove, streamSegName(id))
	e.r.Enqueue(id, segment.NotHeld)
	select {
	case <-hold.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("the worker never reached the retirement's remove")
	}
	closesBefore := e.seam.CallsFor(journaltest.OpClose, name)

	done := make(chan error, 1)
	go func() { done <- e.g.Close() }()

	select {
	case err := <-done:
		t.Fatalf("Close returned (%v) with a retirement in flight, want it waiting", err)
	case <-time.After(50 * time.Millisecond):
	}
	if got := e.seam.CallsFor(journaltest.OpClose, name); got != closesBefore {
		t.Fatalf("a descriptor closed while a retirement was in flight: %d closes, want %d", got, closesBefore)
	}

	hold.Release()
	if err := <-done; err != nil {
		t.Fatalf("Close after the retirement = %v, want nil", err)
	}
	if got := e.seam.CallsFor(journaltest.OpClose, name); got <= closesBefore {
		t.Fatalf("the descriptor was never closed: %d closes, had %d", got, closesBefore)
	}
}

// TestGroupCloseIdempotent: two concurrent Close calls both return and the
// second answers nil; a group with no idle sealer and no retirer closes too.
func TestGroupCloseIdempotent(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := groupBuild(t)
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- e.g.Close() }()
	}
	first, second := <-errs, <-errs
	if first != nil && second != nil {
		t.Fatalf("concurrent Close calls = %v and %v, want one nil", first, second)
	}
	if err := e.g.Close(); err != nil {
		t.Fatalf("a later Close = %v, want nil", err)
	}

	bare := rollBuild(t, streamCap, segment.Targets{Content: 1}, nil)
	ss := segment.NewStreams(1, streamIdentity, bare.set, bare.pool, bare.cache)
	g := segment.NewGroup(segment.GroupConfig{Streams: ss, Pool: bare.pool, Cache: bare.cache})
	if err := g.Close(); err != nil {
		t.Fatalf("Close without idle sealer or retirer = %v, want nil", err)
	}
}
