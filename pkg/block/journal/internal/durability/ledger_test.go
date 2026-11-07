package durability

import (
	"sync"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// TestTrackRunsUnderTheFileDomainWithoutDeadlock covers: the sink never
// takes the file append domain, since the appending goroutine holds it while
// Track runs and the worker's completion takes it. A completion run on the
// appender's goroutine would deadlock here.
func TestTrackRunsUnderTheFileDomainWithoutDeadlock(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f := file(1)

	unlock := r.domains.Lock(f)
	loc := r.appendWrite(t, f, 0, "held")
	if r.d.Durable(loc) {
		t.Fatal("a record no sync covered reads as durable")
	}
	r.syncStream(t, f)
	unlock()

	eventually(t, "the completion to drop the entry", func() bool { return r.ledgerLen(0) == 0 })
}

// TestCompletionWaitsForTheFileDomain covers: an entry is dropped only
// under the file's append domain, so a record whose sync landed stays in the
// ledger while the domain is held and goes once it is released.
func TestCompletionWaitsForTheFileDomain(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f := file(1)

	loc := r.appendWrite(t, f, 0, "gap")
	// Released by the end of the test even on a failed assertion, or the
	// worker would stay blocked on the domain and Stop would never join it.
	unlock := sync.OnceFunc(r.domains.Lock(f))
	defer unlock()
	r.syncStream(t, f)

	// The worker has been kicked by the Synced report and is blocked on the
	// domain: wait until it has asked for it, then check the entry stayed.
	eventually(t, "the worker to ask for the file domain", func() bool {
		r.domains.mu.Lock()
		defer r.domains.mu.Unlock()
		return r.domains.taken >= 2
	})
	if n := r.ledgerLen(0); n != 1 {
		t.Fatalf("the ledger holds %d entries while the file domain is held, want 1: a completion ran in the gap", n)
	}
	st := r.d.made[0]
	st.mu.Lock()
	done := st.ledger[0].done
	st.mu.Unlock()
	if done {
		t.Fatal("the entry is marked done while the file domain is held: the completion ran outside the domain")
	}
	if !r.d.Durable(loc) {
		t.Error("a record a sync covered reads as not durable while its completion waits for the domain")
	}
	unlock()
	eventually(t, "the entry to be dropped once the domain is free", func() bool { return r.ledgerLen(0) == 0 })
}

// TestEveryKindIsTrackedBeforeAppendReturns pins that a header-only record and
// a fill, appended straight through the stream, are registered, and that the
// header-only one carries its bytes.
func TestEveryKindIsTrackedBeforeAppendReturns(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRigUnstarted(t, 1)
	f := file(1)

	del := r.appendKind(t, f, format.KindDelete, format.RecordBody{})
	fill := r.appendKind(t, f, format.KindFill, format.RecordBody{Payload: []byte("filled")})

	st := r.d.made[0]
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.ledger) != 2 {
		t.Fatalf("the ledger holds %d entries after two appends, want 2", len(st.ledger))
	}
	if st.ledger[0].w.Loc != del || st.ledger[0].w.Kind != format.KindDelete {
		t.Errorf("entry 0 = %+v, want the delete at %+v", st.ledger[0].w, del)
	}
	if st.ledger[0].w.Bytes == nil {
		t.Error("a header-only record was tracked without its bytes")
	}
	if st.ledger[1].w.Loc != fill || st.ledger[1].w.Kind != format.KindFill {
		t.Errorf("entry 1 = %+v, want the fill at %+v", st.ledger[1].w, fill)
	}
	if st.ledger[1].w.Bytes != nil {
		t.Error("a fill carries a payload and must be tracked without its bytes")
	}
}

// TestDurableFollowsTheSyncThatCoversTheRecord covers: false before any
// sync, true right after one that covered the record, and false for a record
// appended after the sync began.
func TestDurableFollowsTheSyncThatCoversTheRecord(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	f := file(1)

	a := r.appendWrite(t, f, 0, "a")
	if r.d.Durable(a) {
		t.Fatal("Durable(a) = true before any sync")
	}

	// The sync is held at the seam, so b is appended after it began.
	h := r.seam.HoldFirst(t, journaltest.OpSync, format.SegmentName(a.Segment))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.streams.For(f).Sync()
	}()
	<-h.Reached()
	b := r.appendWrite(t, f, 8, "b")
	h.Release()
	<-done

	if !r.d.Durable(a) {
		t.Error("Durable(a) = false after a sync that covered it")
	}
	if r.d.Durable(b) {
		t.Error("Durable(b) = true, but b was appended after the sync began")
	}
	r.syncStream(t, f)
	if !r.d.Durable(b) {
		t.Error("Durable(b) = false after a sync that covered it")
	}
	if !r.d.Durable(segment.Location{Segment: a.Segment, Off: 1 << 40, Len: 8}) {
		t.Error("Durable of a location never tracked = false, want true")
	}
}

// TestTrackAllocatesNothingInSteadyState pins that the hot path allocates
// nothing once the ledger has grown.
func TestTrackAllocatesNothingInSteadyState(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates on its own")
	}
	d, err := New(testConfig(t, nopClock{}, &testDomains{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sink := d.Sink(0)
	w := segment.Written{Loc: segment.Location{Segment: 1, Off: 64, Len: 64}, File: file(1), Kind: format.KindWrite}
	for range 4096 {
		sink.Track(w)
	}
	if allocs := testing.AllocsPerRun(100, func() { sink.Track(w) }); allocs != 0 {
		t.Fatalf("Track allocated %v times per call, want 0", allocs)
	}
}

type nopClock struct{ core.Clock }

// TestStopJoinsTheWorkers pins that Stop leaves no goroutine or descriptor and
// that a second Stop is a no-op.
func TestStopJoinsTheWorkers(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 3)
	r.appendWrite(t, file(1), 0, "x")
	r.d.Stop()
	r.d.Stop()
	r.d.Start(r.streams) // a no-op after Stop
}

// TestSinkTwiceForOneStreamPanics pins the one-sink-per-stream rule.
func TestSinkTwiceForOneStreamPanics(t *testing.T) {
	d, err := New(testConfig(t, nopClock{}, &testDomains{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.Sink(0)
	defer func() {
		if recover() == nil {
			t.Fatal("a second Sink(0) did not panic")
		}
	}()
	d.Sink(0)
}

// TestNewRequiresClockAndDomains pins that a missing dependency is a caller bug.
func TestNewRequiresClockAndDomains(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no clock":    {Domains: &testDomains{}},
		"no domains":  {Clock: nopClock{}},
		"no counters": {Clock: nopClock{}, Domains: &testDomains{}, MarkLoss: func(core.FileID, core.Extent, core.Version) (index.HeldDelta, error) { return index.HeldDelta{}, nil }},
		"no marker":   {Clock: nopClock{}, Domains: &testDomains{}, Counters: &index.Counters{}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("New did not panic")
				}
			}()
			_, _ = New(cfg)
		})
	}
}

// TestStatsStartAtZero pins the shape of the report: a histogram with one
// bucket past the last bound.
func TestStatsStartAtZero(t *testing.T) {
	d, err := New(testConfig(t, nopClock{}, &testDomains{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p := d.Stats()
	if p.Syncs != 0 || p.SyncFailures != 0 {
		t.Errorf("Stats = %+v, want zero counters", p)
	}
	if len(p.SyncDuration.Counts) != len(SyncDurationBounds)+1 || len(p.SyncDuration.Bounds) != len(SyncDurationBounds) {
		t.Errorf("histogram shape = %d bounds, %d counts", len(p.SyncDuration.Bounds), len(p.SyncDuration.Counts))
	}
	p.SyncDuration.Counts[0] = 9
	if d.Stats().SyncDuration.Counts[0] != 0 {
		t.Error("Stats handed out the histogram itself, not a copy")
	}
}
