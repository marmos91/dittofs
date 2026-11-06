package segment_test

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// endStopBuilder builds the environment the stop checks run over: an open
// maker maximum (a banned stream may still owe its windows to other checks'
// streams sharing the pool), four content spares the takes below draw in id
// order, and a recording consumer registered before anything appends.
func endStopBuilder(t *testing.T) (e rollEnv, rec *endRecorder) {
	t.Helper()

	e = rollBuild(t, streamCap, segment.Targets{Content: 4}, nil)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares == 4 })
	e = e.withStream(t, 1)
	rec = &endRecorder{}
	e.s.OnEnd(rec.OnEnd)
	return e, rec
}

// endStopStreamEnv adapts the roll env to the stream checks' helpers.
func endStopStreamEnv(e rollEnv) streamEnv {
	return streamEnv{seam: e.seam, set: e.set, maker: e.maker, cache: e.cache, budget: e.budget, s: e.s, cap: streamCap}
}

// endStopEndsSegment sets up a single segment with one failed append, waiting
// for its event, and returns its segment id. The caller holds pool counters
// and seam writes to confirm the later stop consumed nothing.
func endStopEndsSegment(t *testing.T, e rollEnv, rec *endRecorder, fail error) core.SegmentID {
	t.Helper()

	se := endStopStreamEnv(e)
	seg := streamSegName(1)
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	placed := streamAppendWrite(t, se, ah, abody)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, fail)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	bPayload := streamTestBody(64, 0x55)
	bh, bbody := streamWriteHeader(streamFileID(2), 64, bPayload, 7, core.Version{Counter: 2})
	if _, err := e.s.Append(bh, bbody); !errors.Is(err, fail) {
		t.Fatalf("the failed Append = %v, want the fault's %v", err, fail)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("disarming the write fault: %v", err)
	}
	evs := rec.awaitCount(t, 1, "the end of the failed append")
	if ev := evs[len(evs)-1]; ev.Segment != placed.Loc.Segment {
		t.Fatalf("the event names segment %d, want the ended one %d", ev.Segment, placed.Loc.Segment)
	}
	return placed.Loc.Segment
}

// TestEndASecondFailureInARowStopsTheStream checks RFC 1 §5.4's stop: a
// stream that took its spare to leave a failed segment meets a failure in
// it before any sync there has succeeded, and the stream stops — the
// second segment's event names the stop, every later Append answers
// ErrStreamStopped wrapping the second failure, and no third spare is
// taken: the pool's ready count and the seam's create and write counts are
// exactly what the two takes left them.
func TestEndASecondFailureInARowStopsTheStream(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	if got := endStopEndsSegment(t, e, rec, syscall.EIO); got != core.SegmentID(1) {
		t.Fatalf("the first ended segment is %d, want 1", got)
	}

	// The second failure. The roll onto segment 2 happens inside the append
	// below: its header write lands and the record after it fails. The rule
	// is armed while the header write is parked at the hold, from the
	// second matching call on — From 2 — so the parked header, the first,
	// passes and the record write, the second, takes the fault. No sync of
	// the segment has succeeded, the fault landing before any, so a failure
	// there is the second segment in a row (RFC 1 §5.4).
	seg2 := streamSegName(2)
	holdH := e.seam.HoldFirst(t, journaltest.OpWrite, seg2)
	appendDone := make(chan error, 1)
	cPayload := streamTestBody(64, 0x7d)
	ch, cbody := streamWriteHeader(streamFileID(3), 0, cPayload, 7, core.Version{Counter: 3})
	go func() {
		_, err := e.s.Append(ch, cbody)
		appendDone <- err
	}()
	<-holdH.Reached()
	if err := e.seam.Schedule(journaltest.FailingFrom(journaltest.OpWrite, seg2, 2, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	holdH.Release()
	if err := <-appendDone; !errors.Is(err, syscall.EIO) {
		t.Fatalf("the second failed Append = %v, want the fault's EIO", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("disarming the write fault: %v", err)
	}
	if got := e.seam.CallsFor(journaltest.OpWrite, seg2); got != 2 {
		t.Fatalf("segment 2 saw %d writes in all, want the header's and the failed record's one alone", got)
	}
	// The counts the stop's appends must leave untouched, pinned once the
	// failure has settled: a pin taken before would race the worker's
	// refill, for this env's maker maximum is open and the pool keeps
	// refilling behind its takes.
	sparesBefore := e.pool.Counters().Spares
	createsBefore := e.seam.Calls(journaltest.OpCreate)
	writes := 0
	for _, op := range []string{journaltest.OpCreate, journaltest.OpWrite, journaltest.OpOpen, journaltest.OpAllocate} {
		writes += e.seam.Calls(op)
	}

	if state, err := e.set.State(2); err != nil || state != segment.Ended {
		t.Fatalf("the second segment's state is %v, %v, want Ended", state, err)
	}
	// The second segment's event names the stop; the first segment's own
	// stands in the recorder ahead of it.
	evs := rec.awaitCount(t, 2, "the end of the second failed append")
	ev := evs[len(evs)-1]
	if ev.Segment != core.SegmentID(2) {
		t.Fatalf("the event names segment %d, want the second segment 2", ev.Segment)
	}
	if !errors.Is(ev.Err, syscall.EIO) {
		t.Fatalf("the event's error is %v, want the fault's EIO", ev.Err)
	}
	if !ev.Stopped {
		t.Fatal("the event does not name the stream stopped, want Stopped: a second segment in a row failed before any sync in it succeeded")
	}

	// Every later Append answers ErrStreamStopped, wrapping the second
	// failure itself, and takes no spare: the pool's ready count and the
	// seam's create and write counts are unchanged by them.
	for i := range 4 {
		payload := streamTestBody(64, byte(0x20+i))
		h, body := streamWriteHeader(streamFileID(4+i), int64(i)*64, payload, 7, core.Version{Counter: uint64(4 + i)})
		_, err := e.s.Append(h, body)
		if !errors.Is(err, segment.ErrStreamStopped) {
			t.Fatalf("Append %d after the stop = %v, want ErrStreamStopped", i, err)
		}
		if !errors.Is(err, syscall.EIO) {
			t.Fatalf("Append %d after the stop = %v, want the second failure's EIO wrapped in it", i, err)
		}
	}
	// The pool's ready count and the seam's create count may rise behind
	// the stop — the worker refills on its own over the open maker
	// maximum — but a third take would spend one of the ready spares, so
	// the count must not have fallen.
	if got := e.pool.Counters().Spares; got < sparesBefore {
		t.Fatalf("the pool holds %d ready spares after the stop's appends, want at least the %d the takes left: no third spare is taken", got, sparesBefore)
	}
	if got := e.seam.Calls(journaltest.OpCreate); got < createsBefore {
		t.Fatalf("the seam saw %d creates after the stop's appends, want at least the %d the two takes made", got, createsBefore)
	}
	// The four ops' totals may rise behind the stop the same way — a
	// worker refill makes, writes and opens its own — but the stop's own
	// appends made none, which the two counts above already witness.
	after := 0
	for _, op := range []string{journaltest.OpCreate, journaltest.OpWrite, journaltest.OpOpen, journaltest.OpAllocate} {
		after += e.seam.Calls(op)
	}
	if after < writes {
		t.Fatalf("the seam saw %d of the four I/O ops after the stop's appends, want at least the %d before them", after, writes)
	}

	// The ledger holds the three takes' segments — two ended, one spared
	// aside — and the two events are all the consumer ever sees.
	c := e.set.Counters()
	if c.Segments != 2 {
		t.Fatalf("the ledger holds %d segments after the stop, want the two the takes drew %d", c.Segments, len(evs))
	}
	endEventsStable(t, rec, "the appends after the stop")
}

// TestEndAFailureAfterTheSparesFirstSyncRolls checks the other side of RFC 1
// §5.4's second-refusal rule: a sync of the spare taken to leave a failed
// segment has succeeded, so a failure in it is not the second in a row and
// the stream keeps rolling — its event does not name a stop, and the next
// append lands in a third segment.
func TestEndAFailureAfterTheSparesFirstSyncRolls(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	if got := endStopEndsSegment(t, e, rec, syscall.EIO); got != core.SegmentID(1) {
		t.Fatalf("the first ended segment is %d, want 1", got)
	}

	// The spare's own Sync succeeds, so the failure below meets a segment
	// whose first sync did. A first landed record is what the Sync covers.
	lPayload := streamTestBody(64, 0x3c)
	lh, lbody := streamWriteHeader(streamFileID(3), 0, lPayload, 7, core.Version{Counter: 3})
	if p := streamAppendWrite(t, endStopStreamEnv(e), lh, lbody); p.Loc.Segment != 2 {
		t.Fatalf("the spare's first append landed in segment %d, want the taken spare 2", p.Loc.Segment)
	}
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the spare's first Sync: %v", err)
	}

	// The failure there: not the second in a row, for the spare's first
	// sync succeeded.
	seg2 := streamSegName(2)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg2, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	fPayload := streamTestBody(64, 0x7d)
	fh, fbody := streamWriteHeader(streamFileID(4), 64, fPayload, 7, core.Version{Counter: 4})
	if _, err := e.s.Append(fh, fbody); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the failed Append in the synced spare = %v, want the fault's EIO", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("disarming the write fault: %v", err)
	}

	// The event is handed over, and it names no stop.
	evs := rec.awaitCount(t, 2, "the end of the synced spare's failed append")
	ev := evs[len(evs)-1]
	if ev.Segment != core.SegmentID(2) {
		t.Fatalf("the event names segment %d, want the second segment 2", ev.Segment)
	}
	if ev.Stopped {
		t.Fatal("the event names the stream stopped, want it rolling: the spare's first sync succeeded before the failure")
	}

	// The next append rolls to a third segment, and is served.
	nPayload := streamTestBody(64, 0x13)
	nh, nbody := streamWriteHeader(streamFileID(5), 0, nPayload, 7, core.Version{Counter: 5})
	next := streamAppendWrite(t, endStopStreamEnv(e), nh, nbody)
	if next.Loc.Segment != 3 {
		t.Fatalf("the append after the synced spare's failure landed in segment %d, want the next one 3", next.Loc.Segment)
	}
	if hdr := rollHeader(t, e, next.Loc.Segment); hdr.Predecessor != 0 {
		t.Fatalf("the new segment names predecessor %d, want none: a failed append ends its segment whatever syncs it saw", hdr.Predecessor)
	}
}

// TestEndASealWhoseRecordSyncSucceededInTheSpareIsNotASecondRefusal checks
// RFC 1 §5.4's "before any sync in that spare has succeeded" against a seal:
// its record sync is a sync of the spare, so a marker sync that fails after
// it is a failure past the spare's first successful sync, and the stream
// rolls on rather than stopping.
func TestEndASealWhoseRecordSyncSucceededInTheSpareIsNotASecondRefusal(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	if got := endStopEndsSegment(t, e, rec, syscall.EIO); got != core.SegmentID(1) {
		t.Fatalf("the first ended segment is %d, want 1", got)
	}
	lh, lbody := streamWriteHeader(streamFileID(3), 0, streamTestBody(64, 0x3c), 7, core.Version{Counter: 3})
	if p := streamAppendWrite(t, endStopStreamEnv(e), lh, lbody); p.Loc.Segment != 2 {
		t.Fatalf("the spare's first append landed in segment %d, want the taken spare 2", p.Loc.Segment)
	}

	// The seal's record sync (the spare's first) passes; its marker sync
	// (the second) fails.
	if err := e.seam.Schedule(journaltest.FailingFrom(journaltest.OpSync, streamSegName(2), 2, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if _, err := e.s.Seal(false); !errors.Is(err, segment.ErrSegmentEnded) {
		t.Fatalf("the seal = %v, want ErrSegmentEnded from the failed marker sync", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("disarming the sync fault: %v", err)
	}
	evs := rec.awaitCount(t, 2, "the failed seal's end")
	ev := evs[len(evs)-1]
	if ev.Segment != 2 || ev.Cause != segment.EndSeal {
		t.Fatalf("the event is %+v, want the seal's end of segment 2", ev)
	}
	if ev.Stopped {
		t.Fatal("the event names the stream stopped, want it rolling: the seal's record sync succeeded in the spare")
	}
	nh, nbody := streamWriteHeader(streamFileID(5), 0, streamTestBody(64, 0x13), 7, core.Version{Counter: 5})
	if p := streamAppendWrite(t, endStopStreamEnv(e), nh, nbody); p.Loc.Segment != 3 {
		t.Fatalf("the append after the seal landed in segment %d, want 3", p.Loc.Segment)
	}
}

// TestEndEROFSStopsTheStream checks the read-only half of RFC 1 §5.4: a
// failure an EROFS carries, wherever it lands, stops the stream with the
// end — the event says so — and the next append takes no spare at all.
func TestEndEROFSStopsTheStream(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)

	// The first landed record gives the segment a sync and the count a
	// value, so the fault below takes the second write alone.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	landed := streamAppendWrite(t, endStopStreamEnv(e), ah, abody)
	seg1 := streamSegName(landed.Loc.Segment)

	// The second write carries EROFS: the device is read-only, so the
	// stream stops with this very end.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg1, 1, syscall.EROFS)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	bPayload := streamTestBody(64, 0x55)
	bh, bbody := streamWriteHeader(streamFileID(2), 64, bPayload, 7, core.Version{Counter: 2})
	if _, err := e.s.Append(bh, bbody); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("the EROFS Append = %v, want the fault's EROFS", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("disarming the write fault: %v", err)
	}

	// The event names the stop: an EROFS failure ends the stream's rolling
	// wherever it met it.
	ev := rec.awaitOne(t, "the end of the EROFS append")
	if ev.Segment != landed.Loc.Segment {
		t.Fatalf("the event names segment %d, want the ended one 1", ev.Segment)
	}
	if !errors.Is(ev.Err, syscall.EROFS) {
		t.Fatalf("the event's error is %v, want the fault's EROFS", ev.Err)
	}
	if !ev.Stopped {
		t.Fatal("the event does not name the stream stopped, want Stopped: an EROFS says the journal calls would keep failing")
	}

	// The next Append answers ErrStreamStopped with the EROFS wrapped in it,
	// and takes no spare: the counts are pinned after the failure has
	// settled, so the worker's own refill is not read as the stream's.
	sparesBefore := e.pool.Counters().Spares
	createsBefore := e.seam.Calls(journaltest.OpCreate)
	cPayload := streamTestBody(64, 0x66)
	ch, cbody := streamWriteHeader(streamFileID(3), 0, cPayload, 7, core.Version{Counter: 3})
	_, err := e.s.Append(ch, cbody)
	if !errors.Is(err, segment.ErrStreamStopped) {
		t.Fatalf("the Append after the EROFS = %v, want ErrStreamStopped", err)
	}
	if !errors.Is(err, syscall.EROFS) {
		t.Fatalf("the Append after the EROFS = %v, want the EROFS wrapped in the stop", err)
	}
	// The pool's ready count may have risen behind the stop — this env's
	// maker maximum is open, so the worker refills on its own — but it must
	// not have fallen: a stopped stream takes nothing.
	if got := e.pool.Counters().Spares; got < sparesBefore {
		t.Fatalf("the pool holds %d ready spares after the stop, want at least the %d it held: a stopped stream takes none", got, sparesBefore)
	}
	if got := e.seam.Calls(journaltest.OpCreate); got < createsBefore {
		t.Fatalf("the seam saw %d creates after the stop, want at least the %d before it: a stopped stream makes no spare", got, createsBefore)
	}
	if state, serr := e.set.State(landed.Loc.Segment); serr != nil || state != segment.Ended {
		t.Fatalf("the EROFS segment's state is %v, %v, want Ended", state, serr)
	}
	endEventsStable(t, rec, "the Appends after the EROFS stop")
}

// TestEndASealThatFoundAnEROFSSyncStopsTheStream checks RFC 1 §5.4's
// read-only rule against a seal that finds the failure: a Sync failed with
// EROFS, and the Seal that follows ends the segment before any End request
// arrives. The seal's event carries the sync's EROFS and names the stop, and
// the next append takes no spare.
func TestEndASealThatFoundAnEROFSSyncStopsTheStream(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	se := endStopStreamEnv(e)
	seg1 := streamSegName(1)
	ah, ab := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	streamAppendWrite(t, se, ah, ab)

	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpSync, seg1, 1, syscall.EROFS)); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Sync(); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("the Sync = %v, want the fault's EROFS", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Seal(false); !errors.Is(err, segment.ErrSegmentEnded) {
		t.Fatalf("the Seal = %v, want ErrSegmentEnded", err)
	}
	ev := rec.awaitOne(t, "the seal's end")
	if ev.Cause != segment.EndSeal {
		t.Fatalf("the event's cause is %v, want the seal's", ev.Cause)
	}
	if !errors.Is(ev.Err, syscall.EROFS) {
		t.Fatalf("the event's error is %v, want the sync's EROFS in its chain", ev.Err)
	}
	if !ev.Stopped {
		t.Fatal("the event does not name the stream stopped, want Stopped: an EROFS says the journal calls would keep failing")
	}
	ch, cb := streamWriteHeader(streamFileID(3), 0, streamTestBody(64, 0x66), 7, core.Version{Counter: 3})
	if _, err := e.s.Append(ch, cb); !errors.Is(err, segment.ErrStreamStopped) {
		t.Fatalf("the Append after the EROFS = %v, want ErrStreamStopped", err)
	}
}

// TestEndAnEROFSRequestStopsTheStreamOverAnAlreadyEndedSegment checks that an
// End request naming an EROFS stops the stream even when another failure got
// to the segment first: the segment's end keeps its first word, but the
// stream's stop is the request's own.
func TestEndAnEROFSRequestStopsTheStreamOverAnAlreadyEndedSegment(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	seg := endStopEndsSegment(t, e, rec, syscall.EIO)
	if ev := rec.one(t); ev.Stopped {
		t.Fatal("the first end names the stream stopped, want it rolling after an EIO")
	}
	if err := e.s.End(seg, syscall.EROFS); err != nil {
		t.Fatalf("End over the ended segment = %v, want nil", err)
	}
	ch, cb := streamWriteHeader(streamFileID(3), 0, streamTestBody(64, 0x66), 7, core.Version{Counter: 3})
	_, err := e.s.Append(ch, cb)
	if !errors.Is(err, segment.ErrStreamStopped) || !errors.Is(err, syscall.EROFS) {
		t.Fatalf("the Append after the EROFS request = %v, want ErrStreamStopped wrapping EROFS", err)
	}
	endEventsStable(t, rec, "the EROFS request over an ended segment")
}

// TestEndAFailedSuccessorHeaderStopsTheStream checks the stop's third path:
// after a failed append, the roll onto the successor fails at the
// successor's own header write — the write the spare's name first sees.
// The append carries the header's refusal, the taken spare ends with no
// event, the stream stays on its ended segment, and every later Append
// answers ErrStreamStopped and takes no further spare.
func TestEndAFailedSuccessorHeaderStopsTheStream(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	if got := endStopEndsSegment(t, e, rec, syscall.EIO); got != core.SegmentID(1) {
		t.Fatalf("the first ended segment is %d, want 1", got)
	}

	// The successor's header write: the next write the seam sees after the
	// first segment's hurts, for the stream sits on an ended segment and
	// the next append rolls. Its refusal is the second the stream meets
	// leaving the ended segment, so the stream stops.
	seg2 := streamSegName(2)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg2, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	cPayload := streamTestBody(64, 0x7d)
	ch, cbody := streamWriteHeader(streamFileID(3), 0, cPayload, 7, core.Version{Counter: 3})
	if _, err := e.s.Append(ch, cbody); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the Append whose successor header write failed = %v, want the fault's EIO", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("disarming the write fault: %v", err)
	}
	if state, err := e.set.State(2); err != nil || state != segment.Ended {
		t.Fatalf("the failed successor's state is %v, %v, want Ended: the spare the failed header write left is held at its cap", state, err)
	}

	// No second event for the spare — nothing was ever reserved in it — and
	// the one event the recorder holds is the first segment's own.
	rec.awaitCount(t, 1, "the first segment's end")
	endEventsStable(t, rec, "the spare the failed header write ended")

	// Every later Append answers ErrStreamStopped, wrapping the header
	// write's own refusal, and no further spare is taken: the two takes
	// spent two of the maker's spares and the count says so.
	sparesBefore := e.pool.Counters().Spares
	createsBefore := e.seam.Calls(journaltest.OpCreate)
	dPayload := streamTestBody(64, 0x13)
	dh, dbody := streamWriteHeader(streamFileID(4), 0, dPayload, 7, core.Version{Counter: 4})
	_, err := e.s.Append(dh, dbody)
	if !errors.Is(err, segment.ErrStreamStopped) {
		t.Fatalf("the Append after the stopped roll = %v, want ErrStreamStopped", err)
	}
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("the Append after the stopped roll = %v, want the header write's EIO wrapped in the stop", err)
	}
	if got := e.pool.Counters().Spares; got < sparesBefore {
		t.Fatalf("the pool holds %d ready spares after the stop, want at least the %d before it: no further spare is taken", got, sparesBefore)
	}
	if got := e.seam.Calls(journaltest.OpCreate); got < createsBefore {
		t.Fatalf("the seam saw %d creates after the stop, want at least the %d before it", got, createsBefore)
	}
	// The next spare never entered the ledger as a segment: State answers
	// ErrNotInSet, for spares and segments sit in different tables.
	if _, err := e.set.State(3); !errors.Is(err, segment.ErrNotInSet) {
		t.Fatalf("the third spare's state answered %v, want ErrNotInSet: no further take ever drew it", err)
	}
}

// TestEndRequestedSecondFailureStops checks the requested end's half of the
// stop: the durability code ends a segment whose Sync failed, in the spare
// the stream took to leave a failed segment, before any sync of that spare
// has succeeded, and the requested end's event says the stream stopped.
func TestEndRequestedSecondFailureStops(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	if got := endStopEndsSegment(t, e, rec, syscall.EIO); got != core.SegmentID(1) {
		t.Fatalf("the first ended segment is %d, want 1", got)
	}

	// The spare's first Sync fails before anything of its own has
	// succeeded, and the durability code asks for its end.
	seg2 := streamSegName(2)
	aPayload := streamTestBody(64, 0x3c)
	ah, abody := streamWriteHeader(streamFileID(3), 0, aPayload, 7, core.Version{Counter: 3})
	if p := streamAppendWrite(t, endStopStreamEnv(e), ah, abody); p.Loc.Segment != 2 {
		t.Fatalf("the spare's first append landed in segment %d, want the taken spare 2", p.Loc.Segment)
	}
	syncFault := fmt.Errorf("the failed sync: %w", syscall.EIO)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpSync, seg2, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if err := e.s.Sync(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the spare's faulted Sync = %v, want the fault's EIO", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("disarming the sync fault: %v", err)
	}
	if err := e.s.End(core.SegmentID(2), syncFault); err != nil {
		t.Fatalf("the requested End of the sync-failed spare = %v, want nil", err)
	}

	// The requested end's event names the stop: no sync of the spare
	// succeeded before the failure, so the end is the second refusal the
	// stream met in a row.
	evs := rec.awaitCount(t, 2, "the requested end of the unsynced spare")
	ev := evs[len(evs)-1]
	if ev.Segment != core.SegmentID(2) {
		t.Fatalf("the event names segment %d, want the spare 2", ev.Segment)
	}
	if ev.Cause != segment.EndRequested {
		t.Fatalf("the event's cause is %s, want the requested end", ev.Cause)
	}
	if !errors.Is(ev.Err, syscall.EIO) {
		t.Fatalf("the event's error is %v, want the caller's EIO", ev.Err)
	}
	if !ev.Stopped {
		t.Fatal("the event does not name the stream stopped, want Stopped: the spare's first sync failed before any succeeded")
	}

	// The later appends answer the stop, and take nothing.
	sparesBefore := e.pool.Counters().Spares
	nPayload := streamTestBody(64, 0x13)
	nh, nbody := streamWriteHeader(streamFileID(4), 0, nPayload, 7, core.Version{Counter: 4})
	_, err := e.s.Append(nh, nbody)
	if !errors.Is(err, segment.ErrStreamStopped) {
		t.Fatalf("the Append after the requested stop = %v, want ErrStreamStopped", err)
	}
	if got := e.pool.Counters().Spares; got < sparesBefore {
		t.Fatalf("the pool holds %d ready spares after the stop, want at least the %d before it", got, sparesBefore)
	}
	endEventsStable(t, rec, "the Append after the requested stop")
}

// TestEndAppenderAlreadyInsideTheRollIsStoppedAtTheLoopTop checks the stop's
// inner check: an append that passed the loop-top check before the failure that
// stopped the stream — one already running AppendRoll's loop when the second
// refusal lands — wakes at the loop top, re-reads the stop there and takes no
// third spare. The park hook holds the second append at the loop top past the
// loop-top check, and the third's failure stops the stream while it is held.
func TestEndAppenderAlreadyInsideTheRollIsStoppedAtTheLoopTop(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The hook is cleared once the stream is closed — registered first, it
	// runs last — for an append still entering the loop reads it until Close
	// has waited for it.
	t.Cleanup(func() { segment.StreamSetAppendParkHook(nil) })
	e, rec := endStopBuilder(t)
	if got := endStopEndsSegment(t, e, rec, syscall.EIO); got != core.SegmentID(1) {
		t.Fatalf("the first ended segment is %d, want 1", got)
	}
	se := endStopStreamEnv(e)

	// X lands in segment 2, so the stream's spare is active there.
	xh, xb := streamWriteHeader(streamFileID(3), 0, streamTestBody(64, 1), 7, core.Version{Counter: 3})
	if p := streamAppendWrite(t, se, xh, xb); p.Loc.Segment != 2 {
		t.Fatalf("X landed in segment %d, want the taken spare 2", p.Loc.Segment)
	}

	// Y parks at the loop top past the loop-top check, holding no lock.
	parked := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseY := func() { releaseOnce.Do(func() { close(release) }) }
	segment.StreamSetAppendParkHook(func() {
		segment.StreamSetAppendParkHook(nil)
		close(parked)
		<-release
	})
	// Registered after the stream's Close, so it runs before it: a check that
	// fails while Y is parked must free it, or Close waits on it and the
	// package hangs to its timeout.
	t.Cleanup(releaseY)
	yDone := endAppend(se, 4, 64, 2)
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("Y never entered the loop")
	}

	// Z's write in segment 2 fails: an unsynced spare taken to leave an ended
	// segment, the second refusal in a row, and the stream stops while Y
	// sits parked at the loop top.
	seg2 := streamSegName(2)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg2, 1, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	zDone := endAppend(se, 5, 128, 3)
	z := <-zDone
	if !errors.Is(z.err, syscall.EIO) {
		t.Fatalf("Z's append = %v, want the fault's EIO", z.err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}
	evs := rec.awaitCount(t, 2, "the second refusal's end")
	if !evs[len(evs)-1].Stopped {
		t.Fatal("the second refusal's event does not name the stop")
	}

	// Y wakes at the loop top and must answer the stop, not take a third
	// spare: no further take happens, and segment 3 stays a spare.
	releaseY()
	y := <-yDone
	if !errors.Is(y.err, segment.ErrStreamStopped) {
		t.Fatalf("the append parked across the stop = %v, want ErrStreamStopped", y.err)
	}
	if _, serr := e.set.State(3); !errors.Is(serr, segment.ErrNotInSet) {
		t.Fatalf("a third spare was taken after the stop: Y landed in %d, State(3) err = %v", y.p.Loc.Segment, serr)
	}
	endEventsStable(t, rec, "the parked append past the stop")
}

// TestEndCloseDuringTheRollsInstallWait checks the close race at the roll's
// install: a Close that lands while the roll waits out the ended segment's
// in-flight appends hand-over unwinds the taken spare itself, so a successor
// is never installed over a closed stream — neither its segment nor its ref.
func TestEndCloseDuringTheRollsInstallWait(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := rollBuild(t, streamCap, segment.Targets{Content: 2}, nil)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 2 })
	e = e.withStream(t, 1)
	rec := &endRecorder{}
	e.s.OnEnd(rec.OnEnd)
	se := endStopStreamEnv(e)
	seg := streamSegName(1)

	// A lands, B's write parks at the seam, and C's fails past it, so the
	// segment ends while B is still in flight.
	ah, ab := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	streamAppendWrite(t, se, ah, ab)
	holdB := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	bDone := endAppend(se, 2, 64, 0x55)
	<-holdB.Reached()
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	if c := <-endAppend(se, 3, 128, 0x7d); !errors.Is(c.err, syscall.EIO) {
		t.Fatalf("C's append = %v, want the fault's EIO", c.err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}

	// D rolls past the ended segment; the roll writes segment 2's header and
	// parks at the install wait, held there by B's still-parked write.
	dDone := endAppend(se, 4, 192, 0x11)
	deadline := time.Now().Add(10 * time.Second)
	for e.seam.CallsFor(journaltest.OpWrite, streamSegName(2)) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the roll never wrote the successor's header")
		}
		time.Sleep(time.Millisecond)
	}

	// The Close lands while the roll is parked at install: the taken spare is
	// the close's to unwind once the roll releases the wait, for the stream
	// publishes no location past a close. Close parks in its in-flight wait —
	// D's append is counted — so the check holds B's write back and asserts
	// the close has not returned over a bounded wait.
	closed := make(chan error, 1)
	go func() { closed <- e.s.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v while the roll was still parked at install: it must wait the append out", err)
	case <-time.After(200 * time.Millisecond):
	}

	// B's write lands — the wait the roll parks on goes false for it, the
	// close's flag carries, and the roll answers the close.
	holdB.Release()
	if b := <-bDone; b.err != nil {
		t.Fatalf("B's held append = %v", b.err)
	}
	d := <-dDone
	if !errors.Is(d.err, segment.ErrStreamClosed) {
		t.Fatalf("D's append = %v, want ErrStreamClosed: the roll found the close at the install wait", d.err)
	}

	// The taken spare is ended in the ledger as a closed take's unwind leaves
	// it, never Active: no successor was installed over the closed stream.
	if state, serr := e.set.State(2); serr != nil || state != segment.Ended {
		t.Fatalf("the closed take's spare stands %v, %v, want Ended by the unwind the close owned", state, serr)
	}
	// The event of the ended segment 1 still hands over: the close's
	// exemption covers the take's spare, not the segment B and C's own.
	rec.awaitOne(t, "the ended segment's event past the close")
	endEventsStable(t, rec, "the close's take of segment 2")
}

// endStopHeldPair stages the interleaving the stop must survive: B and C are
// both reserved in segment 1 and parked at the seam, C's write then fails
// with EIO and ends the segment while B still writes, and B's write fails
// with second afterwards, onto a segment already ended. It returns the
// environment and recorder with both appends answered, B's answer first.
func endStopHeldPair(t *testing.T, second error) (e rollEnv, rec *endRecorder, b, c endAppendResult) {
	t.Helper()

	e, rec = endStopBuilder(t)
	se := endStopStreamEnv(e)
	seg1 := streamSegName(1)

	// A lands first so the segment exists and B and C sit above it.
	ah, abody := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	streamAppendWrite(t, se, ah, abody)

	// Both writes are reserved before either fails: B parks at the first
	// hold and C at the second, so the slot order B < C is fixed and each
	// consults the fault only at its own release.
	holdB := e.seam.HoldFirst(t, journaltest.OpWrite, seg1)
	bDone := endAppend(se, 2, 64, 0x55)
	<-holdB.Reached()
	holdC := e.seam.HoldFirst(t, journaltest.OpWrite, seg1)
	cDone := endAppend(se, 3, 128, 0x7d)
	<-holdC.Reached()

	// Two rules over the same writes: the first consult — C's, released
	// first — meets the EIO, the second — B's — meets second.
	if err := e.seam.Schedule(
		journaltest.Failing(journaltest.OpWrite, seg1, 1, syscall.EIO),
		journaltest.FailingFrom(journaltest.OpWrite, seg1, 2, 1, second),
	); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	holdC.Release()
	c = <-cDone
	if !errors.Is(c.err, syscall.EIO) {
		t.Fatalf("C's Append = %v, want the first fault's EIO", c.err)
	}
	holdB.Release()
	b = <-bDone
	if !errors.Is(b.err, second) {
		t.Fatalf("B's Append = %v, want the second fault %v", b.err, second)
	}
	return e, rec, b, c
}

// TestEndAnEROFSOnAStragglerOfAnEndedSegmentStopsTheStream checks RFC 1
// §5.4's read-only rule for a write that fails after its segment was already
// ended: C's EIO ends segment 1 with B still writing, then B's write fails
// with EROFS. The device is read-only whichever write met it first, so the
// one event the segment hands over names the stop and the next Append
// answers ErrStreamStopped, with no spare taken.
func TestEndAnEROFSOnAStragglerOfAnEndedSegmentStopsTheStream(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec, _, _ := endStopHeldPair(t, syscall.EROFS)

	ev := rec.awaitOne(t, "the end of the segment both failures met")
	if ev.Segment != 1 {
		t.Fatalf("the event names segment %d, want 1", ev.Segment)
	}
	if !errors.Is(ev.Err, syscall.EIO) {
		t.Fatalf("the event's error is %v, want the first failure's EIO: the end is the first failure's", ev.Err)
	}
	if !ev.Stopped {
		t.Fatal("the event does not name the stop, want Stopped: a straggler's EROFS says the device is read-only")
	}

	sparesBefore := e.pool.Counters().Spares
	createsBefore := e.seam.Calls(journaltest.OpCreate)
	dh, dbody := streamWriteHeader(streamFileID(4), 0, streamTestBody(64, 0x66), 7, core.Version{Counter: 4})
	_, err := e.s.Append(dh, dbody)
	if !errors.Is(err, segment.ErrStreamStopped) {
		t.Fatalf("the Append after the straggler's EROFS = %v, want ErrStreamStopped", err)
	}
	if !errors.Is(err, syscall.EROFS) {
		t.Fatalf("the Append after the straggler's EROFS = %v, want the EROFS wrapped in the stop", err)
	}
	if got := e.pool.Counters().Spares; got < sparesBefore {
		t.Fatalf("the pool holds %d ready spares after the stop, want at least the %d it held: a stopped stream takes none", got, sparesBefore)
	}
	if got := e.seam.Calls(journaltest.OpCreate); got < createsBefore {
		t.Fatalf("the seam saw %d creates after the stop, want at least the %d before it: a stopped stream makes no spare", got, createsBefore)
	}
	// Segment 2 would be the first spare a roll takes: the stop took none.
	if state, serr := e.set.State(2); !errors.Is(serr, segment.ErrNotInSet) {
		t.Fatalf("segment 2 stands %v, %v, want it not in the set: a stopped stream takes no spare", state, serr)
	}
	endEventsStable(t, rec, "the Append after the straggler's stop")
}

// TestEndANonEROFSStragglerFailureDoesNotStopTheStream is the mirror: a
// second write failing with EIO on a segment already ended is the same end,
// not a second refusal — the rule counts segments the stream took, and a
// straggler's write is in the segment the first failure ended. The event is
// not Stopped and the next Append rolls onto a spare.
func TestEndANonEROFSStragglerFailureDoesNotStopTheStream(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec, _, _ := endStopHeldPair(t, syscall.EIO)

	ev := rec.awaitOne(t, "the end of the segment both failures met")
	if ev.Stopped {
		t.Fatal("the event names the stop, want it not Stopped: a second EIO in the same segment is no second refusal")
	}

	dh, dbody := streamWriteHeader(streamFileID(4), 0, streamTestBody(64, 0x66), 7, core.Version{Counter: 4})
	p, err := e.s.Append(dh, dbody)
	if err != nil {
		t.Fatalf("the Append after both failures = %v, want it rolled onto the next spare", err)
	}
	if p.Loc.Segment == 1 {
		t.Fatalf("the Append landed in segment 1, want the successor of the ended one")
	}
	endEventsStable(t, rec, "the Append past the ended segment")
}

// TestEndSyncInFlightAcrossAFailureRollOwnsItsOwnSegment checks the stream's
// own Sync against the failure path's roll: a Sync of the ended segment that
// is in flight when the failure lands holds the roll back — the event's
// hand-over waits for it, and the roll waits for the event — so the
// successor starts blank: its tracker takes no window the old segment's fsync
// made durable (R10) and its second-refusal word stays blank until a sync of
// the successor itself succeeds. What the check pins is the roll's wait for
// the event, hence for the in-flight sync: Sync itself carries no guard
// against the stream moving, because the wait keeps it from moving.
func TestEndSyncInFlightAcrossAFailureRollOwnsItsOwnSegment(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	se := endStopStreamEnv(e)
	seg1 := streamSegName(1)

	// A and a second record land, so segment 1's completed prefix is well
	// past segment 2's own record slots.
	ah, ab := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	streamAppendWrite(t, se, ah, ab)
	a2h, a2b := streamWriteHeader(streamFileID(1), 64, streamTestBody(512, 0x43), 7, core.Version{Counter: 2})
	streamAppendWrite(t, se, a2h, a2b)

	// A Sync of segment 1 parks at the seam.
	holdS := e.seam.HoldFirst(t, journaltest.OpSync, seg1)
	syncDone := make(chan error, 1)
	go func() { syncDone <- e.s.Sync() }()
	<-holdS.Reached()

	// C's write fails, ending segment 1 while the sync is in flight. The
	// event and the roll wait for the sync, so it returns first; then the
	// event hands over, and D rolls onto segment 2 and lands there.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg1, 1, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	if c := <-endAppend(se, 3, 1024, 0x7d); !errors.Is(c.err, syscall.EIO) {
		t.Fatalf("C's append = %v, want the fault's EIO", c.err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}
	holdS.Release()
	if err := <-syncDone; err != nil {
		t.Fatalf("the straddling sync = %v, want nil", err)
	}
	rec.awaitCount(t, 1, "the ended segment's event")
	d := <-endAppend(se, 4, 0, 0x11)
	if d.err != nil || d.p.Loc.Segment != 2 {
		t.Fatalf("D's append = %v in segment %d, want a landing in 2", d.err, d.p.Loc.Segment)
	}

	// The sync's success belongs to segment 1 alone: segment 2's tracker
	// claims no window bytes as synced-through, for no fsync of segment 2 ran.
	ePl := <-endAppend(se, 5, 0, 0x22)
	if ePl.err != nil {
		t.Fatal(ePl.err)
	}
	if got, _ := streamReadRecord(t, se, ePl.p.Loc); got.SyncedThrough > d.p.Loc.Off {
		t.Fatalf("the record in segment 2 claims SyncedThrough %d over bytes its own segment never fsynced, want at most D's offset %d", got.SyncedThrough, d.p.Loc.Off)
	}

	// The straddling sync's success does not reach the second-refusal word
	// either: a failure in segment 2 before any sync of it has succeeded is
	// the second refusal in a row, and the event must say the stream stopped.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, streamSegName(2), 1, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	if f := <-endAppend(se, 6, 0, 0x33); !errors.Is(f.err, syscall.EIO) {
		t.Fatalf("F's append = %v, want the fault's EIO", f.err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}
	evs := rec.awaitCount(t, 2, "the second failure's end")
	if ev := evs[len(evs)-1]; !ev.Stopped {
		t.Fatalf("the event after a failure in the unsynced spare says Stopped=false, want true: the straddling sync proved nothing of segment 2 durable")
	}
}

// TestEndTheStraddlingSyncsFailureMarksItsOwnSegmentAlone checks the other
// half of the straddling sync: a failure of the sync in flight when the
// segment ended neither marks the successor sync-failed nor ends it for a
// failure it never had. It answers its error to its caller, which owns its
// window through End. The roll's wait for the event, hence for the in-flight
// sync, is what keeps the failure on the segment it covered: the sync's own
// failure path holds no check of the stream's segment.
func TestEndTheStraddlingSyncsFailureMarksItsOwnSegmentAlone(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	se := endStopStreamEnv(e)
	seg1 := streamSegName(1)

	ah, ab := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	streamAppendWrite(t, se, ah, ab)
	holdS := e.seam.HoldFirst(t, journaltest.OpSync, seg1)
	syncDone := make(chan error, 1)
	go func() { syncDone <- e.s.Sync() }()
	<-holdS.Reached()

	// Two failures queued: C's write fails and ends segment 1, and the held
	// sync itself fails with EIO once D's roll has installed segment 2. Both
	// rules are armed while the sync is parked at its hold — the first rule
	// spends its round on C's write, the next matching write, and the sync
	// rule's round is spent on the fsync the release lets run.
	if err := e.seam.Schedule(
		journaltest.Failing(journaltest.OpWrite, seg1, 1, syscall.EIO),
		journaltest.Failing(journaltest.OpSync, seg1, 1, syscall.EIO),
	); err != nil {
		t.Fatal(err)
	}
	if c := <-endAppend(se, 3, 64, 0x7d); !errors.Is(c.err, syscall.EIO) {
		t.Fatalf("C's append = %v, want the fault's EIO", c.err)
	}
	// The sync rule stays armed: its one round is what the held fsync
	// consults at its release, and nothing may disarm it before that. The
	// event and the roll wait for the sync, so it returns first.
	holdS.Release()
	if err := <-syncDone; !errors.Is(err, syscall.EIO) {
		t.Fatalf("the straddling sync = %v, want the fault's EIO", err)
	}
	rec.awaitCount(t, 1, "the ended segment's event")
	d := <-endAppend(se, 4, 0, 0x11)
	if d.err != nil || d.p.Loc.Segment != 2 {
		t.Fatalf("D's append = %v in segment %d, want a landing in 2", d.err, d.p.Loc.Segment)
	}

	// The successor is untouched: it keeps its roll's state, takes further
	// appends —no marker, no freeze, no sync-failed state the straddling
	// failure would have leaked into it.
	if state, serr := e.set.State(2); serr != nil || state != segment.Active {
		t.Fatalf("segment 2 stands %v, %v after the straddling sync's failure, want Active: the failure of a sync past its segment owns nothing of the successor", state, serr)
	}
	ePl := <-endAppend(se, 5, 0, 0x22)
	if ePl.err != nil || ePl.p.Loc.Segment != 2 {
		t.Fatalf("the append past the failed straddling sync = %v in segment %d, want a landing in 2", ePl.err, ePl.p.Loc.Segment)
	}
	// A sync of the successor still succeeds: the failure landed on the old
	// segment's own fsync, not on the stream's word for the new one.
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the successor's own sync = %v, want nil", err)
	}
	endEventsStable(t, rec, "the append past the failed straddling sync")
}

// TestEndSyncedSegmentBeforeItsFailureStopsOnTheSecondRefusal checks the
// syncedOnce reset at the roll's install: the first segment synced
// successfully before its write failed, so the stream's word that a spare was
// proved durable belongs to the first segment alone. A failure in the second,
// unsynced spare taken to leave that ended segment stops the stream — a stale
// syncedOnce from the first would let the stream take a third spare.
func TestEndSyncedSegmentBeforeItsFailureStopsOnTheSecondRefusal(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)

	// Segment 1 lands a record and syncs it, then a second write fails
	// there: an ended segment whose own sync succeeded, the state every
	// other stop check builds without.
	se := endStopStreamEnv(e)
	seg1 := streamSegName(1)
	ah, ab := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	a := streamAppendWrite(t, se, ah, ab)
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the first segment's Sync: %v", err)
	}
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg1, 1, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	bh, bb := streamWriteHeader(streamFileID(2), 64, streamTestBody(64, 0x55), 7, core.Version{Counter: 2})
	if _, err := e.s.Append(bh, bb); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the failed Append = %v, want the fault's EIO", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}
	if state, serr := e.set.State(a.Loc.Segment); serr != nil || state != segment.Ended {
		t.Fatalf("the synced-and-failed segment stands %v, %v, want Ended", state, serr)
	}
	rec.awaitCount(t, 1, "the first segment's end")

	// The failure in the unsynced spare taken to leave that ended segment is
	// the second refusal in a row, and the stream must stop with it. The
	// rule is armed while segment 2's header write is parked at the hold —
	// From 2 — so the parked header passes and the record write, the second
	// matching call, takes the fault, as the first stop check stages it.
	seg2 := streamSegName(2)
	holdH := e.seam.HoldFirst(t, journaltest.OpWrite, seg2)
	appendDone := make(chan error, 1)
	ch, cb := streamWriteHeader(streamFileID(3), 0, streamTestBody(64, 0x7d), 7, core.Version{Counter: 3})
	go func() {
		_, err := e.s.Append(ch, cb)
		appendDone <- err
	}()
	<-holdH.Reached()
	if err := e.seam.Schedule(journaltest.FailingFrom(journaltest.OpWrite, seg2, 2, 1, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	holdH.Release()
	if err := <-appendDone; !errors.Is(err, syscall.EIO) {
		t.Fatalf("the second failed Append = %v, want the fault's EIO", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}
	evs := rec.awaitCount(t, 2, "the second refusal's end")
	ev := evs[len(evs)-1]
	if ev.Segment != core.SegmentID(2) {
		t.Fatalf("the event names segment %d, want the second segment 2", ev.Segment)
	}
	if !ev.Stopped {
		t.Fatal("the event does not name the stop, want Stopped: the first segment's sync proved nothing of the second one durable")
	}

	// The stopped stream answers every later append and takes no spare.
	sparesBefore := e.pool.Counters().Spares
	nh, nb := streamWriteHeader(streamFileID(4), 0, streamTestBody(64, 0x13), 7, core.Version{Counter: 4})
	if _, err := e.s.Append(nh, nb); !errors.Is(err, segment.ErrStreamStopped) {
		t.Fatalf("the Append after the stop = %v, want ErrStreamStopped", err)
	}
	if got := e.pool.Counters().Spares; got < sparesBefore {
		t.Fatalf("the pool holds %d ready spares after the stop, want at least the %d before it", got, sparesBefore)
	}
}

// TestEndEROFSOnASuccessorHeaderWriteStopsTheStream checks the read-only half
// of the successor-header gate on an ordinary roll: a spare whose header write
// fails with EROFS — the stream leaving no failed segment behind at all —
// stops the stream, for the read-only device ends its rolling wherever the
// roll meets it. Taken-name assertions aside, the stream holds nothing, so
// the EROFS half of the gate stands alone, not leaning on a failure that
// would stop it anyway.
func TestEndEROFSOnASuccessorHeaderWriteStopsTheStream(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)

	// The stream holds nothing yet: its first append rolls from nothing, and
	// the successor header the roll writes is the first write of segment 1.
	// The EROFS rule takes exactly it — a read-only device ends the stream's
	// rolling wherever it meets it, here on a stream that never held a
	// failed segment at all.
	seg1 := streamSegName(1)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg1, 1, syscall.EROFS)); err != nil {
		t.Fatal(err)
	}
	h, b := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	_, err := e.s.Append(h, b)
	if !errors.Is(err, syscall.EROFS) {
		t.Fatalf("the append over the EROFS successor header = %v, want the fault's EROFS", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}
	// The take unwound: the spare holds no record, so its end is the take's
	// alone, and the stream still holds nothing.
	if state, serr := e.set.State(1); serr != nil || state != segment.Ended {
		t.Fatalf("the unwound take stands %v, %v, want Ended", state, serr)
	}

	// The stream stopped with the EROFS: the stopped word wraps the header
	// write's own refusal, no event hands over for the spared take, and the
	// next append answers ErrStreamStopped with no further spare taken.
	sparesBefore := e.pool.Counters().Spares
	createsBefore := e.seam.Calls(journaltest.OpCreate)
	nh, nb := streamWriteHeader(streamFileID(3), 0, streamTestBody(64, 0x13), 7, core.Version{Counter: 3})
	_, err = e.s.Append(nh, nb)
	if !errors.Is(err, segment.ErrStreamStopped) {
		t.Fatalf("the Append after the EROFS = %v, want ErrStreamStopped", err)
	}
	if !errors.Is(err, syscall.EROFS) {
		t.Fatalf("the Append after the EROFS = %v, want the EROFS wrapped in the stop", err)
	}
	if got := e.pool.Counters().Spares; got < sparesBefore {
		t.Fatalf("the pool holds %d ready spares after the stop, want at least the %d it held", got, sparesBefore)
	}
	if got := e.seam.Calls(journaltest.OpCreate); got < createsBefore {
		t.Fatalf("the seam saw %d creates after the stop, want at least the %d before it", got, createsBefore)
	}
	endEventsStable(t, rec, "the appends past the EROFS stop")

	// The taken spare ended in the ledger with no event: the stream never
	// reserved a record in it, and its end is the take's unwind alone. A
	// second take of the pool's next spare never happened: the stopped
	// stream answers before any roll could draw one.
	if _, serr := e.set.State(2); !errors.Is(serr, segment.ErrNotInSet) {
		t.Fatalf("the pool's next spare stands in the set as %v, want ErrNotInSet: the stopped stream took none of it", serr)
	}
}
