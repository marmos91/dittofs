package segment_test

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// TestEndSyncInFlightAcrossAFailureRollOwnsItsOwnSegment checks the stream's
// own Sync against the failure path's roll: a Sync of the ended segment that
// is in flight when the failure lands holds the roll back — the event's
// hand-over waits for it, and the roll waits for the event — so the
// successor starts blank: its tracker takes no window the old segment's fsync
// made durable (R10) and its second-refusal word stays blank until a sync of
// the successor itself succeeds.
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
// window through End.
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
	segment.StreamSetNotifyParkHook(func() { <-release })
	t.Cleanup(func() { segment.StreamSetNotifyParkHook(nil) })
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
	close(release)
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
	<-sealDone
	<-aDone
	<-closeDone
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
