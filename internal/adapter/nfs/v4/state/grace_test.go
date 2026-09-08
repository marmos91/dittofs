package state

import (
	"errors"
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

// newReclaimClient registers a confirmed v4.1 client and returns its client ID.
func newReclaimClient(t *testing.T, sm *StateManager, ownerID string) uint64 {
	t.Helper()
	exch, err := sm.ExchangeID([]byte(ownerID), [8]byte{0x1}, 0, nil, "10.0.0.1:1")
	if err != nil {
		t.Fatalf("ExchangeID(%q): %v", ownerID, err)
	}
	if _, _, err := sm.CreateSession(
		exch.ClientID, exch.SequenceID, 0, defaultForeAttrs(), defaultBackAttrs(), 0, nil,
	); err != nil {
		t.Fatalf("CreateSession(%q): %v", ownerID, err)
	}
	return exch.ClientID
}

// wantCompleteAlready fails unless err carries NFS4ERR_COMPLETE_ALREADY.
func wantCompleteAlready(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrCompleteAlready) {
		t.Fatalf("second ReclaimComplete err = %v, want ErrCompleteAlready", err)
	}
}

func TestReclaimComplete_StateManager(t *testing.T) {
	t.Run("during_grace", func(t *testing.T) {
		sm := NewStateManager(5*time.Second, 5*time.Second)
		defer sm.Shutdown()

		clientID := newReclaimClient(t, sm, "in-grace")
		sm.StartGracePeriod([]uint64{clientID})

		if err := sm.ReclaimComplete(clientID); err != nil {
			t.Fatalf("ReclaimComplete during grace: %v", err)
		}
		wantCompleteAlready(t, sm.ReclaimComplete(clientID))

		// The first call must still retire the client from the roster.
		time.Sleep(20 * time.Millisecond)
		if sm.IsInGrace() {
			t.Error("grace should end early once the only expected client reclaims")
		}
	})

	t.Run("outside_grace", func(t *testing.T) {
		// No grace period is ever configured. The first RECLAIM_COMPLETE is
		// still not an error, and the second is still a duplicate.
		sm := NewStateManager(5*time.Second, 5*time.Second)
		defer sm.Shutdown()

		clientID := newReclaimClient(t, sm, "outside-grace")
		if err := sm.ReclaimComplete(clientID); err != nil {
			t.Fatalf("first ReclaimComplete outside grace: %v", err)
		}
		wantCompleteAlready(t, sm.ReclaimComplete(clientID))
	})

	t.Run("after_grace_ended", func(t *testing.T) {
		sm := NewStateManager(5*time.Second, 5*time.Second)
		defer sm.Shutdown()

		clientID := newReclaimClient(t, sm, "after-grace")
		sm.StartGracePeriod([]uint64{clientID, 999})
		sm.ForceEndGrace()

		if err := sm.ReclaimComplete(clientID); err != nil {
			t.Fatalf("first ReclaimComplete after grace ended: %v", err)
		}
		wantCompleteAlready(t, sm.ReclaimComplete(clientID))
	})

	t.Run("per_client", func(t *testing.T) {
		// One client's completion must not answer for another's.
		sm := NewStateManager(5*time.Second, 5*time.Second)
		defer sm.Shutdown()

		first := newReclaimClient(t, sm, "client-a")
		second := newReclaimClient(t, sm, "client-b")

		if err := sm.ReclaimComplete(first); err != nil {
			t.Fatalf("ReclaimComplete(first): %v", err)
		}
		if err := sm.ReclaimComplete(second); err != nil {
			t.Fatalf("ReclaimComplete(second) after first completed: %v", err)
		}
		wantCompleteAlready(t, sm.ReclaimComplete(first))
		wantCompleteAlready(t, sm.ReclaimComplete(second))
	})
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
