package segment

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// idleRig is the worker's rig: the streams the sealer walks, the sealer
// itself, and everything they are built over. Start is the caller's, so no
// goroutine runs before the check is ready to drive it.
type idleRig struct {
	streams *Streams
	worker  *IdleSealer
	seam    *journaltest.Seam
	set     *Set
	pool    *Pool
	clock   journaltest.DrivenClock
	cap     int64
}

// idleRigOver builds the worker's rig of n streams: a seam, a ledger, a
// maker, a started pool whose content spares are awaited before the build
// returns — twice the stream count, so no roll in any check below ever races
// the refill — a descriptor cache, the streams over them all, and the
// IdleSealer watching every stream on the rig's test clock, not started yet.
// A non-nil admit is the sealer's read-only check, nil a writable journal.
//
// The cleanups are registered in the order they must run: the earlier a
// component holds what a later one waits on, the earlier its cleanup is
// registered, for t.Cleanup runs them last-registered first. The sealer joins
// — R70: the streams and the segments close only after it has — then the
// streams release their refs, then the cache shuts down, then the pool stops.
// One rig per subtest: the profiler's watches must not overlap.
func idleRigOver(t *testing.T, n int, cap int64, threshold int64, admit func() error) *idleRig {
	t.Helper()

	seam, err := journaltest.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	set := NewSet(cap, nil)
	maker := NewMaker(seam.FS(), set, 1<<62, 0, nil)
	pool := NewPool(maker, Targets{Content: 2 * n}, nil)
	t.Cleanup(func() { pool.Stop() })
	pool.Start()
	waitSpares(t, pool, int64(2*n))
	cache := NewDescriptors(seam.FS(), format.SegmentName, core.NewDescriptorBudget(8))
	t.Cleanup(func() {
		if err := cache.Shutdown(); err != nil {
			t.Errorf("the cache's Shutdown: %v", err)
		}
	})
	streams := NewStreams(n, probeIdentity, set, pool, cache)
	t.Cleanup(func() { _ = streams.Close() })
	clock := journaltest.NewStopwatchClock()
	r := &idleRig{
		streams: streams,
		seam:    seam,
		set:     set,
		pool:    pool,
		clock:   clock,
		cap:     cap,
	}
	r.worker = NewIdleSealer(streams, clock, threshold, admit, nil)
	t.Cleanup(func() { r.worker.Stop() })
	return r
}

// withLog replaces the rig's worker with one that logs to log, before any
// start: the check that reads the level a verdict was logged at.
func (r *idleRig) withLog(log *slog.Logger) *idleRig {
	r.worker = NewIdleSealer(r.worker.streams, r.worker.clock, r.worker.threshold, r.worker.admit, log)
	return r
}

// waitSpares waits, bounded in real time — a failure deadline only, never the
// thing that moves the code under test, which is what the pool's own refill
// worker supplies — until the pool's ready content spares reach want. It is
// the settle every read of a counter the refill moves needs: a roll spends a
// spare and kicks the refill, whose whole make — the ledger reservation that
// raises the set's footprint by a cap before any storage call runs, the file
// it allocates, its rename into place — lands in the refill worker's own
// time. A check that reads a footprint or a spare count before the refill it
// kicked has finished reads one the refill can still move under it, and the
// idle seal's footprint fall is measured against a before the refill raises
// by a whole cap mid-test. Called after a test's last roll, it settles the
// refill that roll kicked before the counters are read.
func waitSpares(t *testing.T, p *Pool, want int64) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if c := p.Counters(); c.Spares >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pool never readied %d content spares: %+v", want, p.Counters())
		}
	}
}

// start starts the rig's worker and returns the rig.
func (r *idleRig) start() *idleRig {
	r.worker.Start()
	return r
}

// stream returns the rig's stream i.
func (r *idleRig) stream(i int) *Stream {
	return r.streams.Stream(i)
}

// advance waits until the worker has armed its wait on the clock — an
// unarmed worker is stuck, not slow, which is what armed checks for — and
// then moves the clock by d.
func (r *idleRig) advance(t *testing.T, d time.Duration) {
	t.Helper()
	r.armed(t)
	r.clock.Advance(d)
}

// armed blocks until the clock holds at least one armed wait, so a check
// that is about to advance never releases a wait the worker has not taken
// yet. Past a firing advance it means the pass the advance released has
// finished and the worker is armed on its next wait: a pass returns only
// once every seal it made is durable, accounted in the ledger and moved in
// the stream's own state, and the next wait is armed only after that, so
// what a check reads once armed holds is the pass's settled result. It never
// returns beside a stopped worker, so a pass that never re-arms is a worker
// that died mid-seal, which the bound reports rather than a hang hides.
func (r *idleRig) armed(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if r.clock.Pending() >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the test clock holds %d waits, want at least 1: the worker never armed its wait", r.clock.Pending())
		}
	}
}

// sealed polls, bounded in real time, until the ledger holds at least n
// sealed segments, then waits for the pass that sealed them to finish: the
// ledger counts a segment sealed inside the seal's write, before the free
// tail is returned and before the stream's own seal state moves, so the
// count alone can name a seal still running, and no verdict the checks read
// off the stream or the footprint is settled until the pass has returned and
// the worker is armed on its next wait. It fails the test outright if the
// count never arrives — an idle seal that never runs must be called that,
// not waited for.
func (r *idleRig) sealed(t *testing.T, n int64) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if got := r.set.Counters().Sealed; got >= n {
			r.armed(t)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ledger holds %d sealed segments, want at least %d", r.set.Counters().Sealed, n)
		}
	}
}

// idleRecordBytes is the bytes one write record of n payload bytes takes
// whole: its header, its body and its padding, the exact size a segment holds
// of it beyond the header. Thresholds are derived from it, never hard-coded.
func idleRecordBytes(n int64) int64 {
	return format.RecordHeader{Kind: format.KindWrite, Extent: core.Extent{Len: n}}.Size()
}

// TestTheConstructorRefusesItsBugs checks NewIdleSealer's panics: a nil
// Streams, a nil clock and a negative threshold are caller bugs the
// constructor names on the spot.
func TestTheConstructorRefusesItsBugs(t *testing.T) {
	prof.Watch(t, prof.Options{})

	r := idleRigOver(t, 1, 64<<10, 8, nil)
	for name, call := range map[string]func(){
		"a nil streams":        func() { _ = NewIdleSealer(nil, r.clock, 0, nil, nil) },
		"a nil clock":          func() { _ = NewIdleSealer(r.streams, nil, 0, nil, nil) },
		"a negative threshold": func() { _ = NewIdleSealer(r.streams, r.clock, -1, nil, nil) },
	} {
		call := call
		t.Run(name, func(t *testing.T) {
			defer func() {
				if rec := recover(); rec == nil {
					t.Fatalf("NewIdleSealer with %s did not panic", name)
				}
			}()
			call()
		})
	}
}

// TestTheIdleSealComesAtExactlyTheIdleAge catches the worker's off-by-one on
// the time bound: a stream past the threshold stays open a nanosecond short of
// the idle age with its tail uncut, and the nanosecond that crosses it seals
// the segment early — tail returned, the ledger holding it at the truncated
// size.
func TestTheIdleSealComesAtExactlyTheIdleAge(t *testing.T) {
	prof.Watch(t, prof.Options{})

	r := idleRigOver(t, 1, 64<<10, 8, nil).start()
	s := r.stream(0)
	w := idleAppend(t, s, 64, 0)

	r.advance(t, core.IdleSealAge-time.Nanosecond)
	if state, sealed := idleState(s), r.set.Counters().Sealed; state != sealOpen || sealed != 0 {
		t.Fatalf("a nanosecond before the idle age: state %v, %d sealed, want open and none", state, sealed)
	}
	name := format.SegmentName(w.Loc.Segment)
	if got := r.seam.CallsFor(journaltest.OpTruncate, name); got != 0 {
		t.Fatalf("a nanosecond before the idle age the segment saw %d truncations, want none", got)
	}
	// The pool's refill the first append's roll kicked has settled by here
	// — settled by the wait itself, not by the advances above, which move no
	// real time for the refill to run in — so the footprint read now is the
	// one the seal's fall is measured against.
	waitSpares(t, r.pool, 2)
	footprintBefore := r.set.Counters().UsedBytes

	// The first pass armed a whole idle age — whether it read the stream
	// before the append and fell back on the age, or read it after and
	// answered its fresh stamp with the age's whole remainder, for both
	// readings sat at the clock's origin. The nanosecond lands the clock
	// exactly on that wait, so the pass it releases sees the stream due.
	r.advance(t, time.Nanosecond)
	r.sealed(t, 1)
	if state := idleState(s); state != sealSealed {
		t.Fatalf("a nanosecond past the idle age: state %v, want sealed", state)
	}
	if got := r.seam.CallsFor(journaltest.OpTruncate, name); got == 0 {
		t.Fatal("the idle seal truncated nothing: the tail was not returned")
	}
	// The ledger fell by the whole returned tail: the segment is accounted at
	// the trailer's end — the header, the one record, the marker, its footer
	// entry and the trailer — and the footprint keeps the spares the pool
	// holds beside it, so the fall is the assertion, not the absolute.
	trailerEnd := format.SegmentHeaderSize + w.Loc.Len + format.SealMarkerSize + format.CatalogEntrySize + format.TrailerSize
	if fall := footprintBefore - r.set.Counters().UsedBytes; fall != r.cap-trailerEnd {
		t.Fatalf("the footprint fell by %d across the idle seal, want the whole tail %d", fall, r.cap-trailerEnd)
	}
}

// TestTheIdleSealLeavesAStreamOneNanosecondShortOfTheAgeOpen catches a
// worker that measures a stream's age ahead of the stream's own stamp: the
// pass one seal releases must leave a stream one nanosecond short of the
// idle age open, however far past the fallback wait the clock has moved.
// Two streams appended a nanosecond apart make that exact pass: stream 0's
// deadline releases the wait, and the pass it runs reads stream 1 — which
// a worker reading the clock one nanosecond early finds due and seals. A
// lone-stream check cannot catch this, for no pass runs below the age
// unless another stream's seal releases one.
func TestTheIdleSealLeavesAStreamOneNanosecondShortOfTheAgeOpen(t *testing.T) {
	prof.Watch(t, prof.Options{})

	r := idleRigOver(t, 2, 64<<10, 8, nil).start()
	s0, s1 := r.stream(0), r.stream(1)
	idleAppend(t, s0, 64, 0)
	r.advance(t, time.Nanosecond)
	idleAppend(t, s1, 64, 0)

	// The wait the worker armed at its start fires with the clock on the
	// age and stream 0 due: stream 1, appended a nanosecond after it, is a
	// nanosecond short of the age in the pass that runs. The pass settles
	// with stream 0 alone sealed, whatever the pass made of stream 1.
	r.advance(t, core.IdleSealAge-time.Nanosecond)
	r.armed(t)
	if state, sealed := idleState(s1), r.set.Counters().Sealed; state != sealOpen || sealed != 1 {
		t.Fatalf("a nanosecond short of the age in a running pass: stream 1 %v, %d sealed, want open and stream 0's alone", state, sealed)
	}

	// The nanosecond takes the clock onto the wait the last pass armed —
	// stream 1's own remaining wait — and the pass it releases seals it.
	r.advance(t, time.Nanosecond)
	r.sealed(t, 2)
	if state := idleState(s1); state != sealSealed {
		t.Fatalf("a nanosecond past the age: stream 1 %v, want sealed", state)
	}
}

// TestTheIdleSealNeedsMoreThanTheThreshold catches the worker's off-by-one on
// the bytes bound: a stream holding exactly the threshold beyond its header
// stays open past whole idle ages, and one slot — eight bytes — past it is
// sealed by the same advance.
func TestTheIdleSealNeedsMoreThanTheThreshold(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// One 64-byte write record takes idleRecordBytes(64) bytes past the
	// header — the whole threshold of the first part and more than the
	// second's.
	t.Run("at exactly the threshold it stays open", func(t *testing.T) {
		r := idleRigOver(t, 1, 64<<10, idleRecordBytes(64), nil).start()
		s := r.stream(0)
		if beyond := idleAppend(t, s, 64, 0).Loc.Len; beyond != idleRecordBytes(64) {
			t.Fatalf("the record holds %d bytes beyond the header, want exactly the threshold %d", beyond, idleRecordBytes(64))
		}
		r.advance(t, 2*core.IdleSealAge)
		r.armed(t)
		if state, sealed := idleState(s), r.set.Counters().Sealed; state != sealOpen || sealed != 0 {
			t.Fatalf("at exactly the threshold past two idle ages: state %v, %d sealed, want open and none", state, sealed)
		}
	})

	t.Run("one slot past it seals", func(t *testing.T) {
		r := idleRigOver(t, 1, 64<<10, idleRecordBytes(64)-8, nil).start()
		s := r.stream(0)
		idleAppend(t, s, 64, 0)
		r.advance(t, 2*core.IdleSealAge)
		r.sealed(t, 1)
		if state := idleState(s); state != sealSealed {
			t.Fatalf("one slot past the threshold past two idle ages: state %v, want sealed", state)
		}
	})
}

// TestAStreamBelowTheThresholdStaysOpenForAnyIdleTime checks that a stream
// well below the threshold is never a candidate: one whole advance at a time,
// past many idle ages, it stays open — the rule's age applies only to a
// segment whose bytes past its header pass the threshold.
func TestAStreamBelowTheThresholdStaysOpenForAnyIdleTime(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The threshold sits far above everything a segment cap can hold: no
	// stream can ever pass it.
	r := idleRigOver(t, 1, 64<<10, 1<<40, nil).start()
	s := r.stream(0)
	idleAppend(t, s, 64, 0)
	for step := 1; step <= 5; step++ {
		r.advance(t, 2*core.IdleSealAge)
		r.armed(t)
		if state, sealed := idleState(s), r.set.Counters().Sealed; state != sealOpen || sealed != 0 {
			t.Fatalf("after %d advances past whole idle ages: state %v, %d sealed, want open and none", step, state, sealed)
		}
	}
}

// TestAnIdleSealTakesNoSpare checks the spare the seal itself takes: none.
// Across an idle seal the pool's ready content spares never fall — the
// refill may raise them, and only the next append's roll spends one — and
// the ledger's segment count stands, for the stream stays on its sealed
// segment until the next append, which rolls onto a new segment whose header
// names the sealed one as its predecessor.
func TestAnIdleSealTakesNoSpare(t *testing.T) {
	prof.Watch(t, prof.Options{})

	r := idleRigOver(t, 1, 64<<10, 8, nil).start()
	s := r.stream(0)
	w := idleAppend(t, s, 64, 0)

	spares := r.pool.Counters().Spares
	segments := r.set.Counters().Segments
	r.advance(t, 2*core.IdleSealAge)
	r.sealed(t, 1)
	if after := r.pool.Counters().Spares; after < spares {
		t.Fatalf("the pool's content spares across the idle seal fell to %d from %d: the seal took a spare", after, spares)
	}
	if after := r.set.Counters().Segments; after != segments {
		t.Fatalf("the ledger's segment count across the idle seal = %d, want the same %d", after, segments)
	}

	p := idleAppend(t, s, 64, 64)
	if p.Loc.Segment == w.Loc.Segment {
		t.Fatal("the append after an idle seal landed in the sealed segment")
	}
	raw := make([]byte, format.SegmentHeaderSize)
	if _, err := s.ReadAt(p.Loc.Segment, raw, 0); err != nil {
		t.Fatalf("ReadAt of the successor's header: %v", err)
	}
	hdr, err := format.DecodeSegmentHeader(raw, probeIdentity)
	if err != nil {
		t.Fatalf("the successor's header does not decode: %v", err)
	}
	if hdr.Predecessor != w.Loc.Segment {
		t.Fatalf("the successor's predecessor = %d, want the idle-sealed %d", hdr.Predecessor, w.Loc.Segment)
	}
}

// TestAnAppendResetsTheIdleTime catches a deadline measured from the first
// append or from the worker's start: an append a second before the deadline
// moves it, so two more seconds of idleness — past the original deadline —
// leave the segment open, and it takes the whole age from the second append
// to seal it. The last advance lands exactly on the second append's
// deadline, so a worker that ignores a stream's remaining wait and sleeps the
// fallback age instead misses it — the check asks the armed wait, not just
// the sealed count.
func TestAnAppendResetsTheIdleTime(t *testing.T) {
	prof.Watch(t, prof.Options{})

	r := idleRigOver(t, 1, 64<<10, 8, nil).start()
	s := r.stream(0)
	idleAppend(t, s, 64, 0)

	r.advance(t, core.IdleSealAge-time.Second)
	idleAppend(t, s, 64, 64)

	r.advance(t, 2*time.Second)
	r.armed(t)
	if state := idleState(s); state != sealOpen {
		t.Fatalf("two seconds past the first append's deadline: state %v, want open", state)
	}

	// The second append stamped core.IdleSealAge-1s-2s of idleness away, so
	// the deadline it arms is exactly core.IdleSealAge-2s from the last
	// advance: the advance crosses the deadline exactly, no more.
	r.advance(t, core.IdleSealAge-2*time.Second)
	r.sealed(t, 1)
	if state := idleState(s); state != sealSealed {
		t.Fatalf("a whole age past the second append: state %v, want sealed", state)
	}
}

// TestARefusedAdmissionStopsTheIdleSeal checks R89 at the worker: while the
// read-only check refuses, a stream past both bounds stays open with no write
// and no sync of its file at the seam and no seal for idle ages on end, and
// the worker never spins on the refusal; once the check passes, the next pass
// seals the still-idle segment. core holds no read-only sentinel on this
// base, so the check answers a local error, which its contract allows: any
// non-nil refusal stops the seal.
func TestARefusedAdmissionStopsTheIdleSeal(t *testing.T) {
	prof.Watch(t, prof.Options{})

	var allow atomic.Bool
	readOnly := errors.New("segment: the journal is read-only")
	admit := func() error {
		if allow.Load() {
			return nil
		}
		return readOnly
	}

	r := idleRigOver(t, 1, 64<<10, 8, admit).start()
	s := r.stream(0)
	w := idleAppend(t, s, 64, 0)
	name := format.SegmentName(w.Loc.Segment)
	writes := r.seam.CallsFor(journaltest.OpWrite, name)
	syncs := r.seam.CallsFor(journaltest.OpSync, name)

	r.advance(t, 3*core.IdleSealAge)
	r.armed(t)
	if after := r.seam.CallsFor(journaltest.OpWrite, name); after != writes {
		t.Fatalf("the seam saw %d writes of %s while the check refused, want the %d it held", after, name, writes)
	}
	if after := r.seam.CallsFor(journaltest.OpSync, name); after != syncs {
		t.Fatalf("the seam saw %d syncs of %s while the check refused, want the %d it held", after, name, syncs)
	}
	if state, sealed := idleState(s), r.set.Counters().Sealed; state != sealOpen || sealed != 0 {
		t.Fatalf("past both bounds under the refusing check: state %v, %d sealed, want open and none", state, sealed)
	}

	allow.Store(true)
	r.advance(t, core.IdleSealAge)
	r.sealed(t, 1)
	if state := idleState(s); state != sealSealed {
		t.Fatalf("after the check passed: state %v, want sealed", state)
	}
}

// TestEachStreamSealsOnItsOwnBytes checks a pass that seals only the streams
// past both bounds: stream 0, holding a record the threshold names, is
// sealed; stream 1, idle as long but holding the threshold exactly, stays
// open — a pass that seals every stream, or stops at the first.
func TestEachStreamSealsOnItsOwnBytes(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The threshold sits exactly at the small record's size, so stream 1's
	// lone 32-byte write holds exactly it — never a candidate — while stream
	// 0's 64-byte write passes it.
	threshold := idleRecordBytes(32)
	r := idleRigOver(t, 2, 64<<10, threshold, nil).start()
	s0, s1 := r.stream(0), r.stream(1)
	if beyond := idleAppend(t, s0, 64, 0).Loc.Len; beyond <= threshold {
		t.Fatalf("the big record holds %d bytes beyond the header, want past the threshold %d", beyond, threshold)
	}
	if beyond := idleAppend(t, s1, 32, 0).Loc.Len; beyond != threshold {
		t.Fatalf("the small record holds %d bytes beyond the header, want exactly the threshold %d", beyond, threshold)
	}

	r.advance(t, 2*core.IdleSealAge)
	r.sealed(t, 1)
	if state := idleState(s0); state != sealSealed {
		t.Fatalf("stream 0, past the threshold, after two idle ages: %v, want sealed", state)
	}
	if state := idleState(s1); state != sealOpen {
		t.Fatalf("stream 1, at exactly the threshold, after two idle ages: %v, want open", state)
	}
	if sealed := r.set.Counters().Sealed; sealed != 1 {
		t.Fatalf("the ledger holds %d sealed segments, want exactly stream 0's", sealed)
	}
}

// TestTheSealerStopsAndJoins checks the lifecycle's edges: Stop before Start
// returns at once and leaves Start starting nothing; Start is idempotent; a
// second Stop is a no-op. A worker Stop fails to join shows as the leaked
// goroutine prof.Watch reports, so each part also asserts the worker's clock
// holds no wait the stopped worker would have armed.
func TestTheSealerStopsAndJoins(t *testing.T) {
	prof.Watch(t, prof.Options{})

	t.Run("Stop before Start", func(t *testing.T) {
		r := idleRigOver(t, 1, 64<<10, 8, nil)
		r.worker.Stop()  // returns at once: nothing is running
		r.worker.Stop()  // and again: a no-op
		r.worker.Start() // after Stop: starts nothing
		s := r.stream(0)
		idleAppend(t, s, 64, 0)
		r.clock.Advance(2 * core.IdleSealAge)
		for deadline := time.Now().Add(200 * time.Millisecond); ; {
			if state, sealed := idleState(s), r.set.Counters().Sealed; state == sealSealed || sealed != 0 {
				t.Fatalf("a worker stopped before it started sealed something: state %v, %d sealed", state, sealed)
			}
			if pending := r.clock.Pending(); pending != 0 {
				t.Fatalf("the stopped worker armed %d waits on the clock, want none: Start after Stop started a goroutine", pending)
			}
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(time.Millisecond)
		}
	})

	t.Run("Start then Stop joins", func(t *testing.T) {
		r := idleRigOver(t, 1, 64<<10, 8, nil)
		r.worker.Start()
		r.worker.Start() // idempotent: one goroutine arms one wait
		r.armed(t)
		// The wait the running worker armed is abandoned by Stop — the clock
		// never closes it — so what the check holds is its count: nothing
		// further arms while the worker is gone.
		armed := r.clock.Pending()
		if armed != 1 {
			t.Fatalf("the doubly-started worker armed %d waits, want the one of a single goroutine", armed)
		}
		r.worker.Stop()
		r.worker.Stop()  // the second is a no-op
		r.worker.Start() // after Stop: starts nothing
		r.clock.Advance(10 * time.Millisecond)
		if pending := r.clock.Pending(); pending != armed {
			t.Fatalf("after Stop the clock holds %d waits, want the same %d it abandoned", pending, armed)
		}
	})
}

// TestStopWaitsForASealInFlight checks R70's near half: a Stop that arrives
// while a seal is writing has not returned while the seal's own sync is held
// at the seam, and returns — the segment sealed — once the hold lifts, so the
// caller that stops the sealer may then close the segments under it.
func TestStopWaitsForASealInFlight(t *testing.T) {
	prof.Watch(t, prof.Options{})

	r := idleRigOver(t, 1, 64<<10, 8, nil).start()
	s := r.stream(0)
	w := idleAppend(t, s, 64, 0)
	name := format.SegmentName(w.Loc.Segment)
	held := r.seam.HoldFirst(t, journaltest.OpSync, name)

	r.advance(t, 2*core.IdleSealAge)
	select {
	case <-held.Reached():
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never reached the seal's first sync")
	}

	stopped := make(chan struct{})
	go func() {
		r.worker.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while the seal was still held at the seam")
	case <-time.After(100 * time.Millisecond):
	}

	held.Release()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop never returned after the hold lifted")
	}
	// The join settled the seal whole — the pass it ran in is waited out by
	// Stop — so the count is read directly rather than through sealed, which
	// waits for a re-arm no stopped worker ever makes.
	if got := r.set.Counters().Sealed; got != 1 {
		t.Fatalf("after the in-flight seal the ledger holds %d sealed segments, want 1", got)
	}
	if state := idleState(s); state != sealSealed {
		t.Fatalf("the segment after the in-flight seal: %v, want sealed", state)
	}
}

// TestAFailingSealDoesNotKillTheWorker checks the worker's error half: a seal
// whose every sync fails at the seam ends its segment — the stream's next
// Sync answers ErrSegmentEnded — and the same worker goes on to seal that
// stream's successor, and a second stream beside it, once the fault is
// disarmed and they idle past both bounds again.
func TestAFailingSealDoesNotKillTheWorker(t *testing.T) {
	prof.Watch(t, prof.Options{})

	r := idleRigOver(t, 2, 64<<10, 8, nil).start()
	s0, s1 := r.stream(0), r.stream(1)
	w0 := idleAppend(t, s0, 64, 0)
	idleAppend(t, s1, 64, 0)

	// Every sync of stream 0's first segment fails; stream 1's own file is
	// not named, so its seal runs clean through the same pass.
	if err := r.seam.Schedule(journaltest.Failing(journaltest.OpSync, format.SegmentName(w0.Loc.Segment), 0, errors.New("the sync failed"))); err != nil {
		t.Fatalf("the fault schedule: %v", err)
	}
	r.advance(t, 2*core.IdleSealAge)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		st0, st1 := idleState(s0), idleState(s1)
		if st0 == sealEnded && st1 == sealSealed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the failing seal: stream 0 %v, stream 1 %v, want ended and sealed", st0, st1)
		}
	}
	if err := s0.Sync(); !errors.Is(err, ErrSegmentEnded) {
		t.Fatalf("stream 0's Sync after its failed seal = %v, want ErrSegmentEnded", err)
	}

	// The fault disarmed, the same worker seals both streams' successors once
	// they idle again: stream 0's next append rolls off its ended segment, and
	// stream 1's off its sealed one.
	if err := r.seam.Schedule(); err != nil {
		t.Fatalf("disarming the fault: %v", err)
	}
	idleAppend(t, s0, 64, 128)
	idleAppend(t, s1, 64, 128)
	r.advance(t, 2*core.IdleSealAge)
	r.sealed(t, 3)
	for _, s := range []*Stream{s0, s1} {
		if state := idleState(s); state != sealSealed {
			t.Fatalf("a stream whose successor should have idle-sealed: %v, want sealed", state)
		}
	}
}

// TestAnOutOfBandEndIsErrorNotTheLostRaceLog checks the log level the worker
// gives a seal whose segment was ended in the ledger from outside while the
// stream still held it open and sealable: WriteSeal's check answers the
// ledger's ErrWrongState and the seal ends the segment, so the verdict the
// worker logs reaches the outcome switch's generic Error case — not the
// ErrLedgerDisagrees case beside it, which no test can reach (a seal durable
// on disk that the ledger refuses needs a Set double the real-storage rule
// forbids), and not the lost-race Debug, for the one race the worker can
// really lose is a close of the stream itself. The stream's own state names
// which Error this was: ended, never sealed.
func TestAnOutOfBandEndIsErrorNotTheLostRaceLog(t *testing.T) {
	prof.Watch(t, prof.Options{})

	var buf idleLogBuffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	r := idleRigOver(t, 1, 64<<10, 8, nil).withLog(log)
	// The worker has not started: the out-of-band end lands with no seal in
	// flight anywhere.
	s := r.stream(0)
	w := idleAppend(t, s, 64, 0)
	if err := r.set.End(w.Loc.Segment); err != nil {
		t.Fatalf("the out-of-band end: %v", err)
	}
	r.start()

	r.advance(t, 2*core.IdleSealAge)
	r.armed(t)

	line := buf.line(t, "the idle seal")
	if !strings.Contains(line, "level=ERROR") {
		t.Fatalf("the out-of-band-ended seal's log line = %q, want level=ERROR", line)
	}
	if strings.Contains(line, "lost the segment to a close") {
		t.Fatalf("the out-of-band-ended seal's log line passes itself off as a lost race: %q", line)
	}

	// The stream saw the same thing its next Sync does: the segment ended,
	// never sealed — the generic Error case's own outcome, and the word that
	// separates an ended segment from the ErrLedgerDisagrees case beside it,
	// whose segment would stand sealed.
	if state := idleState(s); state != sealEnded {
		t.Fatalf("the stream's seal state after the out-of-band-ended seal: %v, want ended", state)
	}
}

// idleLogBuffer is a synchronized sink for one worker's log lines, so a test
// can read the line a level check names without racing the worker that
// writes it.
type idleLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *idleLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// line returns the first line containing want, or the empty string when none
// arrived.
func (b *idleLogBuffer) line(t *testing.T, want string) string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	for line := range strings.SplitSeq(b.buf.String(), "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	return ""
}
