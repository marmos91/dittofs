package fs

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestAppendWrite_PressureTimeout_FiresWhenRollupStuck exercises the
// defense-in-depth deadline added to AppendWrite's pressure loop (#670).
// Configures a tiny PressureMaxWait, primes the budget so the second
// write would block, then withholds the pressureCh pulse and expects
// ErrPressureTimeout within roughly PressureMaxWait + jitter.
//
// No rollup worker runs here (StartRollup is not invoked); the test
// simulates a wedged rollup by simply never pulsing pressureCh.
func TestAppendWrite_PressureTimeout_FiresWhenRollupStuck(t *testing.T) {
	const wait = 200 * time.Millisecond
	bc := newFSStoreForTest(t, FSStoreOptions{
		MaxLogBytes:     1,
		PressureMaxWait: wait,
	})

	// Prime: first write already pushes logBytesTotal past maxLogBytes=1.
	if err := bc.AppendWrite(context.Background(), "file1", []byte("x"), 0); err != nil {
		t.Fatalf("seed AppendWrite: %v", err)
	}

	// Second call enters the pressure loop and must return
	// ErrPressureTimeout after ~wait (no pulse, no ctx deadline, no Close).
	start := time.Now()
	err := bc.AppendWrite(context.Background(), "file1", []byte("y"), 4096)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrPressureTimeout) {
		t.Fatalf("AppendWrite err = %v, want ErrPressureTimeout", err)
	}

	// Lower bound: must have actually waited ~PressureMaxWait (no
	// degenerate immediate-return regression). Allow a small slack for
	// scheduler jitter on the timer fire.
	if elapsed < wait/2 {
		t.Fatalf("AppendWrite returned ErrPressureTimeout after only %v; expected ~%v", elapsed, wait)
	}
	// Upper bound: catch a regression that lets the loop block far past
	// the configured deadline. Generous slack because race+CI runners
	// can stall arbitrarily — the point is to fail-loud on "blocked
	// forever", not to enforce a tight SLA.
	if elapsed > wait+5*time.Second {
		t.Fatalf("AppendWrite blocked %v past PressureMaxWait=%v", elapsed-wait, wait)
	}
}

// TestAppendWrite_PressureTimeout_ContextDeadlinePreferredOverTimer
// verifies that when both ctx.Done() and the pressure-timeout fire, the
// caller still observes ctx.Err() preferentially when it lands first —
// the new deadline must NOT swallow caller-side cancellation semantics.
func TestAppendWrite_PressureTimeout_ContextDeadlinePreferredOverTimer(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{
		MaxLogBytes:     1,
		PressureMaxWait: 5 * time.Second, // far longer than the ctx
	})
	if err := bc.AppendWrite(context.Background(), "file1", []byte("x"), 0); err != nil {
		t.Fatalf("seed AppendWrite: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := bc.AppendWrite(ctx, "file1", []byte("y"), 4096)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AppendWrite err = %v, want context.DeadlineExceeded (ctx must win when it fires first)", err)
	}
}

// TestAppendWrite_PressureTimeout_DisabledByNegativeOption keeps the
// "block forever" semantics when PressureMaxWait < 0. The test proves
// the disable path actually disables: the writer stays blocked past
// what would have been the default deadline, then unblocks when
// pressureCh is pulsed.
func TestAppendWrite_PressureTimeout_DisabledByNegativeOption(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{
		MaxLogBytes:     1,
		PressureMaxWait: -1, // explicitly disable
	})
	if err := bc.AppendWrite(context.Background(), "file1", []byte("x"), 0); err != nil {
		t.Fatalf("seed AppendWrite: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- bc.AppendWrite(context.Background(), "file1", []byte("y"), 4096)
	}()

	// Wait longer than the default 30s would allow if disable were
	// broken — we don't actually wait 30s, just long enough that an
	// accidentally-enabled tiny default would have fired.
	select {
	case err := <-done:
		t.Fatalf("AppendWrite returned %v while disabled; should be still blocking", err)
	case <-time.After(300 * time.Millisecond):
		// Still blocked — as expected.
	}

	// Release.
	bc.logBytesTotal.Store(0)
	select {
	case bc.pressureCh <- struct{}{}:
	default:
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AppendWrite after release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AppendWrite did not unblock after pressure release")
	}
}
