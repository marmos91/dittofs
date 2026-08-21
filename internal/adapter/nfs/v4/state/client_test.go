package state

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// ============================================================================
// SETCLIENTID Tests (Five-Case Algorithm)
// ============================================================================

func TestSetClientID_NewClient(t *testing.T) {
	// Case 1: No confirmed, no unconfirmed -> create new unconfirmed record
	sm := NewStateManager(90 * time.Second)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	result, err := sm.SetClientID("client-1", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID failed: %v", err)
	}

	if result.ClientID == 0 {
		t.Error("expected non-zero client ID")
	}

	// Should be unconfirmed
	record := sm.GetClient(result.ClientID)
	if record == nil {
		t.Fatal("client record not found after SetClientID")
	}
	if record.Confirmed {
		t.Error("new client should be unconfirmed")
	}
	if record.ClientIDString != "client-1" {
		t.Errorf("ClientIDString = %q, want %q", record.ClientIDString, "client-1")
	}
	if record.Verifier != verifier {
		t.Error("verifier mismatch")
	}
	if record.Callback.Program != 0x40000000 {
		t.Errorf("callback program = %d, want %d", record.Callback.Program, 0x40000000)
	}
	if record.ClientAddr != "10.0.0.1:1234" {
		t.Errorf("ClientAddr = %q, want %q", record.ClientAddr, "10.0.0.1:1234")
	}
}

func TestSetClientID_ClientReboot(t *testing.T) {
	// Case 3: Confirmed exists, different verifier -> client reboot
	sm := NewStateManager(90 * time.Second)

	verifier1 := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	verifier2 := [8]byte{9, 10, 11, 12, 13, 14, 15, 16}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	// First: create and confirm client
	result1, err := sm.SetClientID("client-reboot", verifier1, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("first SetClientID failed: %v", err)
	}
	err = sm.ConfirmClientID(result1.ClientID, result1.ConfirmVerifier)
	if err != nil {
		t.Fatalf("first ConfirmClientID failed: %v", err)
	}

	// Verify client is confirmed
	record := sm.GetClient(result1.ClientID)
	if record == nil || !record.Confirmed {
		t.Fatal("client should be confirmed after SETCLIENTID_CONFIRM")
	}

	// Second: SETCLIENTID with different verifier (simulating reboot)
	result2, err := sm.SetClientID("client-reboot", verifier2, callback, "10.0.0.1:5678")
	if err != nil {
		t.Fatalf("second SetClientID failed: %v", err)
	}

	// Should get a NEW client ID (reboot generates new ID)
	if result2.ClientID == result1.ClientID {
		t.Error("client reboot should generate a new client ID")
	}

	// Old confirmed record should still exist until new one is confirmed
	oldRecord := sm.GetClient(result1.ClientID)
	if oldRecord == nil {
		t.Error("old confirmed record should still exist before new confirmation")
	}

	// New record should be unconfirmed
	newRecord := sm.GetClient(result2.ClientID)
	if newRecord == nil {
		t.Fatal("new record not found")
	}
	if newRecord.Confirmed {
		t.Error("new record should be unconfirmed")
	}
	if newRecord.Verifier != verifier2 {
		t.Error("new record should have new verifier")
	}
}

func TestSetClientID_SameVerifier(t *testing.T) {
	// Case 5: Confirmed exists, same verifier -> re-SETCLIENTID (callback update)
	sm := NewStateManager(90 * time.Second)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback1 := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}
	callback2 := CallbackInfo{Program: 0x40000001, NetID: "tcp", Addr: "10.0.0.2.8.1"}

	// Create and confirm client
	result1, err := sm.SetClientID("client-same-verf", verifier, callback1, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("first SetClientID failed: %v", err)
	}
	err = sm.ConfirmClientID(result1.ClientID, result1.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID failed: %v", err)
	}

	// Re-SETCLIENTID with same verifier but different callback
	result2, err := sm.SetClientID("client-same-verf", verifier, callback2, "10.0.0.2:5678")
	if err != nil {
		t.Fatalf("second SetClientID failed: %v", err)
	}

	// Should reuse the same client ID
	if result2.ClientID != result1.ClientID {
		t.Errorf("re-SETCLIENTID should reuse client ID: got %d, want %d",
			result2.ClientID, result1.ClientID)
	}

	// Should get a new confirm verifier (different from original)
	if result2.ConfirmVerifier == result1.ConfirmVerifier {
		t.Error("re-SETCLIENTID should generate a new confirm verifier")
	}
}

func TestSetClientID_ReplaceUnconfirmed(t *testing.T) {
	// Case 4: No confirmed, unconfirmed exists -> replace unconfirmed
	sm := NewStateManager(90 * time.Second)

	verifier1 := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	verifier2 := [8]byte{9, 10, 11, 12, 13, 14, 15, 16}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	// First SETCLIENTID (creates unconfirmed)
	result1, err := sm.SetClientID("client-replace", verifier1, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("first SetClientID failed: %v", err)
	}

	// Second SETCLIENTID without confirming first (replaces unconfirmed)
	result2, err := sm.SetClientID("client-replace", verifier2, callback, "10.0.0.1:5678")
	if err != nil {
		t.Fatalf("second SetClientID failed: %v", err)
	}

	// Should get a different client ID (old unconfirmed replaced)
	if result2.ClientID == result1.ClientID {
		t.Error("replacing unconfirmed should generate a new client ID")
	}

	// Old client ID should no longer exist
	if sm.GetClient(result1.ClientID) != nil {
		t.Error("old unconfirmed record should have been removed")
	}

	// New client ID should exist and be unconfirmed
	record := sm.GetClient(result2.ClientID)
	if record == nil {
		t.Fatal("new record not found")
	}
	if record.Confirmed {
		t.Error("replaced record should be unconfirmed")
	}
	if record.Verifier != verifier2 {
		t.Error("replaced record should have new verifier")
	}
}

func TestSetClientID_ConfirmedAndUnconfirmed(t *testing.T) {
	// Case 2: Confirmed exists + unconfirmed exists -> replace unconfirmed
	sm := NewStateManager(90 * time.Second)

	verifier1 := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	verifier2 := [8]byte{9, 10, 11, 12, 13, 14, 15, 16}
	verifier3 := [8]byte{17, 18, 19, 20, 21, 22, 23, 24}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	// Create and confirm client
	result1, err := sm.SetClientID("client-both", verifier1, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("first SetClientID failed: %v", err)
	}
	err = sm.ConfirmClientID(result1.ClientID, result1.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID failed: %v", err)
	}

	// Create unconfirmed with reboot verifier (Case 3 creates unconfirmed)
	result2, err := sm.SetClientID("client-both", verifier2, callback, "10.0.0.1:5678")
	if err != nil {
		t.Fatalf("second SetClientID failed: %v", err)
	}

	// Now another SETCLIENTID comes in while both exist (Case 2)
	result3, err := sm.SetClientID("client-both", verifier3, callback, "10.0.0.1:9012")
	if err != nil {
		t.Fatalf("third SetClientID failed: %v", err)
	}

	// Old unconfirmed should be gone
	if sm.GetClient(result2.ClientID) != nil {
		t.Error("old unconfirmed should have been replaced")
	}

	// Confirmed record should still exist
	if sm.GetClient(result1.ClientID) == nil {
		t.Error("confirmed record should still exist")
	}

	// New unconfirmed should exist
	record := sm.GetClient(result3.ClientID)
	if record == nil {
		t.Fatal("new record not found")
	}
	if record.Confirmed {
		t.Error("new record should be unconfirmed")
	}
}

// ============================================================================
// SETCLIENTID_CONFIRM Tests
// ============================================================================

func TestConfirmClientID_Success(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	result, err := sm.SetClientID("client-confirm", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID failed: %v", err)
	}

	err = sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID failed: %v", err)
	}

	record := sm.GetClient(result.ClientID)
	if record == nil {
		t.Fatal("client record not found after confirm")
	}
	if !record.Confirmed {
		t.Error("client should be confirmed")
	}
}

func TestConfirmClientID_WrongVerifier(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	result, err := sm.SetClientID("client-wrong-verf", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID failed: %v", err)
	}

	// Try to confirm with wrong verifier
	wrongVerf := [8]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	err = sm.ConfirmClientID(result.ClientID, wrongVerf)
	if err == nil {
		t.Fatal("ConfirmClientID should fail with wrong verifier")
	}

	// Client should still be unconfirmed
	record := sm.GetClient(result.ClientID)
	if record == nil {
		t.Fatal("client record should still exist")
	}
	if record.Confirmed {
		t.Error("client should NOT be confirmed with wrong verifier")
	}
}

func TestConfirmClientID_StaleClientID(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Try to confirm a non-existent client ID
	verf := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	err := sm.ConfirmClientID(99999, verf)
	if err == nil {
		t.Fatal("ConfirmClientID should fail for unknown client ID")
	}
}

func TestConfirmClientID_AfterReboot(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	verifier1 := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	verifier2 := [8]byte{9, 10, 11, 12, 13, 14, 15, 16}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	// Create and confirm first client
	result1, err := sm.SetClientID("client-reboot-confirm", verifier1, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("first SetClientID failed: %v", err)
	}
	err = sm.ConfirmClientID(result1.ClientID, result1.ConfirmVerifier)
	if err != nil {
		t.Fatalf("first ConfirmClientID failed: %v", err)
	}

	// Client reboots (different verifier, Case 3)
	result2, err := sm.SetClientID("client-reboot-confirm", verifier2, callback, "10.0.0.1:5678")
	if err != nil {
		t.Fatalf("second SetClientID failed: %v", err)
	}

	// Confirm the new record
	err = sm.ConfirmClientID(result2.ClientID, result2.ConfirmVerifier)
	if err != nil {
		t.Fatalf("second ConfirmClientID failed: %v", err)
	}

	// New record should be confirmed
	newRecord := sm.GetClient(result2.ClientID)
	if newRecord == nil || !newRecord.Confirmed {
		t.Error("new record should be confirmed")
	}

	// Old confirmed record should be replaced (removed)
	oldRecord := sm.GetClient(result1.ClientID)
	if oldRecord != nil {
		t.Error("old confirmed record should have been replaced")
	}
}

// ============================================================================
// Client ID Generation Tests
// ============================================================================

func TestClientIDUniqueness(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}
	seen := make(map[uint64]bool)

	for i := 0; i < 100; i++ {
		verifier := [8]byte{byte(i)}
		name := "client-" + string(rune('A'+i%26)) + string(rune('0'+i/26))
		result, err := sm.SetClientID(name, verifier, callback, "10.0.0.1:1234")
		if err != nil {
			t.Fatalf("SetClientID #%d failed: %v", i, err)
		}
		if seen[result.ClientID] {
			t.Fatalf("duplicate client ID %d at iteration %d", result.ClientID, i)
		}
		seen[result.ClientID] = true
	}
}

func TestConfirmVerifierUnpredictable(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}
	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}

	result1, err := sm.SetClientID("client-verf-1", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("first SetClientID failed: %v", err)
	}

	result2, err := sm.SetClientID("client-verf-2", verifier, callback, "10.0.0.1:5678")
	if err != nil {
		t.Fatalf("second SetClientID failed: %v", err)
	}

	if result1.ConfirmVerifier == result2.ConfirmVerifier {
		t.Error("two different clients should get different confirm verifiers")
	}

	// Also check that verifiers are not all zeros
	allZeros := true
	for _, b := range result1.ConfirmVerifier {
		if b != 0 {
			allZeros = false
			break
		}
	}
	if allZeros {
		t.Error("confirm verifier should not be all zeros (should be crypto/rand generated)")
	}
}

func TestGenerateClientID_BootEpoch(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	result, err := sm.SetClientID("client-epoch", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID failed: %v", err)
	}

	// High 32 bits should be the boot epoch
	highBits := uint32(result.ClientID >> 32)
	if highBits != sm.BootEpoch() {
		t.Errorf("client ID high 32 bits = %d, want boot epoch %d", highBits, sm.BootEpoch())
	}

	// Low 32 bits should be non-zero (counter starts at 1)
	lowBits := uint32(result.ClientID & 0xFFFFFFFF)
	if lowBits == 0 {
		t.Error("client ID low 32 bits should be non-zero (counter)")
	}
}

// ============================================================================
// Concurrency Test
// ============================================================================

func TestConcurrentSetClientID(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	const numGoroutines = 50
	var wg sync.WaitGroup
	results := make([]*SetClientIDResult, numGoroutines)
	errors := make([]error, numGoroutines)

	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			verifier := [8]byte{byte(idx)}
			name := "concurrent-client-" + string(rune('A'+idx%26))
			// Use different names so they don't conflict
			if idx >= 26 {
				name = name + string(rune('0'+idx/26))
			}
			results[idx], errors[idx] = sm.SetClientID(name, verifier, callback, "10.0.0.1:1234")
		}(i)
	}

	wg.Wait()

	// All should succeed (different client names)
	for i := 0; i < numGoroutines; i++ {
		if errors[i] != nil {
			t.Errorf("goroutine %d failed: %v", i, errors[i])
		}
		if results[i] == nil {
			t.Errorf("goroutine %d returned nil result", i)
			continue
		}
	}

	// All client IDs should be unique
	seen := make(map[uint64]bool)
	for i := 0; i < numGoroutines; i++ {
		if results[i] == nil {
			continue
		}
		if seen[results[i].ClientID] {
			t.Errorf("duplicate client ID %d from goroutine %d", results[i].ClientID, i)
		}
		seen[results[i].ClientID] = true
	}
}

// ============================================================================
// VerifierMatches Test
// ============================================================================

func TestVerifierMatches(t *testing.T) {
	record := &ClientRecord{
		Verifier: [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
	}

	same := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	different := [8]byte{9, 10, 11, 12, 13, 14, 15, 16}

	if !record.VerifierMatches(same) {
		t.Error("VerifierMatches should return true for matching verifier")
	}
	if record.VerifierMatches(different) {
		t.Error("VerifierMatches should return false for different verifier")
	}
}

// ============================================================================
// RemoveClient Test
// ============================================================================

func TestRemoveClient(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	// Create and confirm
	result, err := sm.SetClientID("client-remove", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID failed: %v", err)
	}
	err = sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID failed: %v", err)
	}

	// Remove
	sm.RemoveClient(result.ClientID)

	// Should be gone
	if sm.GetClient(result.ClientID) != nil {
		t.Error("client record should have been removed")
	}

	// Double remove should not panic
	sm.RemoveClient(result.ClientID)
}

// ============================================================================
// OPEN Client ID Validation Tests
// ============================================================================

// OPEN is a locking request, so the clientid it carries must name a client that
// completed its registration handshake and still holds a lease (RFC 7530
// Sections 9.1.1, 13.1.10.2 and 9.6.3.2). These tests cover each way that can
// fail, plus the consequences of accepting one: state under a clientid with no
// lease is never reaped, and its share reservation would block clients that did
// register.

// expectStatus asserts that err carries the given NFS4ERR_* status.
func expectStatus(t *testing.T, err error, want uint32) {
	t.Helper()

	if err == nil {
		t.Fatalf("expected error with status %d, got nil", want)
	}
	var stateErr *NFS4StateError
	if !errors.As(err, &stateErr) {
		t.Fatalf("expected *NFS4StateError, got %T: %v", err, err)
	}
	if stateErr.Status != want {
		t.Fatalf("status = %d, want %d", stateErr.Status, want)
	}
}

func TestValidateAndRenewClient_UnknownClientID_Rejected(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	// A clientid the server has never issued: no SETCLIENTID, no
	// SETCLIENTID_CONFIRM, no lease.
	const unknown uint64 = 0xDEADBEEFCAFEF00D

	if err := sm.ValidateAndRenewClient(unknown); !errors.Is(err, ErrStaleClientID) {
		t.Fatalf("admitting an unknown clientid = %v, want ErrStaleClientID", err)
	}
}

func TestValidateAndRenewClient_UnconfirmedClientID_Rejected(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	// SETCLIENTID only: the identity is not established until
	// SETCLIENTID_CONFIRM (RFC 7530 Section 9.1.1).
	res, err := sm.SetClientID("unconfirmed-client", [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
		CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}

	if err := sm.ValidateAndRenewClient(res.ClientID); !errors.Is(err, ErrStaleClientID) {
		t.Fatalf("admitting a clientid before SETCLIENTID_CONFIRM = %v, want ErrStaleClientID", err)
	}
}

// A v4.0 client stops sending RENEW once it holds no open state, so a stretch
// of stateid-free work — LOOKUP, GETATTR, SETATTR, CREATE, REMOVE — leaves the
// lease untouched until the next OPEN. OPEN must therefore renew it, the way
// RFC 7530 Section 9.6.2 counts any operation carrying a valid clientid as
// proof of liveness; otherwise a busy mount is reaped as an idle one and every
// create after the lease duration fails.
func TestValidateAndRenewClient_KeepsBusyClientAlive(t *testing.T) {
	const lease = 200 * time.Millisecond
	sm := NewStateManager(lease)
	defer sm.Shutdown()

	clientID := registerTestClientWithName(t, sm, "busy-client")

	// Three admissions spread over more than one lease duration, with no RENEW
	// and no stateid op in between. Each renews, so the last lands on a client
	// established well over a lease ago and still live.
	for i := range 3 {
		time.Sleep(lease * 2 / 3)
		if err := sm.ValidateAndRenewClient(clientID); err != nil {
			t.Fatalf("ValidateAndRenewClient %d: %v", i+1, err)
		}
	}

	if record := sm.GetClient(clientID); record == nil {
		t.Fatal("client record was reaped despite a steady stream of OPENs")
	} else if record.Lease.IsExpired() {
		t.Fatal("lease expired despite a steady stream of OPENs")
	}
}

// A v4.1 lease has no expiry callback: the record stays on file with a stale
// lease until the session reaper sweeps it. While it is still there, OPEN names
// what happened to the client's state (RFC 7530 Section 9.6.3.2) rather than
// reporting the ID as never-seen.
func TestValidateAndRenewClient_V41LapsedLeaseBeforeReaping_ReturnsExpired(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	clientID := registerTestV41Client(t, sm)

	// Age the lease past its duration without running the reaper, which is the
	// state a v4.1 client is in for up to one reaper interval.
	sm.mu.RLock()
	lease := sm.v41ClientsByID[clientID].Lease
	sm.mu.RUnlock()
	lease.mu.Lock()
	lease.LastRenew = time.Now().Add(-2 * lease.Duration)
	lease.mu.Unlock()

	expectStatus(t, sm.ValidateAndRenewClient(clientID), types.NFS4ERR_EXPIRED)
}

// v4.1 clients are registered by EXCHANGE_ID / CREATE_SESSION and live in a
// separate table; validation must accept them.
func TestValidateAndRenewClient_V41Client_Accepted(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	clientID := registerTestV41Client(t, sm)

	if err := sm.ValidateAndRenewClient(clientID); err != nil {
		t.Fatalf("admitting a v4.1 session client: %v", err)
	}
}

// ============================================================================
// NewStateManager Tests
// ============================================================================

func TestNewStateManager_DefaultLease(t *testing.T) {
	sm := NewStateManager(0) // should use default
	if sm.LeaseDuration() != DefaultLeaseDuration {
		t.Errorf("LeaseDuration = %v, want %v", sm.LeaseDuration(), DefaultLeaseDuration)
	}
}

func TestNewStateManager_CustomLease(t *testing.T) {
	sm := NewStateManager(30 * time.Second)
	if sm.LeaseDuration() != 30*time.Second {
		t.Errorf("LeaseDuration = %v, want %v", sm.LeaseDuration(), 30*time.Second)
	}
}

func TestNewStateManager_BootEpochReasonable(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	now := uint32(time.Now().Unix())
	// Boot epoch should be within 2 seconds of current time
	diff := int64(now) - int64(sm.BootEpoch())
	if diff < 0 {
		diff = -diff
	}
	if diff > 2 {
		t.Errorf("BootEpoch %d is too far from current time %d", sm.BootEpoch(), now)
	}
}
