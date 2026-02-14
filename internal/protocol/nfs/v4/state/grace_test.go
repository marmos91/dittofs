package state

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
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
