package segment_test

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// endAttempts bounds the spins an event check makes before it gives up: the
// notifier runs on a goroutine the test cannot hold, so a check that must see
// no event yields this many times to be certain a wrongly-timed hand-over
// would have landed — and one that waits for an event crosses the same bound.
const endAttempts = 1 << 18

// endRecorder is a recording consumer of a stream's end events: a mutex and a
// slice, never blocking, so a check can read what arrived on its own turn.
type endRecorder struct {
	mu sync.Mutex
	ev []segment.EndEvent
}

// OnEnd is the consumer's own method, handed to Stream.OnEnd.
func (r *endRecorder) OnEnd(ev segment.EndEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev = append(r.ev, ev)
}

// events returns a copy of what arrived so far.
func (r *endRecorder) events() []segment.EndEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ev)
}

// count returns how many events arrived so far, for the checks that poll it.
func (r *endRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ev)
}

// awaitCount fails the test unless at least n events arrived within a bounded
// spin, and returns all that did: a check whose scenario hands over more
// than one event waits for the last of them without pinning the ones before.
func (r *endRecorder) awaitCount(t *testing.T, n int, what string) []segment.EndEvent {
	t.Helper()

	for range endAttempts {
		if r.count() >= n {
			break
		}
		runtime.Gosched()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ev) < n {
		t.Fatalf("%s: %d end event(s) arrived, want at least %d", what, len(r.ev), n)
	}
	return slices.Clone(r.ev)
}

// one returns the single event the recorder holds, failing the test when it
// holds any other count.
func (r *endRecorder) one(t *testing.T) segment.EndEvent {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ev) != 1 {
		t.Fatalf("the consumer holds %d end events, want one", len(r.ev))
	}
	return r.ev[0]
}

// awaitOne fails the test unless exactly one event arrived within a bounded
// spin, and returns it.
func (r *endRecorder) awaitOne(t *testing.T, what string) segment.EndEvent {
	t.Helper()

	for range endAttempts {
		if r.count() == 0 {
			runtime.Gosched()
			continue
		}
		return r.one(t)
	}
	t.Fatalf("%s: no end event arrived within the bounded wait", what)
	return segment.EndEvent{}
}

// awaitNone fails the test if any event arrives within a bounded spin: the
// notifier is a goroutine the test cannot hold, so a check that must see no
// event yields this many times rather than sleeping, certain that a wrongly
// timed hand-over would have landed within them.
func (r *endRecorder) awaitNone(t *testing.T, what string) {
	t.Helper()

	for i := range endAttempts {
		if n := r.count(); n > 0 {
			t.Fatalf("%s: %d end event(s) arrived, want none: %v", what, n, r.events())
		}
		if i%64 == 0 {
			runtime.Gosched()
		}
	}
}

// endEventsStable fails the test unless the recorder's event count stays where
// it stands through a bounded spin: the "no second event" checks read it, for
// the one event a segment hands over has already arrived by the time they run
// and the bound they keep is that nothing more lands.
func endEventsStable(t *testing.T, r *endRecorder, what string) {
	t.Helper()

	n := r.count()
	for i := range endAttempts {
		if got := r.count(); got != n {
			t.Fatalf("%s: the consumer now holds %d end events, want the %d it held: the one end a segment owns hands over once", what, got, n)
		}
		if i%64 == 0 {
			runtime.Gosched()
		}
	}
}

// endZeroSlotWithConsumer runs the zero-slot choreography over a stream whose
// consumer is already registered, and returns the environment, the recording
// consumer, the ended segment's id, the failed slot's offset and the first
// record's end. The env's stream is built the way streamOver builds it — the
// same cap class helpers read — with the consumer handed to it before any
// append, the only time OnEnd accepts one.
func endZeroSlotWithConsumer(t *testing.T) (e streamEnv, rec *endRecorder, id core.SegmentID, cOff, aEnd int64) {
	t.Helper()

	seg := streamSegName(1)
	rec = &endRecorder{}
	e = streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })
	e.s.OnEnd(rec.OnEnd)

	// Record A lands and is synced, so the segment and its window hold a
	// value the later checks read back.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	placed := streamAppendWrite(t, e, ah, abody)
	aEnd = placed.Loc.Off + placed.Loc.Len
	id = placed.Loc.Segment
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the first Sync: %v", err)
	}

	// B's write parks at the seam: its slot is reserved — the lowest of the
	// three — but its bytes have not landed.
	holdB := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	bDone := endAppend(e, 2, 64, 0x55)
	<-holdB.Reached()

	// C's write parks at the second one-shot hold, D's passes both and
	// lands, and C's fails with the seam's EIO at its resumed release.
	holdC := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	cDone := endAppend(e, 3, 128, 0x7d)
	<-holdC.Reached()
	dDone := endAppend(e, 4, 192, 0x13)
	d := <-dDone
	if d.err != nil {
		t.Fatalf("the append past the held ones: %v", d.err)
	}
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	holdC.Release()
	c := <-cDone
	if !errors.Is(c.err, syscall.EIO) {
		t.Fatalf("the failed Append = %v, want the fault's EIO", c.err)
	}

	// B's write lands last: its record completed into the segment the
	// failure ended, and its end is the failed slot's own offset.
	holdB.Release()
	b := <-bDone
	if b.err != nil {
		t.Fatalf("the held Append: %v", b.err)
	}
	if b.p.Loc.Off >= d.p.Loc.Off {
		t.Fatalf("the slots came out ordered B %d, D %d: B must hold the lower slot", b.p.Loc.Off, d.p.Loc.Off)
	}
	return e, rec, id, b.p.Loc.Off + b.p.Loc.Len, aEnd
}

// TestEndTheEventIsHandedOverOnce checks R49 as epic 8 revises it: a failed
// append — the zero-slot choreography — hands its consumer exactly one end
// event, naming the stream, the ended segment, the append's cause, the failed
// slot, the failure itself and the synced-through the window reads from, and
// no stop. More appends and the stream's Close add no second event.
func TestEndTheEventIsHandedOverOnce(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec, id, cOff, aEnd := endZeroSlotWithConsumer(t)
	ev := rec.awaitOne(t, "the end of the failed append")

	if ev.Stream != 1 {
		t.Fatalf("the event names stream %d, want 1", ev.Stream)
	}
	if ev.Segment != id {
		t.Fatalf("the event names segment %d, want the ended one %d", ev.Segment, id)
	}
	if ev.Cause != segment.EndAppend {
		t.Fatalf("the event's cause is %s, want the append of %s", ev.Cause, segment.EndAppend)
	}
	if ev.Failed != cOff {
		t.Fatalf("the event names the failed slot %d, want C's own offset %d", ev.Failed, cOff)
	}
	if !errors.Is(ev.Err, syscall.EIO) {
		t.Fatalf("the event's error is %v, want the fault's EIO", ev.Err)
	}
	if ev.SyncedThrough != aEnd {
		t.Fatalf("the event's synced-through is %d, want A's synced value %d: the window the durability code reads is the records at or above it", ev.SyncedThrough, aEnd)
	}
	if ev.Stopped {
		t.Fatal("the event names the stream stopped, want it rolling")
	}

	// More appends and the stream's Close add no second event: the ended
	// segment's one end is the one word its consumer ever gets.
	nextPayload := streamTestBody(64, 0x3c)
	nh, nbody := streamWriteHeader(streamFileID(5), 0, nextPayload, 7, core.Version{Counter: 5})
	if _, err := e.s.Append(nh, nbody); err != nil {
		t.Fatalf("the append past the end: %v", err)
	}
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the Sync of the new segment: %v", err)
	}
	endEventsStable(t, rec, "the append past the end")
	if err := e.s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	endEventsStable(t, rec, "the Close")
}

// TestEndTheEventWaitsForAnEarlierSlotStillWriting checks the wait the
// hand-over makes: the failure's end holds its event until every slot the
// segment reserved has completed, the earlier-held B's among them. While B
// is still held at the seam no event arrives, the failed append has already
// returned, and another file's append on a second stream of the same Streams
// completes — the wait parks on the hand-over's own cond, off every appender's
// locks. Releasing B hands the event, and B has returned its location by
// then.
func TestEndTheEventWaitsForAnEarlierSlotStillWriting(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// Two streams share the seam, so the check can prove one stream's held
	// write never blocks another's append: the second stream takes a spare
	// outside the pinned maximum — build over the open rollMax, whose
	// worker keeps the class ready, so the other stream's append needs no
	// hand-holding to land.
	e := rollBuild(t, streamCap, segment.Targets{Content: 2}, nil)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 2 })
	ss := segment.NewStreams(2, streamIdentity, e.set, e.pool, e.cache, nopSinkFor)
	t.Cleanup(func() {
		if err := ss.Close(); err != nil {
			t.Errorf("the streams' Close: %v", err)
		}
	})
	sa, sb := ss.Stream(0), ss.Stream(1)
	rec := &endRecorder{}
	sa.OnEnd(rec.OnEnd)

	// Record A lands and is synced on the first stream, so its end
	// event's window has a value the checks read back; the segment it
	// takes is the one the holds below work on.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	se := streamEnv{seam: e.seam, set: e.set, maker: e.maker, cache: e.cache, budget: e.budget, s: sa, cap: streamCap}
	aPlaced := streamAppendWrite(t, se, ah, abody)
	if err := sa.Sync(); err != nil {
		t.Fatalf("the first Sync: %v", err)
	}
	seg := streamSegName(aPlaced.Loc.Segment)

	// B parks at the seam; C fails. The fault is armed once B's write is
	// parked and C's is set loose, so the rule's one round fires on C's
	// consult alone.
	holdB := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	bDone := endAppend(se, 2, 64, 0x55)
	<-holdB.Reached()
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	cDone := endAppend(se, 3, 128, 0x7d)
	c := <-cDone
	if !errors.Is(c.err, syscall.EIO) {
		t.Fatalf("the failed Append = %v, want the fault's EIO", c.err)
	}

	// While B's write is still held, no event arrives; the failed append
	// has returned; and the second stream completes an append of its own,
	// for the hand-over's wait holds no appender's lock.
	rec.awaitNone(t, "the event while an earlier slot still writes")
	otherDone := make(chan error, 1)
	go func() {
		payload := streamTestBody(64, 0x99)
		h, body := streamWriteHeader(streamFileID(6), 512, payload, 7, core.Version{Counter: 6})
		_, err := sb.Append(h, body)
		otherDone <- err
	}()
	select {
	case err := <-otherDone:
		if err != nil {
			t.Fatalf("the append of another file's stream during the hold: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second stream's append never returned while the first stream's end waited on a held write: the hand-over's wait holds an appender's lock")
	}

	// Releasing B hands the event over — and B has returned before it, for
	// its completion is what the wait was waiting for.
	holdB.Release()
	ev := rec.awaitOne(t, "the end after the earlier slot completed")
	if ev.Cause != segment.EndAppend {
		t.Fatalf("the event's cause is %s, want the append of %s", ev.Cause, segment.EndAppend)
	}
	b := <-bDone
	if b.err != nil {
		t.Fatalf("the held Append: %v", b.err)
	}
	if ev.Failed != b.p.Loc.Off+b.p.Loc.Len {
		t.Fatalf("the event names the failed slot %d, want the held record's end %d", ev.Failed, b.p.Loc.Off+b.p.Loc.Len)
	}
}

// TestEndTheEventWaitsForALaterSlotToo checks decision 2: the hand-over's
// wait covers the slots reserved after the failed one as well as those before
// it. D — reserved above C, its write still parked at the seam when C's fails
// — holds the event until it lands, so the record a later append completed is
// never lost from the window. A notifier that waited only for the slots
// below the failure would hand the event over while D still writes, and the
// second no-event check below catches it.
func TestEndTheEventWaitsForALaterSlotToo(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	rec := &endRecorder{}
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })
	e.s.OnEnd(rec.OnEnd)

	// A lands and is synced, so the end event's window has a value.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	streamAppendWrite(t, e, ah, abody)
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the first Sync: %v", err)
	}

	// The three one-shot holds take the writes in their reservation order —
	// B parks at the first, C at the second, D at the third — so every slot
	// is reserved, none has completed, and D's reservation stands above
	// the failed one's while its own write is still parked.
	holdB := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	bDone := endAppend(e, 2, 64, 0x55)
	<-holdB.Reached()
	holdC := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	cDone := endAppend(e, 3, 128, 0x7d)
	<-holdC.Reached()
	holdD := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	dDone := endAppend(e, 4, 192, 0x13)
	<-holdD.Reached()

	// The fault is armed while every write is parked: each consults it only
	// at its hold's release, so the rule's one round fires on the first
	// released write — C's — and leaves B's and D's to pass.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	holdC.Release()
	c := <-cDone
	if !errors.Is(c.err, syscall.EIO) {
		t.Fatalf("the failed Append = %v, want the fault's EIO", c.err)
	}

	// While B — a slot reserved before the failed one — is still held, no
	// event arrives.
	rec.awaitNone(t, "the event while the lower slot still writes")

	// B lands into the segment its fellow append's failure ended, and still
	// no event arrives: D, reserved above the failed slot, is still
	// writing, and the wait covers it too.
	holdB.Release()
	b := <-bDone
	if b.err != nil {
		t.Fatalf("the held Append: %v", b.err)
	}
	rec.awaitNone(t, "the event while the later slot still writes")

	// D lands, the last reservation the segment held completes, and the
	// event hands over at once.
	holdD.Release()
	d := <-dDone
	if d.err != nil {
		t.Fatalf("the later held Append: %v", d.err)
	}
	ev := rec.awaitOne(t, "the end after the later slot completed")
	if ev.Cause != segment.EndAppend {
		t.Fatalf("the event's cause is %s, want the append of %s", ev.Cause, segment.EndAppend)
	}
	cOff := b.p.Loc.Off + b.p.Loc.Len
	if ev.Failed != cOff {
		t.Fatalf("the event names the failed slot %d, want C's own offset %d", ev.Failed, cOff)
	}
	if b.p.Loc.Off >= cOff || cOff >= d.p.Loc.Off {
		t.Fatalf("the slots came out ordered B %d, C %d, D %d: B must hold the slot below the failed one and D the slot above it", b.p.Loc.Off, cOff, d.p.Loc.Off)
	}
}

// TestEndASealEndedSegmentSendsOneEvent checks the seal's ends: a seal whose
// record sync fails ends its segment and hands one event naming the seal as
// its cause, and an explicit Seal of a segment a failed write already ended
// adds no second event — it is refused with ErrSegmentEnded and owns no end
// of its own.
func TestEndASealEndedSegmentSendsOneEvent(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The seal's first sync — the records' — fails on the segment the roll
	// seals, as in the roll checks: the straddling write triggers a roll
	// whose seal's record sync the fault takes.
	e := rollPin(t, 4096, segment.Targets{Content: 4}, nil).withStream(t, 1)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 2 })
	rec := &endRecorder{}
	e.s.OnEnd(rec.OnEnd)

	warm := make([]byte, 1024)
	wh, wb := streamWriteHeaderPair(streamFileID(1), 0, warm)
	p1 := streamAppendWrite(t, e.stream(t), wh, wb)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpSync, streamSegName(p1.Loc.Segment), 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	straddle := streamTestBody(3072, 0xa5)
	h, body := streamWriteHeader(streamFileID(2), 4096, straddle, 7, core.Version{Counter: 2})
	p2, err := e.s.AppendRoll(h, body, segment.Roll{})
	if err != nil {
		t.Fatalf("the straddling AppendRoll: %v", err)
	}
	if p2.N == 0 || p2.N >= int64(len(straddle)) {
		t.Fatalf("the straddling AppendRoll placed %d bytes, want a piece", p2.N)
	}

	// The append of the rest carries the seal's ended failure, and the one
	// event names the seal, not the seal's caller.
	rest := straddle[p2.N:]
	h2, b2 := streamWriteHeader(streamFileID(2), 4096+p2.N, rest, 7, core.Version{Counter: 2})
	if _, err := e.s.AppendRoll(h2, b2, segment.Roll{}); err == nil {
		t.Fatal("the AppendRoll of the rest answered nil, want the seal's ended failure carried to its caller")
	} else if !errors.Is(err, segment.ErrSegmentEnded) || !errors.Is(err, syscall.EIO) {
		t.Fatalf("the AppendRoll of the rest = %v, want ErrSegmentEnded carrying the fault's EIO", err)
	}
	ev := rec.awaitOne(t, "the end of the failed seal")
	if ev.Segment != p1.Loc.Segment {
		t.Fatalf("the event names segment %d, want the seal-ended one %d", ev.Segment, p1.Loc.Segment)
	}
	if ev.Cause != segment.EndSeal {
		t.Fatalf("the event's cause is %s, want the seal of %s", ev.Cause, segment.EndSeal)
	}
	if !errors.Is(ev.Err, syscall.EIO) {
		t.Fatalf("the event's error is %v, want the fault's EIO", ev.Err)
	}
	if !errors.Is(ev.Err, segment.ErrSegmentEnded) {
		t.Fatalf("the event's error is %v, want the seal's ended sentinel named", ev.Err)
	}

	// An explicit Seal of a segment a failed write already ended is refused
	// and owns no end of its own: the failed append's event covered the
	// segment, and no second one arrives for the seal.
	recSeal := &endRecorder{}
	seg := streamSegName(1)
	se := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = se.s.Close() })
	se.s.OnEnd(recSeal.OnEnd)
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	streamAppendWrite(t, se, ah, abody)
	if err := se.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, syscall.EIO)); err != nil {
		t.Fatalf("arming the write fault on the seal's env: %v", err)
	}
	bPayload := streamTestBody(64, 0x55)
	bh, bbody := streamWriteHeader(streamFileID(2), 64, bPayload, 7, core.Version{Counter: 2})
	if _, err := se.s.Append(bh, bbody); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the failed Append on the seal's env = %v, want the fault's EIO", err)
	}
	recSeal.awaitOne(t, "the write-ended segment's end")
	if _, err := se.s.Seal(false); !errors.Is(err, segment.ErrSegmentEnded) {
		t.Fatalf("Seal of the write-ended segment = %v, want ErrSegmentEnded", err)
	}
	endEventsStable(t, recSeal, "the Seal of an already-ended segment")
}

// TestEndRequestedAfterAFailedSync checks the durability code's own request:
// a failed Sync is answered by an End of the segment it failed on — cause
// handed, one event naming the requested end and the stream's next free slot
// as the end of the window — and the next append rolls to a segment naming
// no predecessor. A second End of the same segment returns nil with no event,
// an End of a sealed segment is refused, an End past the Close is refused,
// and an End with no cause panics.
func TestEndRequestedAfterAFailedSync(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	rec := &endRecorder{}
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })
	e.s.OnEnd(rec.OnEnd)

	// One record lands; the next Sync fails.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	placed := streamAppendWrite(t, e, ah, abody)
	next := placed.Loc.Off + placed.Loc.Len
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpSync, seg, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	syncErr := fmt.Errorf("the failed sync: %w", syscall.EIO)
	if err := e.s.Sync(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the faulted Sync = %v, want the fault's EIO", err)
	}

	// The durability code ends the segment itself. One event arrives,
	// naming the requested cause, the failure, and the stream's next free
	// slot as the end of every reservation the segment holds.
	if err := e.s.End(placed.Loc.Segment, syncErr); err != nil {
		t.Fatalf("End of the sync-failed segment = %v, want nil", err)
	}
	ev := rec.awaitOne(t, "the requested end")
	if ev.Segment != placed.Loc.Segment {
		t.Fatalf("the event names segment %d, want %d", ev.Segment, placed.Loc.Segment)
	}
	if ev.Cause != segment.EndRequested {
		t.Fatalf("the event's cause is %s, want the requested end", ev.Cause)
	}
	if ev.Failed != next {
		t.Fatalf("the event names the failed slot %d, want the stream's next free slot %d: no write failed, so the end is the end of every reservation the segment held", ev.Failed, next)
	}
	if !errors.Is(ev.Err, syscall.EIO) {
		t.Fatalf("the event's error is %v, want the caller's cause", ev.Err)
	}

	// The next append rolls to a segment naming no predecessor.
	if err := e.seam.Schedule(); err != nil {
		t.Fatalf("disarming the sync fault: %v", err)
	}
	cPayload := streamTestBody(64, 0x7d)
	ch, cbody := streamWriteHeader(streamFileID(3), 0, cPayload, 7, core.Version{Counter: 3})
	c := streamAppendWrite(t, e, ch, cbody)
	if c.Loc.Segment == placed.Loc.Segment {
		t.Fatalf("the append after the requested end stayed in segment %d, want a rolled-to new one", placed.Loc.Segment)
	}
	if hdr := endHeaderOf(t, e, c.Loc.Segment); hdr.Predecessor != 0 {
		t.Fatalf("the new segment names predecessor %d, want none: a requested end names no successor either", hdr.Predecessor)
	}

	// A second End of the same segment is a no-op nil, not a second event.
	if err := e.s.End(placed.Loc.Segment, syncErr); err != nil {
		t.Fatalf("the second End of the ended segment = %v, want nil", err)
	}
	endEventsStable(t, rec, "the second End")

	// An End of a sealed segment is refused, and sends nothing. The new
	// segment is sealed whole: a full Seal keeps its cap, and the segment
	// the stream now holds is rolled past first.
	dPayload := streamTestBody(64, 0x3c)
	dh, dbody := streamWriteHeader(streamFileID(4), 0, dPayload, 7, core.Version{Counter: 4})
	d := streamAppendWrite(t, e, dh, dbody)
	if _, err := e.s.Seal(false); err != nil {
		t.Fatalf("the Seal of the new segment: %v", err)
	}
	if err := e.s.End(d.Loc.Segment, syncErr); !errors.Is(err, segment.ErrWrongState) {
		t.Fatalf("End of the sealed segment = %v, want ErrWrongState", err)
	}
	endEventsStable(t, rec, "the End of a sealed segment")

	// An End past the stream's Close is refused.
	if err := e.s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := e.s.End(d.Loc.Segment, syncErr); !errors.Is(err, segment.ErrStreamClosed) {
		t.Fatalf("End past the Close = %v, want ErrStreamClosed", err)
	}

	// An End with no cause is a caller bug.
	closed := func() (panicked bool) {
		defer func() { panicked = recover() != nil }()
		_ = e.s.End(d.Loc.Segment, nil)
		return
	}
	if !closed() {
		t.Fatal("End with a nil cause returned, want a panic: a nil cause names no failure")
	}
}

// TestEndRequestedWaitsForAWriteInFlight checks the requested end's wait: End
// returns while a write is still held at the seam — the request is not held
// hostage to a write it never owned — and the event hands over only once that
// write completes.
func TestEndRequestedWaitsForAWriteInFlight(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	rec := &endRecorder{}
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })
	e.s.OnEnd(rec.OnEnd)

	// One record lands, so the stream holds its first segment; the Sync
	// after it fails, so the durability code may ask for the end.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	placed := streamAppendWrite(t, e, ah, abody)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpSync, seg, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if err := e.s.Sync(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the faulted Sync = %v, want the fault's EIO", err)
	}

	// B's write parks at the seam: its reservation is placed, its bytes are
	// not. The durability code asks for the end while B is still in
	// flight.
	holdB := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	bDone := endAppend(e, 2, 64, 0x55)
	<-holdB.Reached()
	endDone := make(chan error, 1)
	go func() {
		endDone <- e.s.End(placed.Loc.Segment, fmt.Errorf("the failed sync: %w", syscall.EIO))
	}()
	select {
	case err := <-endDone:
		if err != nil {
			t.Fatalf("End during the held write: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("End never returned while a write was held at the seam: the requested end waits on a write it never owned")
	}

	// The event waits for the held write's completion before it hands over.
	rec.awaitNone(t, "the requested end's event while the write still writes")
	holdB.Release()
	ev := rec.awaitOne(t, "the requested end after the held write completed")
	b := <-bDone
	if b.err != nil {
		t.Fatalf("the held Append: %v", b.err)
	}
	if ev.Cause != segment.EndRequested {
		t.Fatalf("the event's cause is %s, want the requested end", ev.Cause)
	}
	if ev.Failed != b.p.Loc.Off+b.p.Loc.Len {
		t.Fatalf("the event names the failed slot %d, want the next free slot past the held record %d", ev.Failed, b.p.Loc.Off+b.p.Loc.Len)
	}
}

// TestEndCloseWaitsForTheNotifier checks the join Close makes: the consumer
// runs on the notifier's own goroutine, and a Close that races a consumer
// still running its event does not return until the consumer has.
func TestEndCloseWaitsForTheNotifier(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })

	// The consumer blocks on a channel until the test lets it pass; it is
	// registered before anything appends, as OnEnd's gate demands.
	blocked := make(chan struct{})
	entered := make(chan struct{})
	e.s.OnEnd(func(ev segment.EndEvent) {
		close(entered)
		<-blocked
	})

	// One record lands; the next write fails, ending the segment and
	// starting a notifier whose consumer blocks on the channel.
	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	streamAppendWrite(t, e, ah, abody)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	bPayload := streamTestBody(64, 0x55)
	bh, bbody := streamWriteHeader(streamFileID(2), 64, bPayload, 7, core.Version{Counter: 2})
	if _, err := e.s.Append(bh, bbody); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the failed Append = %v, want the fault's EIO", err)
	}

	// The consumer has its event and parks on the channel. Close must not
	// return until it has.
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the consumer never received the end event")
	}
	closed := make(chan error, 1)
	go func() { closed <- e.s.Close() }()
	for i := range endAttempts {
		select {
		case err := <-closed:
			t.Fatalf("Close returned %v while the consumer still ran", err)
		default:
		}
		if i%64 == 0 {
			runtime.Gosched()
		}
	}
	close(blocked)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close after the consumer passed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close never returned after the consumer was let pass")
	}
}

// TestEndOnEndAfterTheFirstAppendPanics checks OnEnd's own gate: the consumer
// is registered before the first append, and a later call — after a
// reservation, a roll or an end — panics rather than racing an event already
// in flight.
func TestEndOnEndAfterTheFirstAppendPanics(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })

	// Before the first append, registering is free — twice over, for the
	// caller that changes its mind.
	e.s.OnEnd(nil)
	e.s.OnEnd(func(segment.EndEvent) {})

	aPayload := streamTestBody(64, 0x42)
	ah, abody := streamWriteHeader(streamFileID(1), 0, aPayload, 7, core.Version{Counter: 1})
	streamAppendWrite(t, e, ah, abody)

	// After the first reservation, registering a consumer is a bug: an
	// event already in flight would be handed to a consumer that saw none
	// of the segment's history.
	late := func() (panicked bool) {
		defer func() { panicked = recover() != nil }()
		e.s.OnEnd(func(segment.EndEvent) {})
		return
	}
	if !late() {
		t.Fatal("OnEnd after the first append returned, want a panic")
	}
}

// TestEndEveryEndedSegmentHandsOneEventUnderRace checks the one-end contract
// under failure: appenders over two streams, a fault that fails segment-file
// writes at random, and rolls in between; every event names a segment that is
// Ended in the ledger, no segment is named twice — the one end the stream
// owns over it — and the race detector reads clean.
//
// The env is built by hand rather than rollBuild for one difference: its
// descriptor budget is wide. A segment a failed write ended never syncs
// again — Sync answers ErrSegmentEnded — so its descriptor keeps the dirty
// pin until the process ends, and the fault's ended segments alone can pin
// more descriptors than the shared builder's 64.
func TestEndEveryEndedSegmentHandsOneEventUnderRace(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const raceCap = int64(4096)
	seam, err := journaltest.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	set := segment.NewSet(raceCap, nil)
	maker := segment.NewMaker(seam.FS(), set, streamMax, 0, nil)
	budget := core.NewDescriptorBudget(512)
	cache := segment.NewDescriptors(seam.FS(), format.SegmentName, budget)
	pool := segment.NewPool(maker, segment.Targets{Content: 4}, nil)
	t.Cleanup(func() { pool.Stop() })
	pool.Start()
	awaitCounters(t, "the content spares ready", pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 4 })
	ss := segment.NewStreams(2, streamIdentity, set, pool, cache, nopSinkFor)
	t.Cleanup(func() {
		if err := ss.Close(); err != nil {
			t.Errorf("the streams' Close: %v", err)
		}
		if err := cache.Shutdown(); err != nil {
			t.Errorf("the cache's Shutdown: %v", err)
		}
	})
	rec := &endRecorder{}
	ss.OnEnd(rec.OnEnd)

	// The fault: a quarter of every write of a segment file fails with EIO,
	// so the ends and the rolls interleave under the streams' own locks. The
	// maker's writes run under its spare names, which no *.seg rule matches,
	// so the fault leaves the spare making alone.
	if err := seam.Schedule(journaltest.FailingRandomly(journaltest.OpWrite, "*.seg", 0.25, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	const appenders = 8
	const perAppender = 24
	var failed atomic.Int64
	var wg sync.WaitGroup
	for i := range appenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f := streamFileID(i)
			st := ss.For(f)
			for j := range perAppender {
				payload := streamTestBody(64, byte(i*16+j))
				h, body := streamWriteHeader(f, int64(j)*64, payload, 7, core.Version{Counter: uint64(i*perAppender + j + 1)})
				for {
					_, err := st.AppendRoll(h, body, segment.Roll{})
					if err == nil {
						// A landed record syncs its segment here, so the
						// descriptor a clean roll leaves behind loses its
						// dirty pin and the budget stays wide for the
						// ended ones, which never sync again.
						if serr := st.Sync(); serr != nil && !errors.Is(serr, segment.ErrSegmentEnded) {
							t.Errorf("appender %d's sync: %v", i, serr)
							return
						}
						break
					}
					// Every failure but a spare drought ends a segment — the
					// plain EIO of a write the fault refused, the ended-seal word
					// of a roll whose seal found one, the stream's own stop — and
					// the events below must match them one for one, so each counts
					// and the appender moves on: no retry serves any of them, for a
					// stopped stream takes no further spare.
					if errors.Is(err, syscall.EIO) || errors.Is(err, segment.ErrSegmentEnded) || errors.Is(err, segment.ErrStreamStopped) {
						failed.Add(1)
						break
					}
					var ns *core.NoSpaceError
					if !errors.As(err, &ns) || ns.Limit != core.LimitSpare {
						t.Errorf("appender %d's append: %v", i, err)
						return
					}
					pool.Kick()
					ready := false
					for k := range poolAttempts {
						if k%4096 == 0 {
							pool.Kick()
						}
						runtime.Gosched()
						if c := pool.Counters(); c.Spares+c.HeadroomBytes/raceCap+c.RepackReserveBytes/raceCap > 0 {
							ready = true
							break
						}
					}
					if !ready {
						t.Errorf("appender %d's roll found no spare again: %v", i, err)
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()

	// The streams close before any event is read, so every notifier the run
	// started is joined and its event handed over: reading them earlier
	// races the last ends' own hand-overs against the checks below, and the
	// join is what the closed do — the streams never serve again.
	if err := ss.Close(); err != nil {
		t.Fatalf("the streams' Close after the run: %v", err)
	}

	// The fault must have fired for the run to say anything: the count the
	// appenders kept bounds the check below, and a schedule that failed on
	// nothing is the run's own bug, not a pass.
	if failures := failed.Load(); failures == 0 {
		t.Fatal("the fault failed no write: the run proved nothing about the ends")
	}
	// Every event names a segment that is Ended in the ledger, and no
	// segment is named twice: the one end a stream owns over it.
	evs := rec.events()
	if len(evs) == 0 {
		t.Fatal("no end event arrived over the failures the appenders saw: the hand-over dropped its events")
	}
	seen := map[core.SegmentID]int{}
	for _, ev := range evs {
		seen[ev.Segment]++
		state, err := set.State(ev.Segment)
		if err != nil {
			t.Fatalf("the state of the event's segment %d: %v", ev.Segment, err)
		}
		if state != segment.Ended {
			t.Fatalf("segment %d, named by an event, stands %v, want Ended", ev.Segment, state)
		}
		if int(ev.Stream) >= ss.Len() {
			t.Fatalf("an event names stream %d, above the streams the run built", ev.Stream)
		}
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("segment %d handed %d events, want the one end a stream owns over each ended segment", id, n)
		}
	}

	// Every EIO the appenders saw ended a segment, and each ended segment
	// hands exactly one event, so the events never outnumber the failures
	// the run itself counted.
	if failures := failed.Load(); int64(len(evs)) > failures {
		t.Fatalf("%d events arrived over %d failures, want the events to name only ends the appends made", len(evs), failures)
	}

	// The other direction holds too: every Ended segment the run left behind
	// is named by an event, so a notifier that dropped some ends cannot pass
	// as one that handed them over. The take-unwind segments — a spare the
	// roll's successor-header write failed on, ended with no event by its own
	// exemption — are excluded the one way the disk reads them back: their
	// segment header was never written, so the whole file below the cap's
	// trailer reads as zeros, while every segment an append landed in starts
	// with its 48-byte header written.
	//
	// The maker's spare creation is the same zero-filled file, but the
	// run's spares never left the pool's ready set — the takes drew them
	// into the ledger — and the ledger's Ended memberships alone are the
	// candidates below.
	for id := range endStressEndedSegments(t, set, seam, raceCap) {
		if seen[id] != 1 {
			t.Fatalf("Ended segment %d handed %d events, want the one end it owes: a notifier dropped it", id, seen[id])
		}
	}
}

// endStressEndedSegments lists every Ended segment of the set that an append
// reached — a written header at offset 0 tells it from a spare the take's
// unwind ended before any record was reserved there.
func endStressEndedSegments(t *testing.T, set *segment.Set, seam *journaltest.Seam, cap int64) map[core.SegmentID]struct{} {
	t.Helper()

	// The set names no enumerator of its own, so the ids walked here are
	// bounded by the run's own footprint: a make of the open maximum drew
	// ids up from 1, and every segment the run ended sits below the takes
	// the run made. Walk ids until the ledger answers ErrNotInSet for a
	// run of spans wide enough to have passed the last one.
	ended := map[core.SegmentID]struct{}{}
	const span = 32
	for id := core.SegmentID(1); ; id++ {
		if _, err := set.State(id); err != nil {
			if int64(id) > span {
				break
			}
			continue
		}
		st, err := set.State(id)
		if err != nil || st != segment.Ended {
			continue
		}
		// A header write reaching the file tells an append's segment from a
		// take's unwind: the maker creates a spare zeroed whole, and the
		// roll writes the header before anything else, so a non-zero byte
		// in the first 48 names a segment the stream reached.
		raw := make([]byte, 4)
		f, ferr := seam.FS().Open(streamSegName(id))
		if ferr != nil {
			t.Fatalf("opening Ended segment %d's file: %v", id, ferr)
		}
		_, _ = f.ReadAt(raw, 0)
		if err := f.Close(); err != nil {
			t.Fatalf("closing Ended segment %d's file: %v", id, err)
		}
		if endCountNonZero(raw) > 0 {
			ended[id] = struct{}{}
		}
	}
	return ended
}

// TestEndTheEventWaitsForASyncInFlightOnTheEndedSegment checks the event's
// window against a Sync that straddles the failure: the sync took its prefix
// before the append failed and its fsync is still running, so the window the
// event names is read only after the sync returns. An event handed over at
// the failure would name a SyncedThrough below what the returned Sync
// covered, and a consumer failing "the window's records" would drop records
// a nil-returning Sync already made durable (RFC 1 §5.3).
func TestEndTheEventWaitsForASyncInFlightOnTheEndedSegment(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	se := endStopStreamEnv(e)
	seg1 := streamSegName(1)

	ah, ab := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	streamAppendWrite(t, se, ah, ab)
	a2h, a2b := streamWriteHeader(streamFileID(1), 64, streamTestBody(512, 0x43), 7, core.Version{Counter: 2})
	a2 := streamAppendWrite(t, se, a2h, a2b)
	covered := a2.Loc.Off + a2.Loc.Len

	holdS := e.seam.HoldFirst(t, journaltest.OpSync, seg1)
	syncDone := make(chan error, 1)
	go func() { syncDone <- e.s.Sync() }()
	<-holdS.Reached()

	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg1, 1, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	if c := <-endAppend(se, 3, 1024, 0x7d); !errors.Is(c.err, syscall.EIO) {
		t.Fatalf("C's append = %v, want the fault's EIO", c.err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}

	// The sync is still in flight: its window is not final, so no event.
	rec.awaitNone(t, "the event while a sync of the ended segment is in flight")

	holdS.Release()
	if err := <-syncDone; err != nil {
		t.Fatalf("the straddling sync = %v, want nil", err)
	}
	ev := rec.awaitOne(t, "the event after the straddling sync returned")
	if ev.Segment != 1 {
		t.Fatalf("the event names segment %d, want 1", ev.Segment)
	}
	if ev.SyncedThrough < covered {
		t.Fatalf("the event's SyncedThrough = %d, below %d the returned Sync covered: the window would include records a successful Sync made durable", ev.SyncedThrough, covered)
	}
}

// TestEndALedgerRefusalToAnEndStillStartsTheNotifier checks the end's
// notifier against a ledger that refuses the end itself: the refusal returns
// to its caller, but the end's in-flight count is spent by a notifier of its
// own, so neither a Close nor a later roll hangs on it.
func TestEndALedgerRefusalToAnEndStillStartsTheNotifier(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := rollBuild(t, streamCap, segment.Targets{Content: 2}, nil)
	awaitCounters(t, "the content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 2 })
	e = e.withStream(t, 1)
	se := endStopStreamEnv(e)
	ah, ab := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	a := streamAppendWrite(t, se, ah, ab)

	// The ledger ends the stream's active segment first, so the end below —
	// the durability code's own request — meets a refusal on its set call
	// while the stream's state has taken the end.
	if err := e.set.End(a.Loc.Segment); err != nil {
		t.Fatalf("ending the segment in the ledger by hand: %v", err)
	}
	if err := e.s.End(a.Loc.Segment, syscall.EIO); err == nil {
		t.Fatal("End of the ledger-ended segment answered nil, want the ledger's refusal")
	}

	// The refusal was answered, but the end counted a notifier the stream owes
	// Close: a close that follows must return, not wait forever on the
	// in-flight count the refused path would have leaked.
	done := make(chan error, 1)
	go func() { done <- e.s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close past the refused end = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close never returned past the refused end: the refusal leaked the end's in-flight count")
	}
}

// TestEndALedgerRefusalToASealsEndStillStartsTheNotifier checks the same leak
// through the seal's freeze branch: a seal that finds a failed write ends its
// segment, and a ledger that refuses the end returns its refusal with the
// notifier started anyway, so a Close is not left hanging on it.
func TestEndALedgerRefusalToASealsEndStillStartsTheNotifier(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// A set that refuses set.End: a segment of the journal never added, so
	// the seal's freeze branch ends a segment the ledger does not hold.
	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })

	// One record lands; the segment's Sync fails, so the stream holds a
	// syncFailed word while the segment stands open — the state the seal's
	// freeze branch ends it from. The ledger is ended by hand first, so the
	// seal's own set.End meets a refusal: the freeze branch's caller.
	ah, ab := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	a := streamAppendWrite(t, e, ah, ab)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpSync, seg, 1, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Sync(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the faulted Sync = %v, want the fault's EIO", err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}
	// The ledger ends the segment by hand, so the seal's end below is the
	// one the ledger refuses.
	if err := e.set.End(a.Loc.Segment); err != nil {
		t.Fatalf("ending the segment in the ledger by hand: %v", err)
	}
	if res, err := e.s.Seal(false); err == nil {
		t.Fatalf("the Seal of the ledger-ended segment answered nil, want a refusal (res %+v)", res)
	} else if !errors.Is(err, segment.ErrSegmentEnded) {
		t.Fatalf("the Seal of the ledger-ended segment = %v, want the ended sentinel joined in refusal", err)
	}

	done := make(chan error, 1)
	go func() { done <- e.s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close past the refused seal = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close never returned past the refused seal: the refusal leaked the end's in-flight count")
	}
}

// TestEndTheRollDoesNotInstallBeforeTheEventsSnapshot checks the roll's
// install wait against the notifier itself: an end's event must be handed
// over — its snapshot taken — before the roll installs the successor, for a
// roll that woke past the notifier's last-completion broadcast would have
// the snapshot read the successor's own reset failedAt and blank tracker,
// and the event would name neither the failed slot nor the window it owes
// the durability code. The notify park holds the notifier at its top while
// the completion that would wake it has already landed.
func TestEndTheRollDoesNotInstallBeforeTheEventsSnapshot(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	// The hook is cleared once the stream is closed — registered first, it
	// runs last — for a notifier still starting reads it until Close has
	// waited for it.
	t.Cleanup(func() { segment.StreamSetNotifyParkHook(nil) })
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })

	// The consumer parks the event itself, so the check sees the snapshot's
	// word alone: the event keeps its hands off the stream's state, and the
	// park below needs no held lock to hold the notifier back.
	eventIn := make(chan segment.EndEvent, 1)
	e.s.OnEnd(func(ev segment.EndEvent) { eventIn <- ev })

	// A lands and is synced; B's write parks at the seam; the notifier is
	// parked at its own top BEFORE anything can end the segment, so the end
	// C's failure makes is handed over by a notifier the check holds.
	ah, ab := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	a := streamAppendWrite(t, e, ah, ab)
	if err := e.s.Sync(); err != nil {
		t.Fatalf("the first Sync: %v", err)
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseNotifier := func() { releaseOnce.Do(func() { close(release) }) }
	segment.StreamSetNotifyParkHook(func() { <-release })
	// Registered after the stream's Close, so it runs before it: a check that
	// fails while the notifier is parked must free it, or Close waits on it
	// and the package hangs to its timeout.
	t.Cleanup(releaseNotifier)
	holdB := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	bDone := endAppend(e, 2, 64, 0x55)
	<-holdB.Reached()

	// C's write fails, ending the segment; the notifier the end starts is
	// parked at its top by the hook, for it was armed before the end.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 1, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	cDone := endAppend(e, 3, 128, 0x7d)
	if c := <-cDone; !errors.Is(c.err, syscall.EIO) {
		t.Fatalf("the failed Append = %v, want the fault's EIO", c.err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}

	// B's write lands — the last reservation the segment held — and D rolls.
	// The roll reaches the install wait and parks there, for the notifier
	// whose hand-over it waits on is itself parked at its own top: neither
	// the event nor the successor may move while the park holds.
	holdB.Release()
	if b := <-bDone; b.err != nil {
		t.Fatalf("the held Append: %v", b.err)
	}
	dDone := endAppend(e, 4, 0, 0x11)
	deadline := time.Now().Add(10 * time.Second)
	for e.seam.CallsFor(journaltest.OpWrite, streamSegName(2)) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the roll never wrote the successor's header")
		}
		time.Sleep(time.Millisecond)
	}

	// Over a bounded wait, neither the event nor D's append returns: the
	// roll's wait holds the successor off the notifier's hand-over, and the
	// parked notifier holds the event back. The take itself has already
	// drawn segment 2 into the ledger — the roll's own hold-off is proven
	// by D, whose append can only land through an install the park keeps
	// from happening.
	select {
	case ev := <-eventIn:
		t.Fatalf("the event arrived while the notifier was parked: %v", ev)
	case d := <-dDone:
		t.Fatalf("the append installed its successor before the end's record cleared: %v in segment %d", d.err, d.p.Loc.Segment)
	case <-time.After(200 * time.Millisecond):
	}

	// The notifier is let pass: the snapshot reads the ended segment's own
	// failed slot — C's, above A's synced window, below the segment's
	// reservations' end — and only then does the roll install and D's append
	// land. A snapshot after the install would name the successor's reset
	// failedAt (next, the successor's header end 48) and its blank tracker.
	releaseNotifier()
	ev := <-eventIn
	if ev.Segment != a.Loc.Segment {
		t.Fatalf("the event names segment %d, want the ended one %d", ev.Segment, a.Loc.Segment)
	}
	if ev.Failed <= a.Loc.Off+a.Loc.Len {
		t.Fatalf("the event's Failed %d is at or below A's end %d, want C's failed slot above it: the snapshot found the successor's reset failedAt instead", ev.Failed, a.Loc.Off+a.Loc.Len)
	}
	if ev.SyncedThrough == 0 {
		t.Fatalf("the event's SyncedThrough is 0, want A's synced window the segment held: the snapshot found the successor's blank tracker instead")
	}
	d := <-dDone
	if d.err != nil || d.p.Loc.Segment != 2 {
		t.Fatalf("the append past the installed successor = %v in segment %d, want a landing in 2", d.err, d.p.Loc.Segment)
	}
}

// TestEndTheEventsFailedIsTheLowestFailedSlot checks the min the completion
// keeps on failedAt: two failed writes in one segment, the higher completing
// first, and the event names the lower failed slot — the lowest offset whose
// write failed is the window's own lower bound, whichever refusal the
// scheduler answered first.
func TestEndTheEventsFailedIsTheLowestFailedSlot(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })
	rec := &endRecorder{}
	e.s.OnEnd(rec.OnEnd)

	ah, ab := streamWriteHeader(streamFileID(1), 0, streamTestBody(64, 0x42), 7, core.Version{Counter: 1})
	a := streamAppendWrite(t, e, ah, ab)

	// Two one-shot holds park both writes in their reservation order: B
	// first, C above it — its slot is B's end, the failed writes below both
	// assert against the segment's own offsets, read after they landed.
	holdB := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	bDone := endAppend(e, 2, 64, 0x55)
	<-holdB.Reached()
	holdC := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	cDone := endAppend(e, 3, 128, 0x7d)
	<-holdC.Reached()

	// Fail both writes: the count-2 rule spends its rounds on the two
	// released consults. B — the lower slot — is released first, so its
	// failure lands first and the higher one's completion is what the min
	// must not let overwrite it: an unconditional assignment would name C.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg, 2, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	holdB.Release()
	b := <-bDone
	if !errors.Is(b.err, syscall.EIO) {
		t.Fatalf("the lower failed Append = %v, want the fault's EIO", b.err)
	}
	// While C's slot is still parked, the segment's end has already been
	// made by B's completion — the event only hands over once C lands.
	holdC.Release()
	c := <-cDone
	if !errors.Is(c.err, syscall.EIO) {
		t.Fatalf("the higher failed Append = %v, want the fault's EIO", c.err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}

	// The failed appends answer with no place, so their slots are read from
	// the reservations the three same-shaped records took in order: A's own
	// end, then one record's width each — B's slot the lower of the two
	// failed ones, C's above it.
	slotLen := a.Loc.Len
	bSlot := a.Loc.Off + slotLen
	cSlot := bSlot + slotLen

	ev := rec.awaitOne(t, "the end of the doubly failed segment")
	if ev.Segment != a.Loc.Segment {
		t.Fatalf("the event names segment %d, want the ended one %d", ev.Segment, a.Loc.Segment)
	}
	if ev.Failed != bSlot {
		t.Fatalf("the event names the failed slot %d, want the lowest failed one %d: the min keeps the lower slot (%d..%d) however the completions ordered themselves", ev.Failed, bSlot, bSlot, cSlot)
	}
}

// TestEndTheEventsFailedCannotNameTheLeftSegment checks the failedAt reset at
// the roll's install: a requested end of the spare taken past an ended
// segment must name the spare's own failure, never the offset a write failed
// at in the segment the stream left behind.
func TestEndTheEventsFailedCannotNameTheLeftSegment(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	if got := endStopEndsSegment(t, e, rec, syscall.EIO); got != core.SegmentID(1) {
		t.Fatalf("the first ended segment is %d, want 1", got)
	}

	// A record lands in segment 2, then the durability code's request ends
	// it — a requested end with no failed write of its own.
	se := endStopStreamEnv(e)
	ah, ab := streamWriteHeader(streamFileID(3), 0, streamTestBody(64, 0x3c), 7, core.Version{Counter: 3})
	a := streamAppendWrite(t, se, ah, ab)
	if a.Loc.Segment != 2 {
		t.Fatalf("the record landed in segment %d, want the taken spare 2", a.Loc.Segment)
	}
	syncErr := fmt.Errorf("the failed sync: %w", syscall.EIO)
	if err := e.s.End(core.SegmentID(2), syncErr); err != nil {
		t.Fatalf("the requested End of the spare = %v, want nil", err)
	}
	evs := rec.awaitCount(t, 2, "the requested end of the spare")
	ev := evs[len(evs)-1]
	if ev.Segment != core.SegmentID(2) {
		t.Fatalf("the event names segment %d, want the spare 2", ev.Segment)
	}
	if ev.Cause != segment.EndRequested {
		t.Fatalf("the event's cause is %s, want the requested end", ev.Cause)
	}
	if ev.Failed < a.Loc.Off || ev.Failed > a.Loc.Off+a.Loc.Len {
		t.Fatalf("the event names the failed slot %d, want the spare's own end %d..%d: a roll must not leave the left segment's failedAt in the successor's event", ev.Failed, a.Loc.Off, a.Loc.Off+a.Loc.Len)
	}
}

// TestEndRegistrationPastARollAndAnEndPanics completes the OnEnd gate's own
// doc: after a reservation — the case its check already names — and after a
// roll and an end too, for each of the three marks the stream started and a
// consumer registered past any of them races an event already in flight.
func TestEndRegistrationPastARollAndAnEndPanics(t *testing.T) {
	prof.Watch(t, prof.Options{})

	callsOnEnd := func(t *testing.T, s *segment.Stream) (panicked bool) {
		t.Helper()
		defer func() { panicked = recover() != nil }()
		s.OnEnd(func(segment.EndEvent) {})
		return
	}

	// A plain fresh stream: registering is free.
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })
	if panicked := callsOnEnd(t, e.s); panicked {
		t.Fatal("OnEnd before anything panicked, want free registration")
	}

	// Past a roll the gate closes: the first append's roll is a roll the
	// stream made, and the event of the segment the roll's seal may end
	// would be handed to a consumer that saw none of the segment's history.
	er := rollPin(t, streamCap, segment.Targets{Reserve: 2}, nil).withStream(t, 1)
	awaitCounters(t, "the two reserve spares ready", er.pool,
		func(c segment.PoolCounters) bool { return c.RepackReserveBytes == 2*streamCap && c.Spares == 0 })
	payload := streamTestBody(64, 0x3c)
	hh, hb := streamWriteHeader(streamFileID(1), 0, payload, 7, core.Version{Counter: 1})
	if _, err := er.s.AppendRoll(hh, hb, segment.Roll{Repack: true}); err != nil {
		t.Fatalf("the repack AppendRoll over the roll env: %v", err)
	}
	if panicked := callsOnEnd(t, er.s); !panicked {
		t.Fatal("OnEnd past a roll returned, want a panic")
	}

	// Past a requested end the gate closes the same way, for an end may
	// arrive after any append and still owns its window's event.
	es, erec := endStopBuilder(t)
	eh, eb := endWrite(1, 0, 0x41)
	streamAppendWrite(t, endStopStreamEnv(es), eh, eb)
	if err := es.s.End(1, syscall.EIO); err != nil {
		t.Fatalf("End of the active segment = %v, want nil", err)
	}
	erec.awaitOne(t, "the requested end")
}

// TestEndOnAStreamWithNoSegmentEndsNothing checks that a request naming
// segment 0 on a stream that holds none is refused: no segment is ended, no
// event is handed over, and the stream is left exactly as it was — OnEnd
// still registers.
func TestEndOnAStreamWithNoSegmentEndsNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })
	rec := &endRecorder{}
	e.s.OnEnd(rec.OnEnd)
	if err := e.s.End(0, syscall.EIO); !errors.Is(err, segment.ErrNoSegment) {
		t.Fatalf("End(0) on a stream with no segment = %v, want ErrNoSegment", err)
	}
	rec.awaitNone(t, "an End of a segment that does not exist")
	registered := func() (panicked bool) {
		defer func() { panicked = recover() != nil }()
		e.s.OnEnd(func(segment.EndEvent) {})
		return
	}
	if registered() {
		t.Fatal("OnEnd panicked after a refused End, want the stream untouched")
	}
}

// TestEndACloseDuringTheSealWaitAnswersClosed checks the re-check End makes
// after its wait on a running seal: a Close that lands while the request is
// parked on the drain names the call's whole answer, so End answers
// ErrStreamClosed rather than the nil of a stream that no longer owns the
// segment. A's write is held at the seam, so the seal parks on the drain and
// End parks behind it; Close then runs, and A's release wakes both.
func TestEndACloseDuringTheSealWaitAnswersClosed(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seg := streamSegName(1)
	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	t.Cleanup(func() { _ = e.s.Close() })

	// One record lands first, so the stream holds its segment; A is the
	// second append, whose write parks at the seam with its slot reserved.
	h0, b0 := streamWriteHeader(streamFileID(9), 0, streamTestBody(64, 0x40), 7, core.Version{Counter: 1})
	streamAppendWrite(t, e, h0, b0)
	holdA := e.seam.HoldFirst(t, journaltest.OpWrite, seg)
	aDone := endAppend(e, 1, 64, 0x41)
	<-holdA.Reached()

	// The seal parks on the drain for A's reservation.
	sealDone := make(chan error, 1)
	go func() {
		_, err := e.s.Seal(false)
		sealDone <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	// parked reports whether a goroutine is blocked in the drain cond's wait
	// inside the named stream method.
	parked := func(method string) bool {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		for _, g := range bytes.Split(buf, []byte("\n\n")) {
			if bytes.Contains(g, []byte("(*Stream)."+method+"(")) && bytes.Contains(g, []byte("sync.runtime_notifyListWait")) {
				return true
			}
		}
		return false
	}
	for !parked("Seal") {
		if time.Now().After(deadline) {
			t.Fatal("the Seal never parked on the drain while A's write was held")
		}
		runtime.Gosched()
	}

	// End parks on the drain cond behind the seal: seen as a goroutine
	// blocked in the cond's wait inside Stream.End.
	endDone := make(chan error, 1)
	go func() { endDone <- e.s.End(1, fmt.Errorf("the failed sync: %w", syscall.EIO)) }()
	for !parked("End") {
		if time.Now().After(deadline) {
			t.Fatal("End never parked on the drain behind the seal")
		}
		runtime.Gosched()
	}

	// Close runs while both are parked; a Seal answering closed is the
	// observable that it has marked the stream closed.
	closeDone := make(chan error, 1)
	go func() { closeDone <- e.s.Close() }()
	for {
		if _, err := e.s.Seal(false); errors.Is(err, segment.ErrStreamClosed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close never marked the stream closed")
		}
		runtime.Gosched()
	}

	holdA.Release()
	if err := <-endDone; !errors.Is(err, segment.ErrStreamClosed) {
		t.Fatalf("End across a Close that landed during the seal wait = %v, want ErrStreamClosed", err)
	}
	// The seal's own result is not part of what this test claims: it is waited
	// out so no goroutine outlives the test, and logged for a failing run.
	t.Logf("the Seal that waited out the Close returned %v", <-sealDone)
	<-aDone
	if err := <-closeDone; err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
}

// endAwaitParked fails the test unless a goroutine is blocked in the drain
// cond's wait inside the named stream method within a bounded spin. The seal
// and the requests behind it park on that cond, which the test cannot hold
// at a seam, so the goroutine stacks are what shows them parked.
func endAwaitParked(t *testing.T, method, what string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		for _, g := range bytes.Split(buf, []byte("\n\n")) {
			if bytes.Contains(g, []byte("(*Stream)."+method+"(")) && bytes.Contains(g, []byte("sync.runtime_notifyListWait")) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: no goroutine parked in Stream.%s", what, method)
		}
		runtime.Gosched()
	}
}

// TestEndAnAppendFailingDuringTheSealsDrainLeavesTheEndToTheSeal checks RFC
// 28 §4.2's zero-slot row against a seal in flight: an append held at the
// seam while its stream seals the segment fails, and the segment is ended by
// the seal alone — one event, caused by the seal, naming the held slot — with
// no seal marker written and the stream continuing in a segment that names no
// predecessor. The append's own failure path must leave the end to the seal.
func TestEndAnAppendFailingDuringTheSealsDrainLeavesTheEndToTheSeal(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	se := endStopStreamEnv(e)
	seg1 := streamSegName(1)

	ah, ab := endWrite(1, 0, 0x41)
	a := streamAppendWrite(t, se, ah, ab)
	bOff := a.Loc.Off + a.Loc.Len
	holdB := e.seam.HoldFirst(t, journaltest.OpWrite, seg1)
	bDone := endAppend(se, 2, 64, 0x42)
	<-holdB.Reached()

	sealDone := make(chan error, 1)
	go func() {
		_, err := e.s.Seal(false)
		sealDone <- err
	}()
	endAwaitParked(t, "Seal", "the seal behind the held append")

	// B's write fails once released.
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, seg1, 1, syscall.EIO)); err != nil {
		t.Fatal(err)
	}
	holdB.Release()
	b := <-bDone
	if !errors.Is(b.err, syscall.EIO) {
		t.Fatalf("B's append = %v, want the fault's EIO", b.err)
	}
	if err := e.seam.Schedule(); err != nil {
		t.Fatal(err)
	}
	if err := <-sealDone; !errors.Is(err, segment.ErrSegmentEnded) {
		t.Fatalf("the seal = %v, want ErrSegmentEnded", err)
	}

	ev := rec.awaitOne(t, "the seal's end")
	if ev.Cause != segment.EndSeal {
		t.Fatalf("the event's cause is %v, want the seal's: the append's failure leaves the end to the seal that owns the segment", ev.Cause)
	}
	if ev.Failed != bOff {
		t.Fatalf("the event names failed offset %d, want the held slot's %d", ev.Failed, bOff)
	}
	endEventsStable(t, rec, "the seal's end")
	if state, err := e.set.State(1); err != nil || state != segment.Ended {
		t.Fatalf("segment 1 stands %v, %v, want Ended", state, err)
	}

	// No seal marker, and the stream continues in a segment naming none.
	nextRes := <-endAppend(se, 3, 0, 0x43)
	if nextRes.err != nil || nextRes.p.Loc.Segment != 2 {
		t.Fatalf("the append after the failure = %v in segment %d, want a landing in 2", nextRes.err, nextRes.p.Loc.Segment)
	}
	if hdr := endHeaderOf(t, se, 2); hdr.Predecessor != 0 {
		t.Fatalf("the new segment names predecessor %d, want none", hdr.Predecessor)
	}
}

// TestEndRequestWaitsOutARunningSeal checks that an End request arriving
// while a seal runs does not end the segment under it: the request parks until
// the seal settles, and a seal that sealed the segment makes it answer
// ErrWrongState with no event.
func TestEndRequestWaitsOutARunningSeal(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e, rec := endStopBuilder(t)
	se := endStopStreamEnv(e)
	seg1 := streamSegName(1)

	ah, ab := endWrite(1, 0, 0x41)
	streamAppendWrite(t, se, ah, ab)
	holdB := e.seam.HoldFirst(t, journaltest.OpWrite, seg1)
	bDone := endAppend(se, 2, 64, 0x42)
	<-holdB.Reached()

	sealDone := make(chan error, 1)
	go func() {
		_, err := e.s.Seal(false)
		sealDone <- err
	}()
	endAwaitParked(t, "Seal", "the seal behind the held append")

	endDone := make(chan error, 1)
	go func() { endDone <- e.s.End(1, fmt.Errorf("the failed sync: %w", syscall.EIO)) }()
	endAwaitParked(t, "End", "the request behind the running seal")

	holdB.Release()
	if b := <-bDone; b.err != nil {
		t.Fatalf("B's append = %v", b.err)
	}
	if err := <-sealDone; err != nil {
		t.Fatalf("the seal = %v, want nil", err)
	}
	if err := <-endDone; !errors.Is(err, segment.ErrWrongState) {
		t.Fatalf("End across a seal that sealed the segment = %v, want ErrWrongState", err)
	}
	rec.awaitNone(t, "an End request over a sealed segment")
}
