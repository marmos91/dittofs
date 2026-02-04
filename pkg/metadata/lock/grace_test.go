package lock

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/errors"
)

func TestNewGracePeriodManager_StartsNormal(t *testing.T) {
	gpm := NewGracePeriodManager(5*time.Second, nil)
	defer gpm.Close()

	if gpm.GetState() != GraceStateNormal {
		t.Errorf("expected GraceStateNormal, got %v", gpm.GetState())
	}

	if gpm.GetRemainingTime() != 0 {
		t.Errorf("expected 0 remaining time in normal state, got %v", gpm.GetRemainingTime())
	}
}

func TestEnterGracePeriod_BlocksNewLocks(t *testing.T) {
	gpm := NewGracePeriodManager(5*time.Second, nil)
	defer gpm.Close()

	// Enter grace period
	gpm.EnterGracePeriod([]string{"client1", "client2"})

	if gpm.GetState() != GraceStateActive {
		t.Errorf("expected GraceStateActive, got %v", gpm.GetState())
	}

	// New lock should be blocked
	newLockOp := Operation{IsNew: true}
	allowed, err := gpm.IsOperationAllowed(newLockOp)
	if allowed {
		t.Error("expected new lock to be blocked during grace period")
	}
	if err == nil {
		t.Error("expected error for blocked operation")
	}

	// Verify error is ErrGracePeriod
	storeErr, ok := err.(*errors.StoreError)
	if !ok {
		t.Fatalf("expected *StoreError, got %T", err)
	}
	if storeErr.Code != errors.ErrGracePeriod {
		t.Errorf("expected ErrGracePeriod, got %v", storeErr.Code)
	}
}

func TestEnterGracePeriod_AllowsReclaims(t *testing.T) {
	gpm := NewGracePeriodManager(5*time.Second, nil)
	defer gpm.Close()

	// Enter grace period
	gpm.EnterGracePeriod([]string{"client1"})

	// Reclaim operation should be allowed
	reclaimOp := Operation{IsReclaim: true}
	allowed, err := gpm.IsOperationAllowed(reclaimOp)
	if !allowed {
		t.Error("expected reclaim to be allowed during grace period")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestEnterGracePeriod_AllowsTests(t *testing.T) {
	gpm := NewGracePeriodManager(5*time.Second, nil)
	defer gpm.Close()

	// Enter grace period
	gpm.EnterGracePeriod([]string{"client1"})

	// Test operation should be allowed
	testOp := Operation{IsTest: true}
	allowed, err := gpm.IsOperationAllowed(testOp)
	if !allowed {
		t.Error("expected test operation to be allowed during grace period")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestExitGracePeriod_AllowsAllOperations(t *testing.T) {
	gpm := NewGracePeriodManager(5*time.Second, nil)
	defer gpm.Close()

	// Enter then exit grace period
	gpm.EnterGracePeriod([]string{"client1"})
	gpm.ExitGracePeriod()

	if gpm.GetState() != GraceStateNormal {
		t.Errorf("expected GraceStateNormal after exit, got %v", gpm.GetState())
	}

	// All operations should be allowed
	ops := []Operation{
		{IsNew: true},
		{IsReclaim: true},
		{IsTest: true},
	}

	for _, op := range ops {
		allowed, err := gpm.IsOperationAllowed(op)
		if !allowed {
			t.Errorf("expected operation to be allowed after grace period exit: %+v", op)
		}
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}
}

func TestGracePeriod_ExpiresAfterDuration(t *testing.T) {
	callbackCalled := make(chan struct{})

	gpm := NewGracePeriodManager(50*time.Millisecond, func() {
		close(callbackCalled)
	})
	defer gpm.Close()

	// Enter grace period
	gpm.EnterGracePeriod([]string{"client1"})

	if gpm.GetState() != GraceStateActive {
		t.Error("expected active state immediately after entering")
	}

	// Wait for expiration
	select {
	case <-callbackCalled:
		// OK
	case <-time.After(500 * time.Millisecond):
		t.Fatal("grace period did not expire within expected time")
	}

	// State should be normal after expiration
	if gpm.GetState() != GraceStateNormal {
		t.Errorf("expected GraceStateNormal after expiration, got %v", gpm.GetState())
	}
}

func TestGracePeriod_EarlyExitWhenAllReclaimed(t *testing.T) {
	callbackCalled := make(chan struct{})

	gpm := NewGracePeriodManager(5*time.Second, func() {
		close(callbackCalled)
	})
	defer gpm.Close()

	// Enter grace period with 2 expected clients
	gpm.EnterGracePeriod([]string{"client1", "client2"})

	// Mark first client as reclaimed - should not exit yet
	gpm.MarkReclaimed("client1")
	if gpm.GetState() != GraceStateActive {
		t.Error("expected to still be active after first reclaim")
	}

	// Mark second client as reclaimed - should trigger early exit
	gpm.MarkReclaimed("client2")

	// Wait for callback
	select {
	case <-callbackCalled:
		// OK - early exit triggered
	case <-time.After(100 * time.Millisecond):
		t.Fatal("early exit callback was not called")
	}

	if gpm.GetState() != GraceStateNormal {
		t.Errorf("expected GraceStateNormal after early exit, got %v", gpm.GetState())
	}
}

func TestGracePeriod_CallsOnGraceEndCallback(t *testing.T) {
	var callbackCount int32

	gpm := NewGracePeriodManager(50*time.Millisecond, func() {
		atomic.AddInt32(&callbackCount, 1)
	})
	defer gpm.Close()

	// Test timer-based exit
	gpm.EnterGracePeriod([]string{})
	time.Sleep(100 * time.Millisecond)

	if atomic.LoadInt32(&callbackCount) != 1 {
		t.Errorf("expected callback to be called once, got %d", callbackCount)
	}

	// Test manual exit (should also call callback)
	gpm.EnterGracePeriod([]string{})
	gpm.ExitGracePeriod()

	// Give time for callback
	time.Sleep(10 * time.Millisecond)

	if atomic.LoadInt32(&callbackCount) != 2 {
		t.Errorf("expected callback to be called twice, got %d", callbackCount)
	}
}

func TestGracePeriod_ThreadSafety(t *testing.T) {
	gpm := NewGracePeriodManager(100*time.Millisecond, nil)
	defer gpm.Close()

	var wg sync.WaitGroup
	iterations := 100

	// Concurrent operations
	wg.Add(4)

	// Goroutine 1: Enter/exit grace period
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			gpm.EnterGracePeriod([]string{"client1", "client2"})
			gpm.ExitGracePeriod()
		}
	}()

	// Goroutine 2: Check operations
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			gpm.IsOperationAllowed(Operation{IsNew: true})
			gpm.IsOperationAllowed(Operation{IsReclaim: true})
		}
	}()

	// Goroutine 3: Mark reclaimed
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			gpm.MarkReclaimed("client1")
			gpm.MarkReclaimed("client2")
		}
	}()

	// Goroutine 4: Read state
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			gpm.GetState()
			gpm.GetRemainingTime()
			gpm.GetExpectedClients()
			gpm.GetReclaimedClients()
		}
	}()

	wg.Wait()
	// No deadlocks or panics = success
}

func TestGraceState_String(t *testing.T) {
	tests := []struct {
		state    GraceState
		expected string
	}{
		{GraceStateNormal, "normal"},
		{GraceStateActive, "active"},
		{GraceState(99), "unknown"},
	}

	for _, tt := range tests {
		if got := tt.state.String(); got != tt.expected {
			t.Errorf("GraceState(%d).String() = %q, want %q", tt.state, got, tt.expected)
		}
	}
}

func TestGracePeriod_GetExpectedAndReclaimedClients(t *testing.T) {
	gpm := NewGracePeriodManager(5*time.Second, nil)
	defer gpm.Close()

	// Enter grace period with known clients
	gpm.EnterGracePeriod([]string{"client1", "client2", "client3"})

	// Check expected clients
	expected := gpm.GetExpectedClients()
	if len(expected) != 3 {
		t.Errorf("expected 3 expected clients, got %d", len(expected))
	}

	// Reclaim some
	gpm.MarkReclaimed("client1")
	gpm.MarkReclaimed("client2")

	// Check reclaimed clients
	reclaimed := gpm.GetReclaimedClients()
	if len(reclaimed) != 2 {
		t.Errorf("expected 2 reclaimed clients, got %d", len(reclaimed))
	}
}

func TestGracePeriod_GetRemainingTime(t *testing.T) {
	gpm := NewGracePeriodManager(1*time.Second, nil)
	defer gpm.Close()

	// Normal state - should be 0
	if gpm.GetRemainingTime() != 0 {
		t.Errorf("expected 0 remaining in normal state")
	}

	// Enter grace period
	gpm.EnterGracePeriod([]string{})

	// Should have some time remaining
	remaining := gpm.GetRemainingTime()
	if remaining <= 0 || remaining > 1*time.Second {
		t.Errorf("unexpected remaining time: %v", remaining)
	}

	// Wait a bit
	time.Sleep(100 * time.Millisecond)

	// Should have less time now
	remaining2 := gpm.GetRemainingTime()
	if remaining2 >= remaining {
		t.Error("remaining time should decrease over time")
	}
}

func TestGracePeriod_EnterWhileActive_Noop(t *testing.T) {
	gpm := NewGracePeriodManager(5*time.Second, nil)
	defer gpm.Close()

	// Enter grace period
	gpm.EnterGracePeriod([]string{"client1"})

	expected1 := gpm.GetExpectedClients()

	// Enter again (should be no-op)
	gpm.EnterGracePeriod([]string{"client2", "client3"})

	// Expected clients should not change
	expected2 := gpm.GetExpectedClients()
	if len(expected2) != len(expected1) {
		t.Error("re-entering grace period should be a no-op")
	}
}

func TestGracePeriod_ExitWhileNormal_Noop(t *testing.T) {
	callbackCalled := false
	gpm := NewGracePeriodManager(5*time.Second, func() {
		callbackCalled = true
	})
	defer gpm.Close()

	// Exit without entering (should be no-op)
	gpm.ExitGracePeriod()

	if callbackCalled {
		t.Error("callback should not be called when exiting from normal state")
	}

	if gpm.GetState() != GraceStateNormal {
		t.Error("state should remain normal")
	}
}

func TestGracePeriod_MarkReclaimedWhileNormal_Noop(t *testing.T) {
	gpm := NewGracePeriodManager(5*time.Second, nil)
	defer gpm.Close()

	// Mark reclaimed while in normal state (should be no-op)
	gpm.MarkReclaimed("client1")

	// Should still be empty
	if len(gpm.GetReclaimedClients()) != 0 {
		t.Error("reclaimed clients should be empty in normal state")
	}
}

func TestGracePeriod_Close(t *testing.T) {
	callbackCalled := false
	gpm := NewGracePeriodManager(5*time.Second, func() {
		callbackCalled = true
	})

	gpm.EnterGracePeriod([]string{"client1"})

	// Close should stop the timer but NOT call the callback
	gpm.Close()

	if gpm.GetState() != GraceStateNormal {
		t.Error("state should be normal after close")
	}

	// Wait a bit to ensure callback isn't called
	time.Sleep(10 * time.Millisecond)

	if callbackCalled {
		t.Error("callback should not be called on Close()")
	}
}

func TestGracePeriod_NoExpectedClients_NoEarlyExit(t *testing.T) {
	callbackCalled := make(chan struct{})

	gpm := NewGracePeriodManager(100*time.Millisecond, func() {
		close(callbackCalled)
	})
	defer gpm.Close()

	// Enter with no expected clients
	gpm.EnterGracePeriod([]string{})

	// Mark random client as reclaimed - should NOT trigger early exit
	gpm.MarkReclaimed("random-client")

	// Should still be active (no early exit when no expected clients)
	if gpm.GetState() != GraceStateActive {
		t.Error("should still be active when no expected clients defined")
	}

	// Wait for timer to expire
	select {
	case <-callbackCalled:
		// OK - timer expired
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timer should have expired")
	}
}

func TestGracePeriod_GetDuration(t *testing.T) {
	duration := 42 * time.Second
	gpm := NewGracePeriodManager(duration, nil)
	defer gpm.Close()

	if gpm.GetDuration() != duration {
		t.Errorf("expected duration %v, got %v", duration, gpm.GetDuration())
	}
}
