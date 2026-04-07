package health

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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

	// First call probes.
	if got := c.Healthcheck(context.Background()); got.Status != StatusHealthy {
		t.Fatalf("first call: got %v, want healthy", got.Status)
	}
	// Subsequent calls within TTL reuse the result without probing.
	for i := 0; i < 5; i++ {
		c.Healthcheck(context.Background())
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("inner probe called %d times, want 1", got)
	}
}

func TestCachedChecker_ProbesAgainAfterTTL(t *testing.T) {
	// Drive the cache off a fake clock so the test doesn't have to sleep.
	// Saving and restoring the package-level Now is safe here because
	// tests run serially in this package (no t.Parallel).
	real := Now
	defer func() { Now = real }()

	var fakeNow time.Time
	Now = func() time.Time { return fakeNow }

	inner := &countingChecker{report: Report{Status: StatusHealthy}}
	c := NewCachedChecker(inner, 50*time.Millisecond)

	fakeNow = time.Unix(1000, 0)
	c.Healthcheck(context.Background())

	// Still within TTL: no new probe.
	fakeNow = fakeNow.Add(49 * time.Millisecond)
	c.Healthcheck(context.Background())
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("within TTL: probes = %d, want 1", got)
	}

	// Past TTL: a new probe runs.
	fakeNow = fakeNow.Add(10 * time.Millisecond)
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
	// The probe is artificially slow. If single-flight is broken, each
	// goroutine will spin up its own probe and the call count will match
	// the number of goroutines. With correct single-flight it should be
	// exactly 1 because the first caller holds the mutex through the
	// slow probe and everyone else sees the freshly cached result.
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
	c := NewCachedChecker(inner, time.Hour) // long enough to never expire

	c.Healthcheck(context.Background())
	c.Invalidate()
	c.Healthcheck(context.Background())

	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("invalidate: probes = %d, want 2", got)
	}
}

func TestCachedChecker_SatisfiesCheckerInterface(t *testing.T) {
	// Compile-time assertion already exists in cache.go, but an explicit
	// test case makes the invariant visible here too.
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
