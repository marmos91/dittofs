package segment_test

import (
	"errors"
	"runtime"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
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
