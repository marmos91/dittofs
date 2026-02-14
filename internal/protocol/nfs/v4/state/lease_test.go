package state

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
)

// ============================================================================
// LeaseState Unit Tests
// ============================================================================

func TestLeaseRenewal(t *testing.T) {
	var expired int32
	onExpire := func(clientID uint64) {
		atomic.AddInt32(&expired, 1)
	}

	ls := NewLeaseState(1, 200*time.Millisecond, onExpire)
	defer ls.Stop()

	beforeRenew := ls.LastRenew
	time.Sleep(10 * time.Millisecond)

	ls.Renew()

	if !ls.LastRenew.After(beforeRenew) {
		t.Error("Renew() should update LastRenew timestamp")
	}
}

func TestLeaseExpiration(t *testing.T) {
	var expired int32
	var expiredClientID uint64
	onExpire := func(clientID uint64) {
		atomic.StoreUint64(&expiredClientID, clientID)
		atomic.AddInt32(&expired, 1)
	}

	ls := NewLeaseState(42, 50*time.Millisecond, onExpire)
	defer ls.Stop()

	// Wait for expiration
	time.Sleep(150 * time.Millisecond)

	if atomic.LoadInt32(&expired) != 1 {
		t.Errorf("onExpire should have been called once, got %d", atomic.LoadInt32(&expired))
	}
	if atomic.LoadUint64(&expiredClientID) != 42 {
		t.Errorf("expired clientID = %d, want 42", atomic.LoadUint64(&expiredClientID))
	}
}

func TestLeaseExpiration_CleansUpAllState(t *testing.T) {
	sm := NewStateManager(50 * time.Millisecond)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	// Create and confirm a client
	result, err := sm.SetClientID("test-client-cleanup", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	clientID := result.ClientID

	err = sm.ConfirmClientID(clientID, result.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	// Open a file to create state
	openResult, err := sm.OpenFile(clientID, []byte("owner1"), 1,
		[]byte("fh-cleanup-test"),
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	// Confirm the open
	_, err = sm.ConfirmOpen(&openResult.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}

	// Verify state exists
	if sm.GetClient(clientID) == nil {
		t.Fatal("client record should exist before expiry")
	}
	if sm.GetOpenState(openResult.Stateid.Other) == nil {
		t.Fatal("open state should exist before expiry")
	}

	// Wait for lease to expire and trigger cleanup
	time.Sleep(200 * time.Millisecond)

	// Verify all state was cleaned up
	if sm.GetClient(clientID) != nil {
		t.Error("client record should be removed after lease expiry")
	}
	if sm.GetOpenState(openResult.Stateid.Other) != nil {
		t.Error("open state should be removed after lease expiry")
	}

	// Verify open owner was removed
	sm.mu.RLock()
	ownerKey := makeOwnerKey(clientID, []byte("owner1"))
	_, ownerExists := sm.openOwners[ownerKey]
	sm.mu.RUnlock()
	if ownerExists {
		t.Error("open owner should be removed after lease expiry")
	}
}

func TestLeaseRenewal_PreventsExpiry(t *testing.T) {
	var expired int32
	onExpire := func(clientID uint64) {
		atomic.AddInt32(&expired, 1)
	}

	ls := NewLeaseState(1, 100*time.Millisecond, onExpire)
	defer ls.Stop()

	// Renew multiple times before expiry
	for i := 0; i < 5; i++ {
		time.Sleep(50 * time.Millisecond)
		ls.Renew()
	}

	// Wait a bit more (but not long enough for a fresh expiry)
	time.Sleep(50 * time.Millisecond)

	if atomic.LoadInt32(&expired) != 0 {
		t.Errorf("onExpire should NOT have been called, got %d", atomic.LoadInt32(&expired))
	}
}

func TestRenewLease_StaleClientID(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	err := sm.RenewLease(99999)
	if err == nil {
		t.Fatal("RenewLease should fail for unknown client")
	}
	if err != ErrStaleClientID {
		t.Errorf("expected ErrStaleClientID, got %v", err)
	}
}

func TestRenewLease_ValidClient(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	// Create and confirm a client
	result, err := sm.SetClientID("client-renew-lease", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	err = sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	// Renew should succeed
	err = sm.RenewLease(result.ClientID)
	if err != nil {
		t.Fatalf("RenewLease: %v", err)
	}

	// Verify client still exists
	record := sm.GetClient(result.ClientID)
	if record == nil {
		t.Fatal("client record not found after renewal")
	}
	if record.LastRenewal.IsZero() {
		t.Error("LastRenewal should be set after RenewLease")
	}
}

func TestImplicitRenewal_ViaValidateStateid(t *testing.T) {
	sm := NewStateManager(200 * time.Millisecond)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	// Create and confirm a client
	result, err := sm.SetClientID("client-implicit", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	err = sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	// Open a file
	openResult, err := sm.OpenFile(result.ClientID, []byte("owner1"), 1,
		[]byte("fh-implicit-renew"),
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	// Confirm the open
	confirmedStateid, err := sm.ConfirmOpen(&openResult.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}

	// Record the lease LastRenew before validation
	record := sm.GetClient(result.ClientID)
	if record == nil || record.Lease == nil {
		t.Fatal("client should have a lease")
	}
	beforeRenew := record.Lease.LastRenew

	time.Sleep(20 * time.Millisecond)

	// ValidateStateid should implicitly renew the lease
	openState, err := sm.ValidateStateid(confirmedStateid, []byte("fh-implicit-renew"))
	if err != nil {
		t.Fatalf("ValidateStateid: %v", err)
	}
	if openState == nil {
		t.Fatal("openState should not be nil")
	}

	// Verify the lease was renewed
	if !record.Lease.LastRenew.After(beforeRenew) {
		t.Error("ValidateStateid should have implicitly renewed the lease")
	}
}

func TestLeaseExpired_ReturnsError(t *testing.T) {
	sm := NewStateManager(50 * time.Millisecond)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	// Create and confirm a client
	result, err := sm.SetClientID("client-expired", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	err = sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	// Open a file
	openResult, err := sm.OpenFile(result.ClientID, []byte("owner1"), 1,
		[]byte("fh-expired"),
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	// Confirm the open
	confirmedStateid, err := sm.ConfirmOpen(&openResult.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}

	// Stop the lease timer to prevent cleanup callback from removing state
	// (we want to test the ValidateStateid check, not the cleanup)
	record := sm.GetClient(result.ClientID)
	if record != nil && record.Lease != nil {
		record.Lease.Stop()
	}

	// Wait for the lease duration to elapse (making IsExpired() true)
	time.Sleep(100 * time.Millisecond)

	// ValidateStateid should return NFS4ERR_EXPIRED
	_, err = sm.ValidateStateid(confirmedStateid, []byte("fh-expired"))
	if err == nil {
		t.Fatal("ValidateStateid should fail for expired lease")
	}

	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T: %v", err, err)
	}
	if stateErr.Status != types.NFS4ERR_EXPIRED {
		t.Errorf("status = %d, want NFS4ERR_EXPIRED (%d)",
			stateErr.Status, types.NFS4ERR_EXPIRED)
	}
}

func TestShutdown_StopsTimers(t *testing.T) {
	sm := NewStateManager(5 * time.Second)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	// Create and confirm multiple clients
	for i := 0; i < 3; i++ {
		clientIDStr := "shutdown-client-" + string(rune('A'+i))
		result, err := sm.SetClientID(clientIDStr, verifier, callback, "10.0.0.1:1234")
		if err != nil {
			t.Fatalf("SetClientID %d: %v", i, err)
		}
		err = sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier)
		if err != nil {
			t.Fatalf("ConfirmClientID %d: %v", i, err)
		}
	}

	// Shutdown should stop all lease timers without panic
	sm.Shutdown()

	// Verify all leases are stopped
	sm.mu.RLock()
	for _, record := range sm.clientsByID {
		if record.Lease != nil && !record.Lease.stopped {
			t.Errorf("lease for client %d should be stopped after Shutdown", record.ClientID)
		}
	}
	sm.mu.RUnlock()
}

func TestConcurrentRenew(t *testing.T) {
	var expired int32
	onExpire := func(clientID uint64) {
		atomic.AddInt32(&expired, 1)
	}

	ls := NewLeaseState(1, 200*time.Millisecond, onExpire)
	defer ls.Stop()

	const numGoroutines = 50
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				ls.Renew()
				time.Sleep(time.Millisecond)
			}
		}()
	}

	wg.Wait()

	// After all renewals, the lease should NOT have expired
	if atomic.LoadInt32(&expired) != 0 {
		t.Error("lease should not have expired during concurrent renewals")
	}

	// Verify IsExpired returns false
	if ls.IsExpired() {
		t.Error("lease should not be expired after concurrent renewals")
	}
}

func TestLeaseRemainingTime(t *testing.T) {
	ls := NewLeaseState(1, 1*time.Second, nil)
	defer ls.Stop()

	remaining := ls.RemainingTime()
	if remaining <= 0 || remaining > 1*time.Second {
		t.Errorf("RemainingTime = %v, expected (0, 1s]", remaining)
	}

	// After some time, remaining should decrease
	time.Sleep(100 * time.Millisecond)
	remaining2 := ls.RemainingTime()
	if remaining2 >= remaining {
		t.Errorf("RemainingTime should decrease: %v >= %v", remaining2, remaining)
	}
}

func TestLeaseIsExpired(t *testing.T) {
	ls := NewLeaseState(1, 50*time.Millisecond, nil)
	defer ls.Stop()

	if ls.IsExpired() {
		t.Error("new lease should not be expired")
	}

	time.Sleep(100 * time.Millisecond)

	if !ls.IsExpired() {
		t.Error("lease should be expired after duration")
	}
}

func TestLeaseStop(t *testing.T) {
	var expired int32
	onExpire := func(clientID uint64) {
		atomic.AddInt32(&expired, 1)
	}

	ls := NewLeaseState(1, 50*time.Millisecond, onExpire)
	ls.Stop()

	// Wait past the expiry time
	time.Sleep(150 * time.Millisecond)

	// The callback should NOT have fired because we stopped the timer
	if atomic.LoadInt32(&expired) != 0 {
		t.Error("onExpire should NOT have been called after Stop()")
	}
}

func TestLeaseRenewAfterStop(t *testing.T) {
	ls := NewLeaseState(1, 1*time.Second, nil)
	ls.Stop()

	// Renew after stop should not panic
	ls.Renew()
}
