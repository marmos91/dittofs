package state

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// ============================================================================
// GracePeriodState Unit Tests
// ============================================================================

func TestGracePeriod_Active(t *testing.T) {
	gp := NewGracePeriodState(100*time.Millisecond, nil)
	defer gp.Stop()

	// Should be inactive initially
	if gp.IsInGrace() {
		t.Error("grace period should be inactive before StartGrace")
	}

	// Start with some expected clients
	gp.StartGrace([]uint64{1, 2, 3})

	// Should now be active
	if !gp.IsInGrace() {
		t.Error("grace period should be active after StartGrace")
	}

	// Wait for it to expire
	time.Sleep(200 * time.Millisecond)

	// Should now be inactive
	if gp.IsInGrace() {
		t.Error("grace period should be inactive after duration")
	}
}

func TestGracePeriod_BlocksNewOpen(t *testing.T) {
	sm := NewStateManager(5*time.Second, 200*time.Millisecond)
	defer sm.Shutdown()

	// Start grace period with some expected clients
	sm.StartGracePeriod([]uint64{100, 200})

	// CheckGraceForNewState should return NFS4ERR_GRACE
	err := sm.CheckGraceForNewState()
	if err == nil {
		t.Fatal("CheckGraceForNewState should return error during grace period")
	}

	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T: %v", err, err)
	}
	if stateErr.Status != types.NFS4ERR_GRACE {
		t.Errorf("status = %d, want NFS4ERR_GRACE (%d)", stateErr.Status, types.NFS4ERR_GRACE)
	}
}

func TestGracePeriod_AllowsReclaim(t *testing.T) {
	sm := NewStateManager(5*time.Second, 5*time.Second)
	defer sm.Shutdown()

	// Create and confirm a client first
	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	result, err := sm.SetClientID("reclaim-client", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	err = sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	// Start grace period with this client AND a phantom client (so grace doesn't end early)
	sm.StartGracePeriod([]uint64{result.ClientID, 99999})

	// CLAIM_NULL should be blocked during grace
	_, err = sm.OpenFile(
		result.ClientID, []byte("owner2"), 1,
		[]byte("fh-new"),
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err == nil {
		t.Fatal("OpenFile with CLAIM_NULL should fail during grace")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T: %v", err, err)
	}
	if stateErr.Status != types.NFS4ERR_GRACE {
		t.Errorf("status = %d, want NFS4ERR_GRACE (%d)", stateErr.Status, types.NFS4ERR_GRACE)
	}

	// CLAIM_PREVIOUS should be allowed during grace
	openResult, err := sm.OpenFile(
		result.ClientID, []byte("owner1"), 1,
		[]byte("fh-reclaim"),
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_PREVIOUS,
	)
	if err != nil {
		t.Fatalf("OpenFile with CLAIM_PREVIOUS should succeed during grace: %v", err)
	}
	if openResult == nil {
		t.Fatal("OpenFile result should not be nil")
	}
}

func TestGracePeriod_EarlyExit(t *testing.T) {
	var endCalled int32
	sm := NewStateManager(5*time.Second, 5*time.Second)
	defer sm.Shutdown()

	// Override with a grace period that has a callback tracker
	gp := NewGracePeriodState(5*time.Second, func() {
		atomic.AddInt32(&endCalled, 1)
	})
	sm.mu.Lock()
	sm.gracePeriod = gp
	sm.mu.Unlock()

	// Start with two expected clients
	gp.StartGrace([]uint64{100, 200})

	if !gp.IsInGrace() {
		t.Fatal("grace period should be active")
	}

	// First client reclaims
	gp.ClientReclaimed(100)
	if !gp.IsInGrace() {
		t.Fatal("grace period should still be active after one reclaim")
	}

	// Second client reclaims -- should trigger early exit
	gp.ClientReclaimed(200)

	// Allow goroutine scheduling
	time.Sleep(10 * time.Millisecond)

	if gp.IsInGrace() {
		t.Error("grace period should have ended after all clients reclaimed")
	}
	if atomic.LoadInt32(&endCalled) != 1 {
		t.Errorf("onGraceEnd callback should have been called once, got %d", atomic.LoadInt32(&endCalled))
	}
}

func TestGracePeriod_EmptyClients(t *testing.T) {
	gp := NewGracePeriodState(100*time.Millisecond, nil)
	defer gp.Stop()

	// Start with no expected clients
	gp.StartGrace([]uint64{})

	// Should NOT enter grace period
	if gp.IsInGrace() {
		t.Error("grace period should be skipped when no expected clients")
	}
}

func TestGracePeriod_AutoExpiry(t *testing.T) {
	var endCalled int32
	gp := NewGracePeriodState(80*time.Millisecond, func() {
		atomic.AddInt32(&endCalled, 1)
	})
	defer gp.Stop()

	// Start with expected clients, but don't reclaim any
	gp.StartGrace([]uint64{100, 200, 300})

	if !gp.IsInGrace() {
		t.Fatal("grace period should be active")
	}

	// Wait for auto-expiry
	time.Sleep(200 * time.Millisecond)

	if gp.IsInGrace() {
		t.Error("grace period should have expired after duration")
	}
	if atomic.LoadInt32(&endCalled) != 1 {
		t.Errorf("onGraceEnd should have been called once, got %d", atomic.LoadInt32(&endCalled))
	}
}

func TestGracePeriod_NoGraceForReclaim(t *testing.T) {
	sm := NewStateManager(5*time.Second, 5*time.Second)
	defer sm.Shutdown()

	// Create and confirm a client
	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	result, err := sm.SetClientID("no-grace-client", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	err = sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	// No grace period active -- CLAIM_PREVIOUS should fail with NFS4ERR_NO_GRACE
	_, err = sm.OpenFile(
		result.ClientID, []byte("owner1"), 1,
		[]byte("fh-no-grace"),
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_PREVIOUS,
	)
	if err == nil {
		t.Fatal("OpenFile with CLAIM_PREVIOUS should fail outside grace period")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T: %v", err, err)
	}
	if stateErr.Status != types.NFS4ERR_NO_GRACE {
		t.Errorf("status = %d, want NFS4ERR_NO_GRACE (%d)", stateErr.Status, types.NFS4ERR_NO_GRACE)
	}
}

func TestSaveClientState(t *testing.T) {
	sm := NewStateManager(5 * time.Second)
	defer sm.Shutdown()

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	// Create and confirm multiple clients
	clientIDs := make([]uint64, 0)
	for i := 0; i < 3; i++ {
		clientIDStr := "snapshot-client-" + string(rune('A'+i))
		result, err := sm.SetClientID(clientIDStr, verifier, callback, "10.0.0.1:1234")
		if err != nil {
			t.Fatalf("SetClientID %d: %v", i, err)
		}
		err = sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier)
		if err != nil {
			t.Fatalf("ConfirmClientID %d: %v", i, err)
		}
		clientIDs = append(clientIDs, result.ClientID)
	}

	// Save state
	snapshots := sm.SaveClientState()
	if len(snapshots) != 3 {
		t.Fatalf("expected 3 snapshots, got %d", len(snapshots))
	}

	// Verify all client IDs are present
	snapshotIDs := make(map[uint64]bool)
	for _, s := range snapshots {
		snapshotIDs[s.ClientID] = true
		if s.ClientIDString == "" {
			t.Error("snapshot ClientIDString should not be empty")
		}
		if s.ClientAddr == "" {
			t.Error("snapshot ClientAddr should not be empty")
		}
	}
	for _, id := range clientIDs {
		if !snapshotIDs[id] {
			t.Errorf("client ID %d missing from snapshots", id)
		}
	}

	// GetConfirmedClientIDs should match
	confirmedIDs := sm.GetConfirmedClientIDs()
	if len(confirmedIDs) != 3 {
		t.Fatalf("expected 3 confirmed IDs, got %d", len(confirmedIDs))
	}
}

func TestConcurrentGracePeriod(t *testing.T) {
	gp := NewGracePeriodState(5*time.Second, nil)
	defer gp.Stop()

	// Start with many expected clients
	expectedIDs := make([]uint64, 100)
	for i := range expectedIDs {
		expectedIDs[i] = uint64(i + 1)
	}
	gp.StartGrace(expectedIDs)

	// Concurrent reclaim calls from multiple goroutines
	var wg sync.WaitGroup
	wg.Add(len(expectedIDs))

	for _, id := range expectedIDs {
		go func(clientID uint64) {
			defer wg.Done()
			gp.ClientReclaimed(clientID)
		}(id)
	}

	wg.Wait()

	// Allow goroutine scheduling
	time.Sleep(10 * time.Millisecond)

	// Grace should have ended (all clients reclaimed)
	if gp.IsInGrace() {
		t.Error("grace period should have ended after all concurrent reclaims")
	}
}

// ============================================================================
// GraceStatus Tests
// ============================================================================

func TestGraceStatus(t *testing.T) {
	t.Run("active_grace", func(t *testing.T) {
		gp := NewGracePeriodState(5*time.Second, nil)
		defer gp.Stop()

		gp.StartGrace([]uint64{100, 200, 300})

		status := gp.Status()
		if !status.Active {
			t.Error("Status.Active should be true during grace")
		}
		if status.RemainingSeconds <= 0 {
			t.Error("RemainingSeconds should be > 0 during active grace")
		}
		if status.RemainingSeconds > 5.0 {
			t.Errorf("RemainingSeconds = %f, should be <= 5.0", status.RemainingSeconds)
		}
		if status.ExpectedClients != 3 {
			t.Errorf("ExpectedClients = %d, want 3", status.ExpectedClients)
		}
		if status.ReclaimedClients != 0 {
			t.Errorf("ReclaimedClients = %d, want 0", status.ReclaimedClients)
		}
		if status.StartedAt.IsZero() {
			t.Error("StartedAt should not be zero during active grace")
		}
		if status.TotalDuration != 5*time.Second {
			t.Errorf("TotalDuration = %v, want 5s", status.TotalDuration)
		}
	})

	t.Run("inactive_grace", func(t *testing.T) {
		gp := NewGracePeriodState(5*time.Second, nil)
		defer gp.Stop()

		status := gp.Status()
		if status.Active {
			t.Error("Status.Active should be false before StartGrace")
		}
		if status.RemainingSeconds != 0 {
			t.Errorf("RemainingSeconds = %f, want 0 when inactive", status.RemainingSeconds)
		}
	})

	t.Run("after_all_reclaimed", func(t *testing.T) {
		gp := NewGracePeriodState(5*time.Second, nil)
		defer gp.Stop()

		gp.StartGrace([]uint64{100, 200})

		// Reclaim all clients
		gp.ClientReclaimed(100)
		gp.ClientReclaimed(200)

		// Allow early exit to propagate
		time.Sleep(10 * time.Millisecond)

		status := gp.Status()
		if status.Active {
			t.Error("Status.Active should be false after all clients reclaimed")
		}
	})
}

// ============================================================================
// ForceEndGrace Tests
// ============================================================================

func TestForceEndGrace(t *testing.T) {
	var endCalled int32
	gp := NewGracePeriodState(5*time.Second, func() {
		atomic.AddInt32(&endCalled, 1)
	})
	defer gp.Stop()

	gp.StartGrace([]uint64{100, 200, 300})

	if !gp.IsInGrace() {
		t.Fatal("Grace period should be active")
	}

	// Force end
	gp.ForceEnd()

	if gp.IsInGrace() {
		t.Error("Grace period should be inactive after ForceEnd")
	}

	// Callback should have been called
	if atomic.LoadInt32(&endCalled) != 1 {
		t.Errorf("onGraceEnd should have been called once, got %d", atomic.LoadInt32(&endCalled))
	}

	// Idempotent: calling again is a no-op
	gp.ForceEnd()
	if atomic.LoadInt32(&endCalled) != 1 {
		t.Errorf("onGraceEnd should still be 1 after second ForceEnd, got %d", atomic.LoadInt32(&endCalled))
	}
}

func TestForceEndGrace_StateManager(t *testing.T) {
	sm := NewStateManager(5*time.Second, 5*time.Second)
	defer sm.Shutdown()

	sm.StartGracePeriod([]uint64{100, 200})

	if !sm.IsInGrace() {
		t.Fatal("Should be in grace period")
	}

	sm.ForceEndGrace()

	time.Sleep(10 * time.Millisecond)

	if sm.IsInGrace() {
		t.Error("Should not be in grace after ForceEndGrace")
	}
}

// ============================================================================
// ReclaimComplete Tests
// ============================================================================

func TestReclaimComplete(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		gp := NewGracePeriodState(5*time.Second, nil)
		defer gp.Stop()

		gp.StartGrace([]uint64{100, 200, 300})

		err := gp.ReclaimComplete(100)
		if err != nil {
			t.Fatalf("ReclaimComplete: %v", err)
		}
	})

	t.Run("complete_already", func(t *testing.T) {
		gp := NewGracePeriodState(5*time.Second, nil)
		defer gp.Stop()

		gp.StartGrace([]uint64{100, 200})

		// First call succeeds
		if err := gp.ReclaimComplete(100); err != nil {
			t.Fatalf("First ReclaimComplete: %v", err)
		}

		// Second call returns NFS4ERR_COMPLETE_ALREADY
		err := gp.ReclaimComplete(100)
		if err == nil {
			t.Fatal("Expected NFS4ERR_COMPLETE_ALREADY error")
		}
		stateErr, ok := err.(*NFS4StateError)
		if !ok {
			t.Fatalf("Expected NFS4StateError, got %T", err)
		}
		if stateErr.Status != types.NFS4ERR_COMPLETE_ALREADY {
			t.Errorf("Status = %d, want NFS4ERR_COMPLETE_ALREADY (%d)",
				stateErr.Status, types.NFS4ERR_COMPLETE_ALREADY)
		}
	})

	t.Run("outside_grace", func(t *testing.T) {
		gp := NewGracePeriodState(5*time.Second, nil)
		defer gp.Stop()

		// Not in grace period
		err := gp.ReclaimComplete(100)
		if err != nil {
			t.Fatalf("ReclaimComplete outside grace should return nil, got: %v", err)
		}
	})

	t.Run("all_clients_reclaim", func(t *testing.T) {
		gp := NewGracePeriodState(5*time.Second, nil)
		defer gp.Stop()

		gp.StartGrace([]uint64{100, 200, 300})

		// Each client sends ReclaimComplete
		for _, id := range []uint64{100, 200, 300} {
			if err := gp.ReclaimComplete(id); err != nil {
				t.Fatalf("ReclaimComplete(%d): %v", id, err)
			}
		}

		// Allow early exit to propagate
		time.Sleep(20 * time.Millisecond)

		if gp.IsInGrace() {
			t.Error("Grace period should end when all clients complete reclaim")
		}
	})
}

func TestReclaimComplete_StateManager(t *testing.T) {
	sm := NewStateManager(5*time.Second, 5*time.Second)
	defer sm.Shutdown()

	// ReclaimComplete without grace period is OK
	err := sm.ReclaimComplete(100)
	if err != nil {
		t.Fatalf("ReclaimComplete without grace: %v", err)
	}

	// Start grace period
	sm.StartGracePeriod([]uint64{100, 200})

	err = sm.ReclaimComplete(100)
	if err != nil {
		t.Fatalf("ReclaimComplete during grace: %v", err)
	}

	// Second call for same client
	err = sm.ReclaimComplete(100)
	if err == nil {
		t.Fatal("Expected NFS4ERR_COMPLETE_ALREADY")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("Expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_COMPLETE_ALREADY {
		t.Errorf("Status = %d, want NFS4ERR_COMPLETE_ALREADY", stateErr.Status)
	}
}

func TestGraceStatus_StateManager(t *testing.T) {
	sm := NewStateManager(5*time.Second, 5*time.Second)
	defer sm.Shutdown()

	// No grace period configured yet
	status := sm.GraceStatus()
	if status.Active {
		t.Error("Should not be active without grace period")
	}

	// Start grace period
	sm.StartGracePeriod([]uint64{100, 200})

	status = sm.GraceStatus()
	if !status.Active {
		t.Error("Should be active after StartGracePeriod")
	}
	if status.ExpectedClients != 2 {
		t.Errorf("ExpectedClients = %d, want 2", status.ExpectedClients)
	}
}
