package durability_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
)

// TestLoseAppendsItsRecordBeforeTheEventIsPublished catches an event a poller
// can read while no loss record exists yet.
func TestLoseAppendsItsRecordBeforeTheEventIsPublished(t *testing.T) {
	e := newEnv(t)

	ran := false
	e.d.SetAfterAppend(func() {
		ran = true
		if got := e.d.Stats().Losses; len(got) != 0 {
			t.Errorf("an event was published before the record was appended: %+v", got)
		}
		if lh, _ := e.lossRecords(); len(lh) != 1 {
			t.Errorf("the record read back from the segment holds %d loss records, want 1", len(lh))
		}
	})

	if _, err := e.d.Lose(context.Background(), loss(10)); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	if !ran {
		t.Fatal("the after-append hook never ran")
	}
}

// TestLoseWritesTheDroppedRecordsOwnVersionAndFlags catches a loss keyed on a
// version range rather than on the dropped record.
func TestLoseWritesTheDroppedRecordsOwnVersionAndFlags(t *testing.T) {
	e := newEnv(t)

	l := loss(77)
	l.Synced = true
	l.Offloaded = true
	l.Reason = core.LossStale
	if _, err := e.d.Lose(context.Background(), l); err != nil {
		t.Fatalf("Lose: %v", err)
	}

	hs, bodies := e.lossRecords()
	if len(bodies) != 1 {
		t.Fatalf("%d loss records, want 1", len(bodies))
	}
	h, b := hs[0], bodies[0]
	if b.Dropped != 77 {
		t.Errorf("Dropped = %d, want 77", b.Dropped)
	}
	if !b.Synced || !b.Offloaded {
		t.Errorf("synced, offloaded = %v, %v, want true, true", b.Synced, b.Offloaded)
	}
	if b.Reason != 2 {
		t.Errorf("format reason = %d, want 2 (stale)", b.Reason)
	}
	if h.Version != l.Version {
		t.Errorf("header Version = %+v, want the dropped version %+v", h.Version, l.Version)
	}
	if h.File != l.File || h.Extent != l.Extent || h.Tag != l.Share {
		t.Errorf("header names file %v extent %+v tag %d, want %v %+v %d", h.File, h.Extent, h.Tag, l.File, l.Extent, l.Share)
	}
	if h.Seq == 0 || core.Seq(77) == h.Seq {
		t.Errorf("header Seq = %d, want a fresh sequence number, not the dropped one", h.Seq)
	}
}

// TestLoseMapsEachReasonToItsFormatReason catches a reason written under the
// wrong number, acknowledged in particular, whose record is reason 5.
func TestLoseMapsEachReasonToItsFormatReason(t *testing.T) {
	e := newEnv(t)

	want := map[core.LossReason]format.LossReason{
		core.LossCorrupt:      1,
		core.LossStale:        2,
		core.LossFailedWindow: 3,
		core.LossSuperseded:   format.LossSuperseded,
		core.LossAcknowledged: format.LossAcknowledged,
	}
	seq := core.Seq(1)
	for reason, fr := range want {
		l := loss(seq)
		l.Reason = reason
		if _, err := e.d.Lose(context.Background(), l); err != nil {
			t.Fatalf("Lose of reason %d: %v", reason, err)
		}
		_, bodies := e.lossRecords()
		if got := bodies[len(bodies)-1].Reason; got != fr {
			t.Errorf("reason %d wrote format reason %d, want %d", reason, got, fr)
		}
		seq++
	}
}

// TestLoseRaisesTheGenerationAndCountsByReasonAndOffloadedBit catches a raise
// on a stale or offloaded loss, and a counter bumped under the wrong reason.
func TestLoseRaisesTheGenerationAndCountsByReasonAndOffloadedBit(t *testing.T) {
	cases := []struct {
		name       string
		reason     core.LossReason
		offloaded  bool
		raises     uint64
		dataLoss   uint64
		repairable uint64
		stale      uint64
	}{
		{"corrupt dirty", core.LossCorrupt, false, 1, 1, 0, 0},
		{"corrupt offloaded", core.LossCorrupt, true, 0, 0, 1, 0},
		{"superseded dirty", core.LossSuperseded, false, 0, 0, 0, 0},
		{"superseded offloaded", core.LossSuperseded, true, 0, 0, 0, 0},
		{"acknowledged dirty", core.LossAcknowledged, false, 1, 0, 0, 0},
		{"acknowledged offloaded", core.LossAcknowledged, true, 0, 0, 0, 0},
		{"stale dirty", core.LossStale, false, 0, 0, 0, 1},
		{"stale offloaded", core.LossStale, true, 0, 0, 0, 1},
		{"failed window dirty", core.LossFailedWindow, false, 0, 0, 0, 0},
		{"failed window offloaded", core.LossFailedWindow, true, 0, 0, 0, 0},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			l := loss(core.Seq(i + 1))
			l.Reason = c.reason
			l.Offloaded = c.offloaded

			if _, err := e.d.Lose(context.Background(), l); err != nil {
				t.Fatalf("Lose: %v", err)
			}
			if got := e.d.Generation(); got != c.raises {
				t.Errorf("generation = %d, want %d", got, c.raises)
			}
			p := e.d.Stats()
			if p.LossGeneration != c.raises {
				t.Errorf("Stats().LossGeneration = %d, want %d", p.LossGeneration, c.raises)
			}
			if p.CorruptDataLoss != c.dataLoss || p.CorruptRepairable != c.repairable || p.StaleExtents != c.stale {
				t.Errorf("counters data loss %d, repairable %d, stale %d, want %d, %d, %d",
					p.CorruptDataLoss, p.CorruptRepairable, p.StaleExtents, c.dataLoss, c.repairable, c.stale)
			}
			if len(p.Losses) != 1 || p.Losses[0].Reason != c.reason || p.Losses[0].Offloaded != c.offloaded {
				t.Errorf("Stats().Losses = %+v, want one event of reason %d offloaded %v", p.Losses, c.reason, c.offloaded)
			}
		})
	}
}

// TestLoseOfAFailedWindowRaisesNothingBeyondTheWindowsOwnRaise catches a
// generation raised once per record of a window rather than once per window.
func TestLoseOfAFailedWindowRaisesNothingBeyondTheWindowsOwnRaise(t *testing.T) {
	e := newEnv(t)

	if got := e.d.RaiseWindow(); got != 1 {
		t.Fatalf("RaiseWindow = %d, want 1", got)
	}
	for i := range 50 {
		l := loss(core.Seq(i + 1))
		l.Reason = core.LossFailedWindow
		if _, err := e.d.Lose(context.Background(), l); err != nil {
			t.Fatalf("Lose %d: %v", i, err)
		}
	}
	if got := e.d.Generation(); got != 1 {
		t.Errorf("generation after one window of 50 records = %d, want 1", got)
	}
}

// TestLoseHasRaisedTheGenerationWhenItsEventIsFirstVisible catches a
// generation raised after the event is published, which would let a client
// read the event and still be handed a verifier it has seen.
func TestLoseHasRaisedTheGenerationWhenItsEventIsFirstVisible(t *testing.T) {
	e := newEnv(t)

	ran := false
	e.d.SetBeforePublish(func() {
		ran = true
		if got := len(e.d.Stats().Losses); got != 0 {
			t.Errorf("%d events visible before the publish", got)
		}
		if got := e.d.Generation(); got != 1 {
			t.Errorf("generation just before the event is visible = %d, want 1", got)
		}
		if got := e.d.Stats().CorruptDataLoss; got != 1 {
			t.Errorf("CorruptDataLoss just before the event is visible = %d, want 1", got)
		}
	})
	if _, err := e.d.Lose(context.Background(), loss(5)); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	if !ran {
		t.Fatal("the before-publish hook never ran")
	}
	if got := len(e.d.Stats().Losses); got != 1 {
		t.Errorf("%d events after Lose, want 1", got)
	}
}

// TestLoseReturnsTheEventItPublished catches a returned event that differs
// from the ring's.
func TestLoseReturnsTheEventItPublished(t *testing.T) {
	e := newEnv(t)

	l := loss(5)
	l.AtOpen = true
	ev, err := e.d.Lose(context.Background(), l)
	if err != nil {
		t.Fatalf("Lose: %v", err)
	}
	want := core.LossEvent{
		SeqNum: 1, Share: l.Share, File: l.File, Extent: l.Extent, Version: l.Version,
		Reason: core.LossCorrupt, AtOpen: true,
	}
	if ev != want {
		t.Errorf("returned event %+v, want %+v", ev, want)
	}
	if got := e.d.Stats().Losses; len(got) != 1 || got[0] != want {
		t.Errorf("ring = %+v, want [%+v]", got, want)
	}
}

// TestLoseKeepsTheNewestEventsWhenTheRingOverflows catches an unbounded ring
// and a gap a poller could not see.
func TestLoseKeepsTheNewestEventsWhenTheRingOverflows(t *testing.T) {
	e := newEnv(t)

	const total = core.LossRingSize + 5
	for i := range total {
		l := loss(core.Seq(i + 1))
		l.Reason = core.LossStale
		if _, err := e.d.Lose(context.Background(), l); err != nil {
			t.Fatalf("Lose %d: %v", i, err)
		}
	}
	got := e.d.Stats().Losses
	if len(got) != core.LossRingSize {
		t.Fatalf("the ring holds %d events, want %d", len(got), core.LossRingSize)
	}
	for i, ev := range got {
		if want := uint64(i + 6); ev.SeqNum != want {
			t.Fatalf("event %d has SeqNum %d, want %d: the oldest first, so a poller sees it missed 5", i, ev.SeqNum, want)
		}
	}
	if got[0].Version != loss(6).Version {
		t.Errorf("the oldest kept event is %+v, want the sixth loss", got[0])
	}
}

// TestLoseOfASeqAlreadyNamedDoesNothing catches a loss reported twice, by two
// callers that reach one dropped record, or once at open and once again.
func TestLoseOfASeqAlreadyNamedDoesNothing(t *testing.T) {
	t.Run("a second Lose", func(t *testing.T) {
		e := newEnv(t)
		l := loss(9)
		l.Synced = true
		if _, err := e.d.Lose(context.Background(), l); err != nil {
			t.Fatalf("first Lose: %v", err)
		}
		ev, err := e.d.Lose(context.Background(), l)
		if err != nil || ev != (core.LossEvent{}) {
			t.Fatalf("second Lose = %+v, %v, want the zero event and nil", ev, err)
		}
		assertOneLoss(t, e)
	})

	t.Run("after Place", func(t *testing.T) {
		e := newEnv(t)
		e.d.Place(core.LossEvent{Reason: core.LossCorrupt, Extent: loss(9).Extent}, 9)
		l := loss(9)
		l.Synced = true
		ev, err := e.d.Lose(context.Background(), l)
		if err != nil || ev != (core.LossEvent{}) {
			t.Fatalf("Lose = %+v, %v, want the zero event and nil", ev, err)
		}
		if got := len(e.d.Stats().Losses); got != 1 {
			t.Errorf("%d events, want only the placed one", got)
		}
		if got := e.d.Generation(); got != 0 {
			t.Errorf("generation = %d, want 0", got)
		}
		if lh, _ := e.lossRecords(); len(lh) != 0 {
			t.Errorf("%d loss records appended for a seq already named", len(lh))
		}
		if got := e.marked(); len(got) != 0 {
			t.Errorf("MarkLoss called %d times for a seq already named", len(got))
		}
	})
}

// assertOneLoss checks that exactly one loss record, one event, one raise and
// one mark exist.
func assertOneLoss(t *testing.T, e *env) {
	t.Helper()

	if lh, _ := e.lossRecords(); len(lh) != 1 {
		t.Errorf("%d loss records, want 1", len(lh))
	}
	if got := len(e.d.Stats().Losses); got != 1 {
		t.Errorf("%d events, want 1", got)
	}
	if got := e.d.Generation(); got != 1 {
		t.Errorf("generation = %d, want 1", got)
	}
	if got := len(e.marked()); got != 1 {
		t.Errorf("MarkLoss called %d times, want 1", got)
	}
}

// TestPlacePublishesAtOpenAndRaisesNothing catches a loss found at open that
// raises the generation again or counts as a drop this run made.
func TestPlacePublishesAtOpenAndRaisesNothing(t *testing.T) {
	e := newEnv(t)

	e.d.Place(core.LossEvent{File: file(2), Reason: core.LossCorrupt}, 3)
	e.d.Place(core.LossEvent{File: file(2), Reason: core.LossStale}, 4)

	p := e.d.Stats()
	if len(p.Losses) != 2 {
		t.Fatalf("%d events, want 2", len(p.Losses))
	}
	for i, ev := range p.Losses {
		if !ev.AtOpen {
			t.Errorf("event %d is not marked AtOpen", i)
		}
		if ev.SeqNum != uint64(i+1) {
			t.Errorf("event %d has SeqNum %d, want %d", i, ev.SeqNum, i+1)
		}
	}
	if p.LossGeneration != 0 || p.CorruptDataLoss != 0 || p.StaleExtents != 0 {
		t.Errorf("Place changed the generation or a counter: %+v", p)
	}
	if lh, _ := e.lossRecords(); len(lh) != 0 {
		t.Errorf("Place appended %d loss records", len(lh))
	}
}

// TestLoseMarksExactlyTheSyncedLosses catches a marker for a loss whose
// dropped record was never durable, and a synced loss that leaves none.
func TestLoseMarksExactlyTheSyncedLosses(t *testing.T) {
	e := newEnv(t)

	synced := loss(1)
	synced.Synced = true
	if _, err := e.d.Lose(context.Background(), synced); err != nil {
		t.Fatalf("Lose synced: %v", err)
	}
	if _, err := e.d.Lose(context.Background(), loss(2)); err != nil {
		t.Fatalf("Lose unsynced: %v", err)
	}

	got := e.marked()
	want := []marked{{synced.File, synced.Extent, synced.Version}}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("MarkLoss calls = %+v, want %+v", got, want)
	}
}

// TestLoseIsDurableThroughACrash catches a loss record left unsynced when
// Lose returns, which a crash before anyone read the loss would then forget.
func TestLoseIsDurableThroughACrash(t *testing.T) {
	e := newEnv(t)

	if _, err := e.d.Lose(context.Background(), loss(42)); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	// The synced bytes are read before anything closes the segment, whose
	// own close could sync what Lose left behind.
	if _, bodies := e.lossRecords(); len(bodies) != 1 {
		t.Fatalf("the durable loss records when Lose returned are %+v, want one", bodies)
	}
	e.shutdown()
	if err := e.seam.Crash(); err != nil {
		t.Fatalf("Crash: %v", err)
	}

	_, bodies := e.lossRecords()
	if len(bodies) != 1 || bodies[0].Dropped != 42 {
		t.Errorf("after a crash the durable loss records are %+v, want one dropping 42", bodies)
	}
}

// TestLoseWithACancelledContextChangesNothing catches a loss taken after its
// caller gave up.
func TestLoseWithACancelledContextChangesNothing(t *testing.T) {
	e := newEnv(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l := loss(1)
	l.Synced = true
	if _, err := e.d.Lose(ctx, l); err != context.Canceled {
		t.Fatalf("Lose = %v, want context.Canceled", err)
	}
	if lh, _ := e.lossRecords(); len(lh) != 0 {
		t.Errorf("%d loss records appended", len(lh))
	}
	if p := e.d.Stats(); len(p.Losses) != 0 || p.LossGeneration != 0 {
		t.Errorf("state changed: %+v", p)
	}
	if got := e.marked(); len(got) != 0 {
		t.Errorf("MarkLoss called %d times", len(got))
	}
	// The cancelled call named nothing: the same loss goes through now.
	if _, err := e.d.Lose(context.Background(), l); err != nil {
		t.Fatalf("Lose after a cancelled one: %v", err)
	}
	assertOneLoss(t, e)
}

// TestStatsReturnsACopyOfTheRing catches a Stats whose slice aliases the ring.
func TestStatsReturnsACopyOfTheRing(t *testing.T) {
	e := newEnv(t)

	if _, err := e.d.Lose(context.Background(), loss(1)); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	p := e.d.Stats()
	p.Losses[0].SeqNum = 999
	if got := e.d.Stats().Losses[0].SeqNum; got != 1 {
		t.Errorf("the ring's event changed to SeqNum %d through a Stats copy", got)
	}
}

// assertDropStands checks what every failed append leaves: the caller's drop
// is kept, so the event is published, the generation is raised, the seq is
// on the directory's failed list and the synced loss is marked.
func assertDropStands(t *testing.T, e *env, ev core.LossEvent, l durability.Loss) {
	t.Helper()

	if ev.SeqNum != 1 || ev.File != l.File || ev.Extent != l.Extent || ev.Reason != l.Reason {
		t.Errorf("the event returned with the error is %+v, want the published loss of %+v", ev, l)
	}
	p := e.d.Stats()
	if len(p.Losses) != 1 || p.Losses[0] != ev {
		t.Errorf("the ring holds %+v, want the one event %+v", p.Losses, ev)
	}
	if p.LossGeneration != 1 {
		t.Errorf("generation = %d, want 1: a drop whose record failed still raises it", p.LossGeneration)
	}
	if got := e.d.Failed(); !slices.Equal(got, []core.Seq{l.Seq}) {
		t.Errorf("Failed() = %v, want [%d]", got, l.Seq)
	}
	if got := e.marked(); (len(got) == 1) != l.Synced {
		t.Errorf("MarkLoss called %d times for a loss with Synced=%v", len(got), l.Synced)
	}
	if lh, _ := e.lossRecords(); len(lh) != 0 {
		t.Errorf("%d loss records are durable, want none", len(lh))
	}
}

// TestLoseWithNoHeadroomKeepsTheDropAndReportsNoSpace catches a drop undone
// because its record could not be appended: the journal stops holding what
// it cannot produce, whatever the headroom.
func TestLoseWithNoHeadroomKeepsTheDropAndReportsNoSpace(t *testing.T) {
	e := newEnvPinned(t, segment.Targets{})

	l := loss(5)
	l.Synced = true
	ev, err := e.d.Lose(context.Background(), l)

	if !errors.Is(err, core.ErrNoSpace) {
		t.Fatalf("Lose = %v, want ErrNoSpace", err)
	}
	var ns *core.NoSpaceError
	if !errors.As(err, &ns) || ns.Limit != core.LimitHeadroom {
		t.Fatalf("Lose = %v, want a *core.NoSpaceError naming the headroom", err)
	}
	assertDropStands(t, e, ev, l)
}

// TestLoseAppendsFromTheHeadroomWhenContentIsExhausted catches a loss record
// refused while a headroom spare stood ready.
func TestLoseAppendsFromTheHeadroomWhenContentIsExhausted(t *testing.T) {
	e := newEnvPinned(t, segment.Targets{Headroom: 1})
	if got := e.pool.Counters().HeadroomBytes; got != harnessCap {
		t.Fatalf("the pool holds %d headroom bytes, want one spare of %d", got, harnessCap)
	}

	ev, err := e.d.Lose(context.Background(), loss(5))
	if err != nil {
		t.Fatalf("Lose with a headroom spare ready: %v", err)
	}
	if lh, _ := e.lossRecords(); len(lh) != 1 {
		t.Errorf("%d loss records are durable, want 1", len(lh))
	}
	if got := e.pool.Counters().HeadroomBytes; got != 0 {
		t.Errorf("the pool still holds %d headroom bytes: the record was not appended from the headroom", got)
	}
	if got := e.d.Failed(); len(got) != 0 {
		t.Errorf("Failed() = %v, want empty after an appended record", got)
	}
	if ev.SeqNum != 1 {
		t.Errorf("event %+v, want number 1", ev)
	}
}

// errReadOnly is the sentinel the read-only check returns in tests.
var errReadOnly = errors.New("the journal is read only")

// TestLoseReadOnlyAppendsNothingAndKeepsTheDrop catches a loss record
// written through a read-only journal, and a drop undone by the refusal.
func TestLoseReadOnlyAppendsNothingAndKeepsTheDrop(t *testing.T) {
	e := newEnv(t)
	e.d.SetReadOnly(func() error { return errReadOnly })
	writes := e.seam.Calls(journaltest.OpWrite)

	l := loss(5)
	l.Synced = true
	ev, err := e.d.Lose(context.Background(), l)

	if !errors.Is(err, errReadOnly) {
		t.Fatalf("Lose = %v, want the read-only error", err)
	}
	if got := e.seam.Calls(journaltest.OpWrite); got != writes {
		t.Errorf("%d writes reached the journal, want none", got-writes)
	}
	assertDropStands(t, e, ev, l)
}

// TestLoseAFailedWriteKeepsTheDrop catches a drop undone, or an unsynced
// record trusted, when the device fails the record's write.
func TestLoseAFailedWriteKeepsTheDrop(t *testing.T) {
	e := newEnv(t)
	if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, "*.seg", 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	l := loss(5)
	l.Synced = true
	ev, err := e.d.Lose(context.Background(), l)

	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("Lose = %v, want EIO", err)
	}
	assertDropStands(t, e, ev, l)
}

// TestLoseDeferredAppendsNothingAndOwesNoFailedSeq catches a deferred loss
// reported as one whose record failed: the retiring copy owes the record.
func TestLoseDeferredAppendsNothingAndOwesNoFailedSeq(t *testing.T) {
	e := newEnv(t)
	writes := e.seam.Calls(journaltest.OpWrite)

	l := loss(5)
	l.Synced = true
	l.Deferred = true
	ev, err := e.d.Lose(context.Background(), l)
	if err != nil {
		t.Fatalf("Lose: %v", err)
	}

	if !ev.Deferred {
		t.Errorf("event %+v, want Deferred", ev)
	}
	if got := e.seam.Calls(journaltest.OpWrite); got != writes {
		t.Errorf("%d writes reached the journal, want none", got-writes)
	}
	if got := e.marked(); len(got) != 1 {
		t.Errorf("MarkLoss called %d times, want 1 for a synced deferred loss", len(got))
	}
	p := e.d.Stats()
	if p.LossGeneration != 1 || len(p.Losses) != 1 || !p.Losses[0].Deferred {
		t.Errorf("state %+v, want generation 1 and one deferred event", p)
	}
	if got := e.d.Failed(); len(got) != 0 {
		t.Errorf("Failed() = %v, want empty: the record is owed to the retiring copy", got)
	}
}

// TestLoseDeferredUnsyncedMarksNothing catches a marker left for a loss that
// was never synced.
func TestLoseDeferredUnsyncedMarksNothing(t *testing.T) {
	e := newEnv(t)

	l := loss(5)
	l.Deferred = true
	if _, err := e.d.Lose(context.Background(), l); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	if got := e.marked(); len(got) != 0 {
		t.Errorf("MarkLoss called %d times for an unsynced loss", len(got))
	}
}

// TestFailedIsSortedAndACopy catches a failed list returned in arrival order,
// or one the caller can write through.
func TestFailedIsSortedAndACopy(t *testing.T) {
	e := newEnv(t)
	e.d.SetReadOnly(func() error { return errReadOnly })

	for _, seq := range []core.Seq{9, 3, 6} {
		if _, err := e.d.Lose(context.Background(), loss(seq)); !errors.Is(err, errReadOnly) {
			t.Fatalf("Lose(%d) = %v", seq, err)
		}
	}
	got := e.d.Failed()
	if !slices.Equal(got, []core.Seq{3, 6, 9}) {
		t.Fatalf("Failed() = %v, want [3 6 9]", got)
	}
	got[0] = 99
	if again := e.d.Failed(); again[0] != 3 {
		t.Errorf("Failed() aliases its list: %v", again)
	}

	// The list is the directory's: a Durability built later on it sees it.
	if later := e.newDurability().Failed(); !slices.Equal(later, []core.Seq{3, 6, 9}) {
		t.Errorf("a later Durability on the directory sees %v, want [3 6 9]", later)
	}
}

// TestStatsNeverShowsAnEventBeyondItsGeneration catches a snapshot whose
// generation was read before a loss that its ring already holds: a poller
// pairing the two would record a generation from before the loss.
func TestStatsNeverShowsAnEventBeyondItsGeneration(t *testing.T) {
	e := newEnv(t)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	bad := 0
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				p := e.d.Stats()
				for _, ev := range p.Losses {
					if ev.SeqNum > p.LossGeneration {
						mu.Lock()
						bad++
						mu.Unlock()
						break
					}
				}
			}
		}()
	}
	for i := range 200 {
		if _, err := e.d.Lose(context.Background(), loss(core.Seq(100+i))); err != nil {
			t.Errorf("Lose: %v", err)
		}
	}
	close(stop)
	wg.Wait()
	if bad != 0 {
		t.Fatalf("%d snapshots held an event whose generation raise they did not include", bad)
	}
}

// TestLoseReturnsTheMarkersError catches a refused marker swallowed: the
// caller is the only one who can learn a synced loss left none. The loss
// itself still stands.
func TestLoseReturnsTheMarkersError(t *testing.T) {
	e := newEnv(t)
	sentinel := errors.New("marker refused")
	e.mu.Lock()
	e.markErr = sentinel
	e.mu.Unlock()

	l := loss(5)
	l.Synced = true
	ev, err := e.d.Lose(context.Background(), l)

	if !errors.Is(err, sentinel) {
		t.Fatalf("Lose = %v, want the marker's error", err)
	}
	if ev.SeqNum != 1 || ev.Reason != core.LossCorrupt {
		t.Errorf("event = %+v, want the published event", ev)
	}
	if got := e.d.Generation(); got != 1 {
		t.Errorf("generation = %d, want 1", got)
	}
	if got := e.d.Stats().CorruptDataLoss; got != 1 {
		t.Errorf("CorruptDataLoss = %d, want 1", got)
	}
	if lh, _ := e.lossRecords(); len(lh) != 1 {
		t.Errorf("%d loss records, want 1", len(lh))
	}
	if got := e.d.Failed(); len(got) != 0 {
		t.Errorf("Failed = %v, want none: the record was appended", got)
	}
}

// TestLoseReturnsTheAppendErrorBeforeTheMarkers catches the marker's error
// hiding the one that explains why no record exists.
func TestLoseReturnsTheAppendErrorBeforeTheMarkers(t *testing.T) {
	e := newEnv(t)
	e.mu.Lock()
	e.markErr = errors.New("marker refused")
	e.mu.Unlock()
	e.d.SetReadOnly(func() error { return errReadOnly })

	l := loss(5)
	l.Synced = true
	if _, err := e.d.Lose(context.Background(), l); !errors.Is(err, errReadOnly) {
		t.Fatalf("Lose = %v, want the append's error first", err)
	}
}

// TestPlaceOfASeqAlreadyNamedPublishesNothing catches one dropped record
// reported twice, which a failed sync that still reached the segment can
// cause at the next open.
func TestPlaceOfASeqAlreadyNamedPublishesNothing(t *testing.T) {
	t.Run("a second Place", func(t *testing.T) {
		e := newEnv(t)
		e.d.Place(core.LossEvent{Reason: core.LossCorrupt, Extent: loss(9).Extent}, 9)
		e.d.Place(core.LossEvent{Reason: core.LossCorrupt, Extent: loss(9).Extent}, 9)
		if got := len(e.d.Stats().Losses); got != 1 {
			t.Errorf("%d events for one dropped seq, want 1", got)
		}
	})
	t.Run("after Lose", func(t *testing.T) {
		e := newEnv(t)
		if _, err := e.d.Lose(context.Background(), loss(9)); err != nil {
			t.Fatalf("Lose: %v", err)
		}
		e.d.Place(core.LossEvent{Reason: core.LossCorrupt, Extent: loss(9).Extent}, 9)
		if got := len(e.d.Stats().Losses); got != 1 {
			t.Errorf("%d events for one dropped seq, want 1", got)
		}
	})
}

// TestFailedHoldsASeqOnceAcrossDurabilities catches a seq listed twice when
// two Durabilities on one directory both fail to append its record.
func TestFailedHoldsASeqOnceAcrossDurabilities(t *testing.T) {
	e := newEnv(t)
	other := e.newDurability()
	e.d.SetReadOnly(func() error { return errReadOnly })
	other.SetReadOnly(func() error { return errReadOnly })

	_, _ = e.d.Lose(context.Background(), loss(7))
	_, _ = other.Lose(context.Background(), loss(7))

	if got := e.d.Failed(); !slices.Equal(got, []core.Seq{7}) {
		t.Errorf("Failed = %v, want [7]", got)
	}
}

// TestLoseOfARecordWhoseSegmentWasEndedIsNotDurable catches a loss reported
// durable because the stream rolled past the segment holding its record
// before the sync: the sync then covers the new segment, not the record's.
func TestLoseOfARecordWhoseSegmentWasEndedIsNotDurable(t *testing.T) {
	e := newEnv(t)
	st := e.streams.For(file(1))
	e.d.SetBeforeSync(func() {
		if err := e.seam.Schedule(journaltest.Failing(journaltest.OpSync, "*.seg", 1, syscall.EIO)); err != nil {
			t.Errorf("Schedule: %v", err)
		}
		if _, err := st.Seal(false); err == nil {
			t.Errorf("the seal did not fail")
		}
		// Another appender rolls the stream past the ended segment.
		h := format.RecordHeader{Kind: format.KindLoss, File: file(1), Seq: 900, Tag: 7}
		if _, err := st.Append(h, format.RecordBody{Loss: format.Loss{Dropped: 900, Reason: format.LossStale}}); err != nil {
			t.Errorf("the rolling append: %v", err)
		}
	})

	l := loss(42)
	ev, err := e.d.Lose(context.Background(), l)

	if err == nil {
		t.Fatalf("Lose = nil, but its record's segment was ended and never made durable")
	}
	assertDropStands(t, e, ev, l)
}

// TestLoseOfAReasonThatNamesNoneIsRefusedOnEveryPath catches a deferred loss
// publishing an event with no reason, where the appending path panics.
func TestLoseOfAReasonThatNamesNoneIsRefusedOnEveryPath(t *testing.T) {
	e := newEnv(t)
	l := loss(3)
	l.Reason = 0
	l.Deferred = true

	defer func() {
		if recover() == nil {
			t.Errorf("a deferred loss with no reason did not panic like an appending one")
		}
		if n := len(e.d.Stats().Losses); n != 0 {
			t.Errorf("%d events published for a reasonless loss", n)
		}
	}()
	_, _ = e.d.Lose(context.Background(), l)
}

// levelRecorder is a slog.Handler that keeps the level of every record.
type levelRecorder struct {
	mu     *sync.Mutex
	levels *[]slog.Level
}

func (h levelRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h levelRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.levels = append(*h.levels, r.Level)
	return nil
}
func (h levelRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h levelRecorder) WithGroup(string) slog.Handler      { return h }

// TestLoseLogsAnExpectedRefusalAtDebugAndAnUnexpectedFailureAtError catches
// a refused record logged as an error, or a device fault logged as routine.
func TestLoseLogsAnExpectedRefusalAtDebugAndAnUnexpectedFailureAtError(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *env)
		want  slog.Level
	}{
		{"read-only", func(e *env) { e.d.SetReadOnly(func() error { return errors.New("read-only") }) }, slog.LevelDebug},
		{"write fault", func(e *env) {
			if err := e.seam.Schedule(journaltest.Failing(journaltest.OpWrite, "*.seg", 1, syscall.EIO)); err != nil {
				t.Fatalf("Schedule: %v", err)
			}
		}, slog.LevelError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var levels []slog.Level
			e := newEnv(t)
			e.log = slog.New(levelRecorder{&mu, &levels})
			e.d = e.newDurability()
			c.setup(e)

			_, _ = e.d.Lose(context.Background(), loss(5))

			mu.Lock()
			defer mu.Unlock()
			if len(levels) != 1 || levels[0] != c.want {
				t.Errorf("logged levels = %v, want exactly [%v]", levels, c.want)
			}
		})
	}
}

// piece returns loss(seq) over the extent [off, off+len) of the file.
func piece(seq core.Seq, off, n int64) durability.Loss {
	l := loss(seq)
	l.Extent = core.Extent{Off: off, Len: n}
	return l
}

// TestLoseOfTwoPiecesOfOneRecordReportsEachAndAppendsOneRecord catches a
// dedup keyed on the dropped seq alone, which loses the second piece's
// event, marker and raise.
func TestLoseOfTwoPiecesOfOneRecordReportsEachAndAppendsOneRecord(t *testing.T) {
	e := newEnv(t)
	for _, x := range [][2]int64{{0, 3}, {5, 3}} {
		l := piece(9, x[0], x[1])
		l.Synced = true
		ev, err := e.d.Lose(context.Background(), l)
		if err != nil || ev == (core.LossEvent{}) {
			t.Fatalf("Lose of [%d,+%d) = %+v, %v, want a published event", x[0], x[1], ev, err)
		}
	}
	if got := len(e.d.Stats().Losses); got != 2 {
		t.Errorf("%d events, want 2, one per piece", got)
	}
	if got := len(e.marked()); got != 2 {
		t.Errorf("MarkLoss called %d times, want 2", got)
	}
	if got := e.d.Generation(); got != 2 {
		t.Errorf("generation = %d, want 2", got)
	}
	if lh, _ := e.lossRecords(); len(lh) != 1 {
		t.Errorf("%d loss records, want 1: the first names the whole dropped record", len(lh))
	}
}

// TestLoseOfTwoSupersededPiecesAppendsARecordForEach catches a second piece
// of a record skipped as named: a superseded loss record excludes its record
// only over its own extent.
func TestLoseOfTwoSupersededPiecesAppendsARecordForEach(t *testing.T) {
	e := newEnv(t)
	for _, x := range [][2]int64{{0, 3}, {5, 3}} {
		l := piece(9, x[0], x[1])
		l.Reason = core.LossSuperseded
		if _, err := e.d.Lose(context.Background(), l); err != nil {
			t.Fatalf("Lose: %v", err)
		}
	}
	lh, _ := e.lossRecords()
	if len(lh) != 2 {
		t.Fatalf("%d loss records, want 2", len(lh))
	}
	if lh[0].Extent == lh[1].Extent {
		t.Errorf("both loss records name %+v", lh[0].Extent)
	}
}

// TestLoseOfASupersededPieceDoesNotNameTheWholeRecord catches a superseded
// loss that makes a later corrupt piece of the same record skip its record.
func TestLoseOfASupersededPieceDoesNotNameTheWholeRecord(t *testing.T) {
	e := newEnv(t)
	sup := piece(9, 0, 3)
	sup.Reason = core.LossSuperseded
	if _, err := e.d.Lose(context.Background(), sup); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	if _, err := e.d.Lose(context.Background(), piece(9, 5, 3)); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	if lh, _ := e.lossRecords(); len(lh) != 2 {
		t.Errorf("%d loss records, want 2", len(lh))
	}
}

// TestLoseOfASupersededPieceAfterACorruptOneAppendsItsOwnRecord catches a
// superseded piece skipped because a corrupt piece already named its seq: the
// corrupt record names the whole dropped record, but a superseded one
// excludes it only over its own extent, so the second piece still owes its
// own record. The reverse order (superseded first, corrupt second) appends
// two records too, which TestLoseOfASupersededPieceDoesNotNameTheWholeRecord
// pins: the superseded record never names the whole record.
func TestLoseOfASupersededPieceAfterACorruptOneAppendsItsOwnRecord(t *testing.T) {
	e := newEnv(t)
	if _, err := e.d.Lose(context.Background(), piece(9, 0, 3)); err != nil {
		t.Fatalf("Lose of the corrupt piece: %v", err)
	}
	sup := piece(9, 5, 3)
	sup.Reason = core.LossSuperseded
	if _, err := e.d.Lose(context.Background(), sup); err != nil {
		t.Fatalf("Lose of the superseded piece: %v", err)
	}
	lh, lb := e.lossRecords()
	if len(lh) != 2 {
		t.Fatalf("%d loss records, want 2", len(lh))
	}
	if lh[0].Extent != (core.Extent{Off: 0, Len: 3}) || lb[0].Reason != format.LossCorrupt {
		t.Errorf("first record = %+v %v, want the corrupt piece [0,+3)", lh[0].Extent, lb[0].Reason)
	}
	if lh[1].Extent != (core.Extent{Off: 5, Len: 3}) || lb[1].Reason != format.LossSuperseded {
		t.Errorf("second record = %+v %v, want the superseded piece [5,+3)", lh[1].Extent, lb[1].Reason)
	}
	for i, b := range lb {
		if b.Dropped != 9 {
			t.Errorf("record %d names dropped seq %d, want 9", i, b.Dropped)
		}
	}
}

// TestPlaceKeysOnTheDroppedSeqAndTheExtent catches a Place that reports only
// the first piece of a record dropped in two.
func TestPlaceKeysOnTheDroppedSeqAndTheExtent(t *testing.T) {
	e := newEnv(t)
	e.d.Place(core.LossEvent{Reason: core.LossCorrupt, Extent: core.Extent{Off: 0, Len: 3}}, 9)
	e.d.Place(core.LossEvent{Reason: core.LossCorrupt, Extent: core.Extent{Off: 5, Len: 3}}, 9)
	e.d.Place(core.LossEvent{Reason: core.LossCorrupt, Extent: core.Extent{Off: 5, Len: 3}}, 9)
	if got := len(e.d.Stats().Losses); got != 2 {
		t.Errorf("%d events, want 2", got)
	}
}

// TestLoseOfAPieceAfterAPlacedRecordAppendsNothing pins that a record on disk
// already names the seq, while the piece still publishes.
func TestLoseOfAPieceAfterAPlacedRecordAppendsNothing(t *testing.T) {
	e := newEnv(t)
	e.d.Place(core.LossEvent{Reason: core.LossCorrupt, Extent: core.Extent{Off: 0, Len: 3}}, 9)
	if _, err := e.d.Lose(context.Background(), piece(9, 5, 3)); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	if got := len(e.d.Stats().Losses); got != 2 {
		t.Errorf("%d events, want 2", got)
	}
	if lh, _ := e.lossRecords(); len(lh) != 0 {
		t.Errorf("%d loss records appended, want 0", len(lh))
	}
}

// TestLoseWithRaisedLeavesTheGenerationAlone catches a read-path loss that
// raises the generation a second time.
func TestLoseWithRaisedLeavesTheGenerationAlone(t *testing.T) {
	t.Run("raised", func(t *testing.T) {
		e := newEnv(t)
		l := loss(3)
		l.Raised = true
		if _, err := e.d.Lose(context.Background(), l); err != nil {
			t.Fatalf("Lose: %v", err)
		}
		p := e.d.Stats()
		if p.LossGeneration != 0 {
			t.Errorf("generation = %d, want 0: the caller raised it", p.LossGeneration)
		}
		if len(p.Losses) != 1 || p.CorruptDataLoss != 1 {
			t.Errorf("events %d, data loss %d, want 1, 1", len(p.Losses), p.CorruptDataLoss)
		}
	})
	t.Run("not raised", func(t *testing.T) {
		e := newEnv(t)
		if _, err := e.d.Lose(context.Background(), loss(3)); err != nil {
			t.Fatalf("Lose: %v", err)
		}
		if got := e.d.Generation(); got != 1 {
			t.Errorf("generation = %d, want 1", got)
		}
	})
}
