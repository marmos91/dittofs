package durability_test

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
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
		core.LossAcknowledged: 5,
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
		e.d.Place(core.LossEvent{Reason: core.LossCorrupt}, 9)
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
