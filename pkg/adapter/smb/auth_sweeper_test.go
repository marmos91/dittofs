package smb

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// TestAuthSweeper_RequestDoesNotBlockCaller is the guard for the defect: the
// control-plane fires its auth-cache subscribers synchronously, so a sweep run
// inline holds up the operator's API call for the length of the walk.
func TestAuthSweeper_RequestDoesNotBlockCaller(t *testing.T) {
	// Buffered: the sweep signals without blocking, so an unbuffered channel
	// drops the signal whenever the worker reaches the send before this
	// goroutine parks on the receive.
	sweeping := make(chan struct{}, 1)
	release := make(chan struct{})
	sw := newAuthSweeper(func(context.Context) {
		select {
		case sweeping <- struct{}{}:
		default:
		}
		<-release
	})
	t.Cleanup(func() { close(release); sw.stop(context.Background()) })

	sw.request()
	select {
	case <-sweeping:
	case <-time.After(5 * time.Second):
		t.Fatal("sweep never started")
	}

	// A second request lands while the first sweep is parked. Inline, this is
	// the call that stalls.
	done := make(chan struct{})
	go func() { defer close(done); sw.request() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request blocked behind an in-flight sweep")
	}
}

// TestAuthSweeper_CoalescesBurst pins the other half. A bulk grant edit raises
// one invalidation per row; without coalescing those become N queued sweeps
// over the same tables. A burst that lands during one sweep must collapse to a
// single follow-up run, and that run must start after the last request so it
// observes the final state.
func TestAuthSweeper_CoalescesBurst(t *testing.T) {
	var sweeps atomic.Int32
	first := make(chan struct{})
	release := make(chan struct{})
	settled := make(chan struct{}, 16)

	sw := newAuthSweeper(func(context.Context) {
		if sweeps.Add(1) == 1 {
			close(first)
			<-release
		}
		select {
		case settled <- struct{}{}:
		default:
		}
	})
	t.Cleanup(func() { sw.stop(context.Background()) })

	sw.request()
	<-first

	for range 50 {
		sw.request()
	}
	close(release)

	// Drain the follow-up sweep, then give any further queued ones a window to
	// appear.
	select {
	case <-settled:
	case <-time.After(5 * time.Second):
		t.Fatal("first sweep never completed")
	}
	select {
	case <-settled:
	case <-time.After(5 * time.Second):
		t.Fatal("burst never produced a follow-up sweep")
	}
	time.Sleep(100 * time.Millisecond)

	if got := sweeps.Load(); got != 2 {
		t.Fatalf("50 requests during one sweep produced %d sweeps, want 2 (the in-flight one plus one follow-up)", got)
	}
}

// TestAuthSweeper_StopJoinsInFlightSweep pins the shutdown contract: stop must
// not return while a sweep is still running, or the sweep reaches into session
// and handler state during teardown.
func TestAuthSweeper_StopJoinsInFlightSweep(t *testing.T) {
	started := make(chan struct{})
	var finished atomic.Bool
	var sawCancel atomic.Bool

	sw := newAuthSweeper(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		sawCancel.Store(true)
		time.Sleep(50 * time.Millisecond)
		finished.Store(true)
	})

	sw.request()
	<-started
	sw.stop(context.Background())

	if !sawCancel.Load() {
		t.Error("stop did not cancel the in-flight sweep's context")
	}
	if !finished.Load() {
		t.Error("stop returned while a sweep was still running")
	}

	// Idempotent, and a request after stop must not panic or hang.
	sw.stop(context.Background())
	sw.request()
}

// TestAuthSweeper_StopWithNoSweepRunning covers the common case: an adapter
// that was wired but never invalidated still has a goroutine to join.
func TestAuthSweeper_StopWithNoSweepRunning(t *testing.T) {
	var sweeps atomic.Int32
	sw := newAuthSweeper(func(context.Context) { sweeps.Add(1) })

	done := make(chan struct{})
	go func() { defer close(done); sw.stop(context.Background()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop hung with no sweep in flight")
	}
	if got := sweeps.Load(); got != 0 {
		t.Fatalf("sweeps = %d, want 0", got)
	}
}

// TestAdapter_AuthInvalidateIsOffloadedAndJoined pins the wiring: SetRuntime
// must hand the sweep to the worker rather than subscribing the sweep itself,
// an invalidation must return to the control-plane goroutine without waiting
// for it, and Stop must join the worker so no sweep outlives teardown.
func TestAdapter_AuthInvalidateIsOffloadedAndJoined(t *testing.T) {
	a := New(Config{})
	rt := runtime.New(nil)
	a.SetRuntime(rt)

	a.resolverMu.Lock()
	sweeper := a.authSweep
	a.resolverMu.Unlock()
	if sweeper == nil {
		t.Fatal("SetRuntime did not start the authorization sweep worker")
	}

	// The control-plane path must not block on the sweep.
	done := make(chan struct{})
	go func() { defer close(done); rt.InvalidateAuthCache() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("InvalidateAuthCache blocked on the SMB sweep")
	}

	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	a.resolverMu.Lock()
	remaining := a.authSweep
	a.resolverMu.Unlock()
	if remaining != nil {
		t.Error("Stop left the sweep worker attached")
	}
	// The worker is joined, so a second stop returns at once rather than hanging.
	stopped := make(chan struct{})
	go func() { defer close(stopped); sweeper.stop(context.Background()) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not join the sweep worker")
	}
}

// TestAuthSweeper_StopGivesUpOnDeadline pins the shutdown bound. A sweep does
// plenty no context reaches — the revalidate mutex, closing a revoked session's
// opens, draining its parked locks — so an unbounded join lets one slow store
// call hold shutdown open forever, and the caller's shutdown deadline does not
// cover a Stop that never returns.
func TestAuthSweeper_StopGivesUpOnDeadline(t *testing.T) {
	stuck := make(chan struct{})
	started := make(chan struct{})
	sw := newAuthSweeper(func(context.Context) {
		close(started)
		<-stuck
	})
	t.Cleanup(func() { close(stuck); sw.stop(context.Background()) })

	sw.request()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- sw.stop(ctx) }()
	select {
	case joined := <-done:
		if joined {
			t.Fatal("stop reported a join while the sweep was still stuck")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop ignored its deadline and hung on an unresponsive sweep")
	}
}
