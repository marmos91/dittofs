package state

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
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
	confirmedRes, err := sm.ConfirmOpen(&openResult.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}
	confirmedStateid := &confirmedRes.Stateid

	// Record the lease LastRenew before validation
	record := sm.GetClient(result.ClientID)
	if record == nil || record.Lease == nil {
		t.Fatal("client should have a lease")
	}
	beforeRenew := record.Lease.LastRenew

	time.Sleep(20 * time.Millisecond)

	// ValidateStateid should implicitly renew the lease
	openState, err := sm.ValidateStateid(confirmedStateid, []byte("fh-implicit-renew"), StateidOpRead, 0)
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
	confirmedRes, err := sm.ConfirmOpen(&openResult.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}
	confirmedStateid := &confirmedRes.Stateid

	// Stop the lease timer to prevent cleanup callback from removing state
	// (we want to test the ValidateStateid check, not the cleanup)
	record := sm.GetClient(result.ClientID)
	if record != nil && record.Lease != nil {
		record.Lease.Stop()
	}

	// Wait for the lease duration to elapse (making IsExpired() true)
	time.Sleep(100 * time.Millisecond)

	// ValidateStateid should return NFS4ERR_EXPIRED
	_, err = sm.ValidateStateid(confirmedStateid, []byte("fh-expired"), StateidOpRead, 0)
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

// A re-SETCLIENTID reuses the client ID, so SETCLIENTID_CONFIRM must confirm the
// record that already owns that ID rather than installing a second one beside
// it. Two records under one ID leave whichever the maps do not point at holding
// a lease timer nothing renews, and when that timer fires it reaps the client on
// its original schedule no matter how often RENEW refreshed the live lease.
func TestRenewLease_SurvivesReSetClientID(t *testing.T) {
	const lease = time.Second

	sm := NewStateManager(lease)
	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}

	setClientID := func(port string) uint64 {
		t.Helper()
		callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8." + port}
		result, err := sm.SetClientID("client-resetclientid", verifier, callback, "10.0.0.1:1234")
		if err != nil {
			t.Fatalf("SetClientID: %v", err)
		}
		if err := sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier); err != nil {
			t.Fatalf("ConfirmClientID: %v", err)
		}
		return result.ClientID
	}

	clientID := setClientID("1")
	// Same verifier, new callback: RFC 7530 Case 5, which reuses the client ID.
	if got := setClientID("2"); got != clientID {
		t.Fatalf("re-SETCLIENTID returned client ID %d, want the reused %d", got, clientID)
	}

	// Renew inside the lease, then check the client is still live past the point
	// where the first confirm's timer was originally due to fire.
	for _, wait := range []time.Duration{lease * 6 / 10, lease * 6 / 10} {
		time.Sleep(wait)
		if err := sm.RenewLease(clientID); err != nil {
			t.Fatalf("RenewLease after %v: %v", wait, err)
		}
	}

	if sm.GetClient(clientID) == nil {
		t.Fatal("client was reaped despite being renewed within its lease")
	}
}

// A confirm carrying the wrong verifier must leave the live client exactly as it
// was. Confirming a re-SETCLIENTID writes through to the record that already
// owns the client ID, so validating after the write would let a stale retransmit
// of the previous confirm install an unconfirmed callback address on a client
// that is still using the old one, and then report failure.
func TestConfirmClientID_StaleRetransmitLeavesRecordIntact(t *testing.T) {
	sm := NewStateManager(time.Minute)
	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}

	setClientID := func(addr string) *SetClientIDResult {
		t.Helper()
		callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: addr}
		result, err := sm.SetClientID("client-stale-confirm", verifier, callback, "10.0.0.1:1234")
		if err != nil {
			t.Fatalf("SetClientID(%s): %v", addr, err)
		}
		return result
	}

	first := setClientID("10.0.0.1.8.1")
	if err := sm.ConfirmClientID(first.ClientID, first.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	// Same verifier, new callback: a re-SETCLIENTID, left pending.
	second := setClientID("10.0.0.1.8.2")

	// The first confirm arrives again while that one is pending.
	if err := sm.ConfirmClientID(first.ClientID, first.ConfirmVerifier); err == nil {
		t.Fatal("a confirm carrying the superseded verifier must be refused")
	}
	if got := sm.GetClient(first.ClientID).Callback.Addr; got != "10.0.0.1.8.1" {
		t.Fatalf("refused confirm changed the live callback to %q, want the confirmed 10.0.0.1.8.1", got)
	}

	// The real confirm still lands and installs the new callback.
	if err := sm.ConfirmClientID(second.ClientID, second.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID(re-SETCLIENTID): %v", err)
	}
	if got := sm.GetClient(second.ClientID).Callback.Addr; got != "10.0.0.1.8.2" {
		t.Fatalf("confirmed callback = %q, want 10.0.0.1.8.2", got)
	}
}

// cbProbeHarness drives SETCLIENTID_CONFIRM with CB_NULL held under test
// control, so a probe can be left in flight across a re-SETCLIENTID.
type cbProbeHarness struct {
	sm      *StateManager
	t       *testing.T
	probing chan string
	release map[string]chan error
}

func newCBProbeHarness(t *testing.T, addrs ...string) *cbProbeHarness {
	t.Helper()
	h := &cbProbeHarness{
		sm:      NewStateManager(time.Minute),
		t:       t,
		probing: make(chan string, 4),
		release: map[string]chan error{},
	}
	for _, a := range addrs {
		h.release[a] = make(chan error)
	}
	h.sm.cbNullFunc = func(_ context.Context, cb CallbackInfo) error {
		h.probing <- cb.Addr
		return <-h.release[cb.Addr]
	}
	return h
}

// confirm runs SETCLIENTID + SETCLIENTID_CONFIRM for addr and waits until its
// CB_NULL probe has started, so the probe is reliably in flight on return.
func (h *cbProbeHarness) confirm(addr string) uint64 {
	h.t.Helper()
	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	result, err := h.sm.SetClientID("client-cbpath", verifier,
		CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: addr}, "10.0.0.1:1234")
	if err != nil {
		h.t.Fatalf("SetClientID(%s): %v", addr, err)
	}
	if err := h.sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier); err != nil {
		h.t.Fatalf("ConfirmClientID(%s): %v", addr, err)
	}
	if got := <-h.probing; got != addr {
		h.t.Fatalf("probe went to %q, want %q", got, addr)
	}
	return result.ClientID
}

func (h *cbProbeHarness) cbPathUp(clientID uint64) bool {
	h.sm.mu.Lock()
	defer h.sm.mu.Unlock()
	return h.sm.clientsByID[clientID].CBPathUp
}

func (h *cbProbeHarness) waitCBPathUp(clientID uint64) {
	h.t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !h.cbPathUp(clientID); {
		if time.Now().After(deadline) {
			h.t.Fatal("CB_NULL success never enabled the callback path")
		}
	}
}

// Delegations are gated on CBPathUp. A re-SETCLIENTID can move the client to a
// new callback address, so confirming it must drop the verdict earned by the
// address it replaces — otherwise a delegation is granted in the window before
// the new address has been probed at all, and recalled somewhere this client is
// not listening.
func TestConfirmClientID_ReSetClientIDClearsCallbackVerdict(t *testing.T) {
	const addr1, addr2 = "10.0.0.1.8.1", "10.0.0.1.8.2"
	h := newCBProbeHarness(t, addr1, addr2)

	clientID := h.confirm(addr1)
	h.release[addr1] <- nil
	h.waitCBPathUp(clientID)

	if got := h.confirm(addr2); got != clientID {
		t.Fatal("re-SETCLIENTID returned a different client ID")
	}
	if h.cbPathUp(clientID) {
		t.Fatal("confirm kept the previous callback address's verdict for a new one")
	}
}

// CB_NULL runs asynchronously, so a re-SETCLIENTID can land while the previous
// address is still being probed. That probe's verdict is about an address the
// client no longer uses and must not vouch for the one that replaced it.
func TestConfirmClientID_StaleCallbackProbeDoesNotVouchForNewAddress(t *testing.T) {
	const addr1, addr2 = "10.0.0.1.8.1", "10.0.0.1.8.2"
	h := newCBProbeHarness(t, addr1, addr2)

	clientID := h.confirm(addr1)
	// Re-SETCLIENTID while the first probe is still in flight.
	if got := h.confirm(addr2); got != clientID {
		t.Fatal("re-SETCLIENTID returned a different client ID")
	}

	// The stale probe succeeds, about the replaced address.
	h.release[addr1] <- nil
	for deadline := time.Now().Add(500 * time.Millisecond); time.Now().Before(deadline); {
		if h.cbPathUp(clientID) {
			t.Fatal("a CB_NULL for the replaced address enabled the new one")
		}
	}

	// Only the new address's own probe may enable the callback path.
	h.release[addr2] <- nil
	h.waitCBPathUp(clientID)
}
