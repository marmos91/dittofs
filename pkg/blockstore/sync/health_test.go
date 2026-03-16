package sync

import (
	"context"
	"errors"
	gosync "sync"
	"sync/atomic"
	"testing"
	"time"
)

// errProbe is a sentinel error for health probe failures in tests.
var errProbe = errors.New("probe failure")

// controllableProbe returns a probe function that fails until told to succeed.
// The failCount tracks how many times the probe has been called while failing.
func controllableProbe(shouldFail *atomic.Bool) (func(ctx context.Context) error, *atomic.Int32) {
	failCount := &atomic.Int32{}
	return func(ctx context.Context) error {
		if shouldFail.Load() {
			failCount.Add(1)
			return errProbe
		}
		return nil
	}, failCount
}

// fastHealthConfig returns a Config with short probe intervals for unit tests.
func fastHealthConfig() Config {
	cfg := DefaultConfig()
	cfg.HealthCheckInterval = 10 * time.Millisecond
	cfg.UnhealthyCheckInterval = 10 * time.Millisecond
	cfg.HealthCheckFailureThreshold = 3
	return cfg
}

func TestHealthMonitor_StartsHealthy(t *testing.T) {
	shouldFail := &atomic.Bool{}
	probe, _ := controllableProbe(shouldFail)

	hm := NewHealthMonitor(probe, DefaultConfig())
	if !hm.IsHealthy() {
		t.Fatal("expected HealthMonitor to start healthy")
	}
}

func TestHealthMonitor_ThreeFailuresMarkUnhealthy(t *testing.T) {
	shouldFail := &atomic.Bool{}
	shouldFail.Store(true)
	probe, _ := controllableProbe(shouldFail)

	hm := NewHealthMonitor(probe, fastHealthConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hm.Start(ctx)
	defer hm.Stop()

	// Wait enough time for at least 3 probes (3 * 10ms + margin)
	time.Sleep(80 * time.Millisecond)

	if hm.IsHealthy() {
		t.Fatal("expected HealthMonitor to be unhealthy after 3 consecutive failures")
	}
}

func TestHealthMonitor_RecoverAfterOneSuccess(t *testing.T) {
	shouldFail := &atomic.Bool{}
	shouldFail.Store(true)
	probe, _ := controllableProbe(shouldFail)

	hm := NewHealthMonitor(probe, fastHealthConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hm.Start(ctx)
	defer hm.Stop()

	// Wait for unhealthy
	time.Sleep(80 * time.Millisecond)
	if hm.IsHealthy() {
		t.Fatal("expected unhealthy after failures")
	}

	// Allow probe to succeed
	shouldFail.Store(false)

	// Wait for at least 1 successful probe
	time.Sleep(30 * time.Millisecond)

	if !hm.IsHealthy() {
		t.Fatal("expected HealthMonitor to recover after 1 successful probe")
	}
}

func TestHealthMonitor_FewerThanThreeFailuresStaysHealthy(t *testing.T) {
	callCount := &atomic.Int32{}
	probe := func(ctx context.Context) error {
		n := callCount.Add(1)
		// Fail only the first 2 calls, then succeed
		if n <= 2 {
			return errProbe
		}
		return nil
	}

	hm := NewHealthMonitor(probe, fastHealthConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hm.Start(ctx)
	defer hm.Stop()

	// Wait long enough for all probes to fire
	time.Sleep(80 * time.Millisecond)

	if !hm.IsHealthy() {
		t.Fatal("expected HealthMonitor to stay healthy with fewer than 3 consecutive failures")
	}
}

func TestHealthMonitor_TransitionCallbackInvoked(t *testing.T) {
	shouldFail := &atomic.Bool{}
	shouldFail.Store(true)
	probe, _ := controllableProbe(shouldFail)

	hm := NewHealthMonitor(probe, fastHealthConfig())

	var mu gosync.Mutex
	var transitions []bool
	hm.SetTransitionCallback(func(healthy bool) {
		mu.Lock()
		defer mu.Unlock()
		transitions = append(transitions, healthy)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hm.Start(ctx)
	defer hm.Stop()

	// Wait for unhealthy transition
	time.Sleep(80 * time.Millisecond)

	// Now recover
	shouldFail.Store(false)
	time.Sleep(30 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(transitions) < 2 {
		t.Fatalf("expected at least 2 transitions (unhealthy + healthy), got %d: %v", len(transitions), transitions)
	}
	if transitions[0] != false {
		t.Fatalf("expected first transition to be unhealthy (false), got %v", transitions[0])
	}
	if transitions[1] != true {
		t.Fatalf("expected second transition to be healthy (true), got %v", transitions[1])
	}
}

func TestHealthMonitor_NilProbeFunctionAlwaysHealthy(t *testing.T) {
	hm := NewHealthMonitor(nil, DefaultConfig())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hm.Start(ctx)
	defer hm.Stop()

	// Even after starting, nil probe should keep healthy
	time.Sleep(30 * time.Millisecond)

	if !hm.IsHealthy() {
		t.Fatal("expected nil probe HealthMonitor to always be healthy")
	}
}

func TestHealthMonitor_StopCleansUp(t *testing.T) {
	shouldFail := &atomic.Bool{}
	probe, _ := controllableProbe(shouldFail)

	hm := NewHealthMonitor(probe, fastHealthConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hm.Start(ctx)

	// Let it run briefly
	time.Sleep(30 * time.Millisecond)

	// Stop should not hang
	done := make(chan struct{})
	go func() {
		hm.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Stop completed - good
	case <-time.After(1 * time.Second):
		t.Fatal("Stop() did not return within 1s")
	}
}

func TestHealthMonitor_OutageDuration(t *testing.T) {
	shouldFail := &atomic.Bool{}
	shouldFail.Store(true)
	probe, _ := controllableProbe(shouldFail)

	hm := NewHealthMonitor(probe, fastHealthConfig())

	// When healthy, outage duration is 0
	if d := hm.OutageDuration(); d != 0 {
		t.Fatalf("expected 0 outage duration when healthy, got %v", d)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hm.Start(ctx)
	defer hm.Stop()

	// Wait for unhealthy
	time.Sleep(80 * time.Millisecond)

	if hm.IsHealthy() {
		t.Fatal("expected unhealthy")
	}

	d := hm.OutageDuration()
	if d <= 0 {
		t.Fatalf("expected positive outage duration when unhealthy, got %v", d)
	}

	// Recover
	shouldFail.Store(false)
	time.Sleep(30 * time.Millisecond)

	if !hm.IsHealthy() {
		t.Fatal("expected recovery")
	}
	if d := hm.OutageDuration(); d != 0 {
		t.Fatalf("expected 0 outage duration after recovery, got %v", d)
	}
}
