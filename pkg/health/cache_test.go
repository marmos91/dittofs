package health

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withFakeClock installs a fake clock on a specific CachedChecker and
// returns a function that advances it. Using an instance-level clock
// (rather than a package-level variable) means tests running in parallel
// don't trample each other, and the race detector has nothing to flag
// because each checker has its own clock field.
//
// The internal mutex guards the fake timestamp against concurrent reads
// from probe goroutines and writes from the advance() calls: any test
// that combines fake time with concurrent Healthcheck callers will be
// correctly synchronised.
func withFakeClock(c *CachedChecker, start time.Time) (advance func(time.Duration)) {
	var (
		mu   sync.Mutex
		fake = start
	)
	c.clock = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return fake
	}
	return func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		fake = fake.Add(d)
	}
}

// countingChecker records how many times Healthcheck has been called and
// returns a configurable canned report. It is safe for concurrent use.
type countingChecker struct {
	calls  atomic.Int64
	report Report
	// probeDelay lets tests simulate a slow underlying probe, which is
	// how we verify single-flight behavior (concurrent callers should
	// share the result of one slow probe, not each trigger their own).
	probeDelay time.Duration
}

func (c *countingChecker) Healthcheck(ctx context.Context) Report {
	c.calls.Add(1)
	if c.probeDelay > 0 {
		select {
		case <-time.After(c.probeDelay):
		case <-ctx.Done():
			return Report{Status: StatusUnknown, Message: ctx.Err().Error()}
		}
	}
	return c.report
}

func TestCachedChecker_ServesFromCacheWithinTTL(t *testing.T) {
	inner := &countingChecker{report: Report{Status: StatusHealthy, CheckedAt: time.Now()}}
	c := NewCachedChecker(inner, 100*time.Millisecond)

	if got := c.Healthcheck(context.Background()); got.Status != StatusHealthy {
		t.Fatalf("first call: got %v, want healthy", got.Status)
	}
	for i := 0; i < 5; i++ {
		c.Healthcheck(context.Background())
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("inner probe called %d times, want 1", got)
	}
}

func TestCachedChecker_ProbesAgainAfterTTL(t *testing.T) {
	inner := &countingChecker{report: Report{Status: StatusHealthy}}
	c := NewCachedChecker(inner, 50*time.Millisecond)
	advance := withFakeClock(c, time.Unix(1000, 0))

	c.Healthcheck(context.Background())

	advance(49 * time.Millisecond)
	c.Healthcheck(context.Background())
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("within TTL: probes = %d, want 1", got)
	}

	advance(10 * time.Millisecond)
	c.Healthcheck(context.Background())
	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("past TTL: probes = %d, want 2", got)
	}
}

func TestCachedChecker_ZeroTTLDisablesCaching(t *testing.T) {
	inner := &countingChecker{report: Report{Status: StatusHealthy}}
	c := NewCachedChecker(inner, 0)
	for i := 0; i < 3; i++ {
		c.Healthcheck(context.Background())
	}
	if got := inner.calls.Load(); got != 3 {
		t.Fatalf("zero TTL: probes = %d, want 3", got)
	}
}

func TestCachedChecker_NegativeTTLTreatedAsZero(t *testing.T) {
	inner := &countingChecker{report: Report{Status: StatusHealthy}}
	c := NewCachedChecker(inner, -1*time.Second)
	c.Healthcheck(context.Background())
	c.Healthcheck(context.Background())
	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("negative TTL: probes = %d, want 2", got)
	}
}

func TestCachedChecker_SingleFlightUnderConcurrency(t *testing.T) {
	inner := &countingChecker{
		report:     Report{Status: StatusHealthy},
		probeDelay: 20 * time.Millisecond,
	}
	c := NewCachedChecker(inner, time.Second)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			c.Healthcheck(context.Background())
		}()
	}
	wg.Wait()

	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("single-flight broken: probes = %d, want 1", got)
	}
}

func TestCachedChecker_InvalidateForcesReprobe(t *testing.T) {
	inner := &countingChecker{report: Report{Status: StatusHealthy}}
	c := NewCachedChecker(inner, time.Hour)

	c.Healthcheck(context.Background())
	c.Invalidate()
	c.Healthcheck(context.Background())

	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("invalidate: probes = %d, want 2", got)
	}
}

// TestCachedChecker_WaiterRespectsContextCancellation verifies the
// behavior promised by the Checker contract: a caller blocked waiting
// for an in-flight probe must be able to return early when their own
// context is canceled, without blocking until the probe finishes.
//
// Setup: a slow probe runs in goroutine A. Goroutine B calls Healthcheck
// with a context that B cancels mid-flight. B must return promptly with
// a StatusUnknown report, while A's probe continues to completion.
func TestCachedChecker_WaiterRespectsContextCancellation(t *testing.T) {
	probeStarted := make(chan struct{})
	probeCanFinish := make(chan struct{})
	inner := CheckerFunc(func(ctx context.Context) Report {
		close(probeStarted)
		<-probeCanFinish
		return Report{Status: StatusHealthy}
	})
	c := NewCachedChecker(inner, time.Hour)

	// Start the leader probe.
	leaderDone := make(chan Report, 1)
	go func() {
		leaderDone <- c.Healthcheck(context.Background())
	}()
	<-probeStarted

	// Start a waiter whose context we will cancel.
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterResult := make(chan Report, 1)
	go func() {
		waiterResult <- c.Healthcheck(waiterCtx)
	}()

	// Give the waiter a moment to enter its select.
	time.Sleep(10 * time.Millisecond)

	// Cancel the waiter. It must return quickly with StatusUnknown.
	cancelWaiter()
	select {
	case rep := <-waiterResult:
		if rep.Status != StatusUnknown {
			t.Fatalf("canceled waiter: got status %q, want unknown", rep.Status)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("canceled waiter did not return promptly — still blocked on probe")
	}

	// Leader must still be running — its probe was not affected.
	select {
	case <-leaderDone:
		t.Fatal("leader probe finished early; waiter cancellation should not cancel the probe")
	default:
	}

	// Let the probe finish and verify the leader gets its healthy result.
	close(probeCanFinish)
	select {
	case rep := <-leaderDone:
		if rep.Status != StatusHealthy {
			t.Fatalf("leader: got status %q, want healthy", rep.Status)
		}
	case <-time.After(time.Second):
		t.Fatal("leader probe did not finish after probeCanFinish closed")
	}
}

// TestCachedChecker_LeaderCancellationDoesNotPoisonCache verifies rule #1
// of the context-handling contract: the probe runs with a detached
// context, so a leader whose own caller context is already canceled
// still runs the probe against a fresh context and publishes a real
// result — not a StatusUnknown that would poison the cache for every
// subsequent caller.
func TestCachedChecker_LeaderCancellationDoesNotPoisonCache(t *testing.T) {
	inner := &countingChecker{report: Report{Status: StatusHealthy}}
	c := NewCachedChecker(inner, time.Hour)

	// Hand the leader a context that is ALREADY canceled.
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	rep := c.Healthcheck(canceledCtx)

	// The probe should have run (we're the leader, the cache was
	// empty, and the leader path doesn't select on the caller's ctx).
	if rep.Status != StatusHealthy {
		t.Fatalf("leader with canceled ctx: got status %q, want healthy — probe ran against detached ctx", rep.Status)
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("probe not invoked under canceled leader ctx: calls = %d", got)
	}

	// Subsequent callers see the cached healthy result, not a poisoned unknown.
	for i := 0; i < 3; i++ {
		r := c.Healthcheck(context.Background())
		if r.Status != StatusHealthy {
			t.Fatalf("subsequent caller #%d: got %q, want healthy — cache was poisoned", i, r.Status)
		}
	}
}

// TestCachedChecker_ProbeUsesDetachedContext verifies the probe does not
// receive the caller's context. If the cache forwarded the caller's ctx
// directly, a canceled caller ctx would show up inside the probe — this
// test asserts it does not.
//
// The ctx state must be observed *during* the probe, not after. The
// cache cancels the probe ctx via defer once the probe returns, so any
// post-hoc inspection of the ctx reference would see it canceled and
// cause a false positive. The CheckerFunc captures Err() and Deadline()
// inline while still running.
func TestCachedChecker_ProbeUsesDetachedContext(t *testing.T) {
	var (
		observedErr         error
		observedHasDeadline bool
		probeRan            atomic.Bool
	)
	inner := CheckerFunc(func(ctx context.Context) Report {
		observedErr = ctx.Err()
		_, observedHasDeadline = ctx.Deadline()
		probeRan.Store(true)
		return Report{Status: StatusHealthy}
	})
	c := NewCachedChecker(inner, 0) // cache disabled so probe always runs

	callerCtx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before the call
	c.Healthcheck(callerCtx)

	if !probeRan.Load() {
		t.Fatal("probe was not invoked")
	}
	if observedErr != nil {
		t.Fatalf("probe received a canceled context (err=%v); expected a detached, non-canceled ctx", observedErr)
	}
	if !observedHasDeadline {
		t.Fatal("probe context should carry a deadline from the probeTimeout, got none")
	}
}

// TestCachedChecker_PanicInProbeDoesNotWedgeCache ensures that a panic
// in the underlying probe cannot leave the cache in a state where
// c.inflight stays non-nil and call.done is never closed — which would
// block every subsequent caller forever. The cache must recover, cache
// an unhealthy report, release waiters, and accept new callers.
func TestCachedChecker_PanicInProbeDoesNotWedgeCache(t *testing.T) {
	// First call panics; subsequent call should be unblocked (cache
	// cleaned up) and serve the cached unhealthy report within TTL.
	inner := CheckerFunc(func(ctx context.Context) Report {
		panic("probe exploded")
	})
	c := NewCachedChecker(inner, time.Hour)

	// Leader call: expect the panic to re-propagate, but the cache
	// internals must be cleaned up before the panic reaches us.
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic to propagate to leader caller")
			}
		}()
		c.Healthcheck(context.Background())
	}()

	// Subsequent call must not block indefinitely. We give it a
	// generous 2s budget; a wedged cache would time out here.
	result := make(chan Report, 1)
	go func() {
		result <- c.Healthcheck(context.Background())
	}()
	select {
	case rep := <-result:
		if rep.Status != StatusUnhealthy {
			t.Fatalf("after panic: got status %q, want unhealthy", rep.Status)
		}
		if rep.Message == "" {
			t.Fatal("after panic: expected a non-empty message describing the panic")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cache wedged: subsequent Healthcheck blocked after a probe panic")
	}
}

// TestCachedChecker_PanicInProbeReleasesWaiters verifies that waiters
// blocked on an in-flight probe are released when that probe panics,
// and that they see the synthesized unhealthy report rather than
// blocking indefinitely on a never-closed done channel.
func TestCachedChecker_PanicInProbeReleasesWaiters(t *testing.T) {
	probeStarted := make(chan struct{})
	probeShouldPanic := make(chan struct{})
	inner := CheckerFunc(func(ctx context.Context) Report {
		close(probeStarted)
		<-probeShouldPanic
		panic("deliberate test panic")
	})
	c := NewCachedChecker(inner, time.Hour)

	// Goroutine A is the leader.
	leaderPanicked := make(chan bool, 1)
	go func() {
		defer func() {
			leaderPanicked <- (recover() != nil)
		}()
		c.Healthcheck(context.Background())
	}()
	<-probeStarted

	// Goroutine B is a waiter that joins while the probe is in flight.
	waiterResult := make(chan Report, 1)
	go func() {
		waiterResult <- c.Healthcheck(context.Background())
	}()

	// Give B a moment to hit the wait branch.
	time.Sleep(10 * time.Millisecond)

	// Trigger the panic. Both leader and waiter must unblock.
	close(probeShouldPanic)

	select {
	case panicked := <-leaderPanicked:
		if !panicked {
			t.Fatal("leader did not receive the probe's panic")
		}
	case <-time.After(time.Second):
		t.Fatal("leader did not return after probe panic")
	}

	select {
	case rep := <-waiterResult:
		if rep.Status != StatusUnhealthy {
			t.Fatalf("waiter after panic: got %q, want unhealthy", rep.Status)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter was not released after probe panic — cache wedged")
	}
}

func TestCachedChecker_NilInnerPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewCachedChecker(nil, ...) did not panic")
		}
	}()
	NewCachedChecker(nil, time.Second)
}

func TestCachedChecker_NonPositiveProbeTimeoutFallsBack(t *testing.T) {
	inner := &countingChecker{report: Report{Status: StatusHealthy}}
	c := NewCachedCheckerWithTimeout(inner, time.Hour, 0)
	if c.probeTimeout != DefaultProbeTimeout {
		t.Fatalf("probeTimeout fallback: got %v, want %v", c.probeTimeout, DefaultProbeTimeout)
	}
}

func TestCachedChecker_SatisfiesCheckerInterface(t *testing.T) {
	var _ Checker = (*CachedChecker)(nil)
}

func TestCheckerFunc(t *testing.T) {
	called := false
	f := CheckerFunc(func(ctx context.Context) Report {
		called = true
		return Report{Status: StatusDegraded, Message: "test"}
	})
	rep := f.Healthcheck(context.Background())
	if !called {
		t.Fatal("CheckerFunc did not invoke wrapped function")
	}
	if rep.Status != StatusDegraded {
		t.Fatalf("CheckerFunc: got %v, want degraded", rep.Status)
	}
}
