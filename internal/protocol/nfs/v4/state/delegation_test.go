package state

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
)

// ============================================================================
// GrantDelegation Tests
// ============================================================================

func TestGrantDelegation_Read(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	deleg := sm.GrantDelegation(100, []byte("fh-read-deleg"), types.OPEN_DELEGATE_READ)

	if deleg == nil {
		t.Fatal("GrantDelegation should return non-nil DelegationState")
	}
	if deleg.ClientID != 100 {
		t.Errorf("ClientID = %d, want 100", deleg.ClientID)
	}
	if deleg.DelegType != types.OPEN_DELEGATE_READ {
		t.Errorf("DelegType = %d, want OPEN_DELEGATE_READ (%d)", deleg.DelegType, types.OPEN_DELEGATE_READ)
	}
	if deleg.Stateid.Seqid != 1 {
		t.Errorf("Stateid.Seqid = %d, want 1", deleg.Stateid.Seqid)
	}

	// Verify stateid type tag is 0x03 (StateTypeDeleg)
	if deleg.Stateid.Other[0] != StateTypeDeleg {
		t.Errorf("Stateid.Other[0] = 0x%02x, want 0x%02x (StateTypeDeleg)", deleg.Stateid.Other[0], StateTypeDeleg)
	}

	// Verify stored in both maps
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if _, exists := sm.delegByOther[deleg.Stateid.Other]; !exists {
		t.Error("delegation should be in delegByOther map")
	}
	if delegs, exists := sm.delegByFile[string([]byte("fh-read-deleg"))]; !exists || len(delegs) != 1 {
		t.Error("delegation should be in delegByFile map")
	}
}

func TestGrantDelegation_Write(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	deleg := sm.GrantDelegation(200, []byte("fh-write-deleg"), types.OPEN_DELEGATE_WRITE)

	if deleg == nil {
		t.Fatal("GrantDelegation should return non-nil DelegationState")
	}
	if deleg.DelegType != types.OPEN_DELEGATE_WRITE {
		t.Errorf("DelegType = %d, want OPEN_DELEGATE_WRITE (%d)", deleg.DelegType, types.OPEN_DELEGATE_WRITE)
	}
	if deleg.RecallSent {
		t.Error("RecallSent should be false on fresh delegation")
	}
	if deleg.Revoked {
		t.Error("Revoked should be false on fresh delegation")
	}
}

// ============================================================================
// ReturnDelegation Tests
// ============================================================================

func TestReturnDelegation_Success(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	deleg := sm.GrantDelegation(100, []byte("fh-return-test"), types.OPEN_DELEGATE_READ)

	err := sm.ReturnDelegation(&deleg.Stateid)
	if err != nil {
		t.Fatalf("ReturnDelegation: %v", err)
	}

	// Verify maps are empty
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if _, exists := sm.delegByOther[deleg.Stateid.Other]; exists {
		t.Error("delegation should be removed from delegByOther")
	}
	if delegs, exists := sm.delegByFile[string([]byte("fh-return-test"))]; exists && len(delegs) > 0 {
		t.Error("delegation should be removed from delegByFile")
	}
}

func TestReturnDelegation_NotFound(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Create a stateid with current boot epoch but unknown
	other := sm.generateStateidOther(StateTypeDeleg)
	stateid := &types.Stateid4{Seqid: 1, Other: other}

	// Not found but current epoch: idempotent success (per Pitfall 3)
	err := sm.ReturnDelegation(stateid)
	if err != nil {
		t.Errorf("ReturnDelegation for unknown but current-epoch stateid should succeed, got %v", err)
	}
}

func TestReturnDelegation_StaleStateid(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Create a stateid with a different boot epoch
	var other [types.NFS4_OTHER_SIZE]byte
	other[0] = StateTypeDeleg
	other[1] = 0xFF // Different epoch
	other[2] = 0xFF
	other[3] = 0xFF

	stateid := &types.Stateid4{Seqid: 1, Other: other}

	err := sm.ReturnDelegation(stateid)
	if err == nil {
		t.Fatal("ReturnDelegation should fail for stale stateid")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_STALE_STATEID {
		t.Errorf("status = %d, want NFS4ERR_STALE_STATEID (%d)", stateErr.Status, types.NFS4ERR_STALE_STATEID)
	}
}

func TestReturnDelegation_Idempotent(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	deleg := sm.GrantDelegation(100, []byte("fh-idempotent"), types.OPEN_DELEGATE_READ)

	// First return
	err := sm.ReturnDelegation(&deleg.Stateid)
	if err != nil {
		t.Fatalf("first ReturnDelegation: %v", err)
	}

	// Second return (idempotent) -- should succeed
	err = sm.ReturnDelegation(&deleg.Stateid)
	if err != nil {
		t.Errorf("second ReturnDelegation should be idempotent, got %v", err)
	}
}

// ============================================================================
// GetDelegationsForFile Tests
// ============================================================================

func TestGetDelegationsForFile(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fh := []byte("fh-multi-deleg")

	// Grant multiple delegations on same file
	deleg1 := sm.GrantDelegation(100, fh, types.OPEN_DELEGATE_READ)
	deleg2 := sm.GrantDelegation(200, fh, types.OPEN_DELEGATE_READ)

	delegs := sm.GetDelegationsForFile(fh)
	if len(delegs) != 2 {
		t.Fatalf("expected 2 delegations, got %d", len(delegs))
	}

	// Verify both are present
	found1, found2 := false, false
	for _, d := range delegs {
		if d.ClientID == deleg1.ClientID {
			found1 = true
		}
		if d.ClientID == deleg2.ClientID {
			found2 = true
		}
	}
	if !found1 || !found2 {
		t.Error("expected both delegations to be returned")
	}
}

func TestGetDelegationsForFile_Empty(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	delegs := sm.GetDelegationsForFile([]byte("fh-no-delegations"))
	if len(delegs) != 0 {
		t.Errorf("expected 0 delegations for unknown file, got %d", len(delegs))
	}
}

// ============================================================================
// Lease Expiry Delegation Cleanup Tests
// ============================================================================

func TestLeaseExpiry_CleansDelegations(t *testing.T) {
	sm := NewStateManager(50 * time.Millisecond)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	// Create and confirm a client
	result, err := sm.SetClientID("test-client-deleg-expire", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	clientID := result.ClientID

	err = sm.ConfirmClientID(clientID, result.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	// Grant a delegation for this client
	fh := []byte("fh-deleg-expire-test")
	deleg := sm.GrantDelegation(clientID, fh, types.OPEN_DELEGATE_READ)

	// Verify delegation exists
	delegs := sm.GetDelegationsForFile(fh)
	if len(delegs) != 1 {
		t.Fatalf("expected 1 delegation before expiry, got %d", len(delegs))
	}

	// Wait for lease to expire and trigger cleanup
	time.Sleep(200 * time.Millisecond)

	// Verify delegation was cleaned up
	delegs = sm.GetDelegationsForFile(fh)
	if len(delegs) != 0 {
		t.Errorf("expected 0 delegations after lease expiry, got %d", len(delegs))
	}

	// Verify not in delegByOther
	sm.mu.RLock()
	_, exists := sm.delegByOther[deleg.Stateid.Other]
	sm.mu.RUnlock()
	if exists {
		t.Error("delegation should be removed from delegByOther after lease expiry")
	}
}

func TestLeaseExpiry_MultipleClients(t *testing.T) {
	sm := NewStateManager(100 * time.Millisecond)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	// Client A -- will expire
	resultA, err := sm.SetClientID("client-A-deleg-expire", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID A: %v", err)
	}
	err = sm.ConfirmClientID(resultA.ClientID, resultA.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID A: %v", err)
	}

	// Client B -- will be kept alive
	resultB, err := sm.SetClientID("client-B-deleg-alive", verifier, callback, "10.0.0.2:1234")
	if err != nil {
		t.Fatalf("SetClientID B: %v", err)
	}
	err = sm.ConfirmClientID(resultB.ClientID, resultB.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID B: %v", err)
	}

	// Grant delegations for both clients on different files
	fhA := []byte("fh-client-A-deleg")
	fhB := []byte("fh-client-B-deleg")
	sm.GrantDelegation(resultA.ClientID, fhA, types.OPEN_DELEGATE_READ)
	delegB := sm.GrantDelegation(resultB.ClientID, fhB, types.OPEN_DELEGATE_READ)

	// Keep client B alive with aggressive renewals, let client A expire
	for i := 0; i < 8; i++ {
		time.Sleep(30 * time.Millisecond)
		_ = sm.RenewLease(resultB.ClientID)
	}

	// Client A should have expired by now (100ms lease, 240ms elapsed)
	// Wait a bit more for cleanup
	time.Sleep(50 * time.Millisecond)

	// Client A's delegation should be gone
	delegsA := sm.GetDelegationsForFile(fhA)
	if len(delegsA) != 0 {
		t.Errorf("client A's delegation should be removed after expiry, got %d", len(delegsA))
	}

	// Client B's delegation should still exist
	delegsB := sm.GetDelegationsForFile(fhB)
	if len(delegsB) != 1 {
		t.Fatalf("client B's delegation should still exist, got %d", len(delegsB))
	}
	if delegsB[0].ClientID != delegB.ClientID {
		t.Errorf("remaining delegation should belong to client B")
	}
}

// ============================================================================
// countOpensOnFile Tests
// ============================================================================

func TestCountOpensOnFile(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fh := []byte("fh-count-opens")

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	// Client 1: create and confirm
	result1, err := sm.SetClientID("client-count-1", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID 1: %v", err)
	}
	err = sm.ConfirmClientID(result1.ClientID, result1.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID 1: %v", err)
	}

	// Client 2: create and confirm
	result2, err := sm.SetClientID("client-count-2", verifier, callback, "10.0.0.2:1234")
	if err != nil {
		t.Fatalf("SetClientID 2: %v", err)
	}
	err = sm.ConfirmClientID(result2.ClientID, result2.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID 2: %v", err)
	}

	// Open from client 1
	_, err = sm.OpenFile(result1.ClientID, []byte("owner1"), 1, fh,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile client 1: %v", err)
	}

	// Open from client 2
	_, err = sm.OpenFile(result2.ClientID, []byte("owner2"), 1, fh,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile client 2: %v", err)
	}

	sm.mu.RLock()
	defer sm.mu.RUnlock()

	// Count opens excluding client 1: should be 1 (from client 2)
	count := sm.countOpensOnFile(fh, result1.ClientID)
	if count != 1 {
		t.Errorf("countOpensOnFile excluding client 1 = %d, want 1", count)
	}

	// Count opens excluding client 2: should be 1 (from client 1)
	count = sm.countOpensOnFile(fh, result2.ClientID)
	if count != 1 {
		t.Errorf("countOpensOnFile excluding client 2 = %d, want 1", count)
	}

	// Count opens excluding a non-existent client: should be 2
	count = sm.countOpensOnFile(fh, 99999)
	if count != 2 {
		t.Errorf("countOpensOnFile excluding non-existent = %d, want 2", count)
	}
}

// ============================================================================
// Delegation Stateid Type Tag Tests
// ============================================================================

func TestDelegationStateidTypeTag(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	deleg := sm.GrantDelegation(100, []byte("fh-type-tag"), types.OPEN_DELEGATE_READ)

	// The first byte of the "other" field should be 0x03 (StateTypeDeleg)
	if deleg.Stateid.Other[0] != 0x03 {
		t.Errorf("delegation stateid type tag = 0x%02x, want 0x03", deleg.Stateid.Other[0])
	}
}

func TestDelegationStateidUniqueAcrossGrants(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	deleg1 := sm.GrantDelegation(100, []byte("fh-unique-1"), types.OPEN_DELEGATE_READ)
	deleg2 := sm.GrantDelegation(200, []byte("fh-unique-2"), types.OPEN_DELEGATE_WRITE)

	if deleg1.Stateid.Other == deleg2.Stateid.Other {
		t.Error("two different delegation stateids should have different Other fields")
	}
}

// ============================================================================
// ReturnDelegation with File Handle Map Cleanup Tests
// ============================================================================

func TestReturnDelegation_CleansFileMap(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fh := []byte("fh-file-map-cleanup")
	deleg1 := sm.GrantDelegation(100, fh, types.OPEN_DELEGATE_READ)
	_ = sm.GrantDelegation(200, fh, types.OPEN_DELEGATE_READ)

	// Return first delegation
	err := sm.ReturnDelegation(&deleg1.Stateid)
	if err != nil {
		t.Fatalf("ReturnDelegation: %v", err)
	}

	// Should still have one delegation on this file
	delegs := sm.GetDelegationsForFile(fh)
	if len(delegs) != 1 {
		t.Errorf("expected 1 remaining delegation, got %d", len(delegs))
	}
	if delegs[0].ClientID != 200 {
		t.Errorf("remaining delegation should be for client 200, got %d", delegs[0].ClientID)
	}
}

// ============================================================================
// ShouldGrantDelegation Tests (Plan 11-03)
// ============================================================================

// setCBPathUp is a test helper that sets the CBPathUp flag on a client.
// This simulates a successful CB_NULL verification without needing a real listener.
func setCBPathUp(sm *StateManager, clientID uint64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if rec, ok := sm.clientsByID[clientID]; ok {
		rec.CBPathUp = true
	}
}

func TestShouldGrantDelegation_ReadAccess(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	result, err := sm.SetClientID("client-grant-read", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	err = sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}
	setCBPathUp(sm, result.ClientID) // Simulate successful CB_NULL

	fh := []byte("fh-grant-read")
	delegType, shouldGrant := sm.ShouldGrantDelegation(result.ClientID, fh, types.OPEN4_SHARE_ACCESS_READ)

	if !shouldGrant {
		t.Fatal("should grant delegation for sole reader with callback")
	}
	if delegType != types.OPEN_DELEGATE_READ {
		t.Errorf("delegType = %d, want OPEN_DELEGATE_READ (%d)", delegType, types.OPEN_DELEGATE_READ)
	}
}

func TestShouldGrantDelegation_WriteAccess(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	result, err := sm.SetClientID("client-grant-write", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	err = sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}
	setCBPathUp(sm, result.ClientID)

	fh := []byte("fh-grant-write")
	delegType, shouldGrant := sm.ShouldGrantDelegation(result.ClientID, fh, types.OPEN4_SHARE_ACCESS_WRITE)

	if !shouldGrant {
		t.Fatal("should grant delegation for sole writer with callback")
	}
	if delegType != types.OPEN_DELEGATE_WRITE {
		t.Errorf("delegType = %d, want OPEN_DELEGATE_WRITE (%d)", delegType, types.OPEN_DELEGATE_WRITE)
	}
}

func TestShouldGrantDelegation_BothAccess(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	result, err := sm.SetClientID("client-grant-both", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	err = sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}
	setCBPathUp(sm, result.ClientID)

	fh := []byte("fh-grant-both")
	delegType, shouldGrant := sm.ShouldGrantDelegation(result.ClientID, fh, types.OPEN4_SHARE_ACCESS_BOTH)

	if !shouldGrant {
		t.Fatal("should grant delegation for sole accessor with callback")
	}
	if delegType != types.OPEN_DELEGATE_WRITE {
		t.Errorf("BOTH access should grant WRITE delegation, got %d", delegType)
	}
}

func TestShouldGrantDelegation_NoCallback(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	// Empty callback address
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: ""}

	result, err := sm.SetClientID("client-no-cb", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	err = sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	fh := []byte("fh-no-callback")
	_, shouldGrant := sm.ShouldGrantDelegation(result.ClientID, fh, types.OPEN4_SHARE_ACCESS_READ)

	if shouldGrant {
		t.Error("should NOT grant delegation when callback is empty")
	}
}

func TestShouldGrantDelegation_UnknownClient(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fh := []byte("fh-unknown-client")
	_, shouldGrant := sm.ShouldGrantDelegation(99999, fh, types.OPEN4_SHARE_ACCESS_READ)

	if shouldGrant {
		t.Error("should NOT grant delegation for unknown client")
	}
}

func TestShouldGrantDelegation_OtherClientHasOpens(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	// Client A
	resultA, err := sm.SetClientID("client-grant-A", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID A: %v", err)
	}
	err = sm.ConfirmClientID(resultA.ClientID, resultA.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID A: %v", err)
	}

	// Client B
	resultB, err := sm.SetClientID("client-grant-B", verifier, callback, "10.0.0.2:1234")
	if err != nil {
		t.Fatalf("SetClientID B: %v", err)
	}
	err = sm.ConfirmClientID(resultB.ClientID, resultB.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID B: %v", err)
	}

	fh := []byte("fh-multi-open")

	// Client A opens file
	_, err = sm.OpenFile(resultA.ClientID, []byte("ownerA"), 1, fh,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile A: %v", err)
	}

	// Client B tries to get delegation -- should be denied
	_, shouldGrant := sm.ShouldGrantDelegation(resultB.ClientID, fh, types.OPEN4_SHARE_ACCESS_READ)
	if shouldGrant {
		t.Error("should NOT grant delegation when another client has opens")
	}
}

func TestShouldGrantDelegation_SameClientAlreadyHasDeleg(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	result, err := sm.SetClientID("client-double-deleg", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	err = sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	fh := []byte("fh-double-deleg")
	// Grant first delegation
	sm.GrantDelegation(result.ClientID, fh, types.OPEN_DELEGATE_READ)

	// Trying again should NOT grant (avoid double-grant)
	_, shouldGrant := sm.ShouldGrantDelegation(result.ClientID, fh, types.OPEN4_SHARE_ACCESS_READ)
	if shouldGrant {
		t.Error("should NOT grant delegation when client already has one on same file")
	}
}

func TestShouldGrantDelegation_OtherClientHasDeleg(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	// Client A
	resultA, err := sm.SetClientID("client-deleg-conflict-A", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID A: %v", err)
	}
	err = sm.ConfirmClientID(resultA.ClientID, resultA.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID A: %v", err)
	}

	// Client B
	resultB, err := sm.SetClientID("client-deleg-conflict-B", verifier, callback, "10.0.0.2:1234")
	if err != nil {
		t.Fatalf("SetClientID B: %v", err)
	}
	err = sm.ConfirmClientID(resultB.ClientID, resultB.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID B: %v", err)
	}

	fh := []byte("fh-deleg-conflict")
	// Client A holds a delegation
	sm.GrantDelegation(resultA.ClientID, fh, types.OPEN_DELEGATE_READ)

	// Client B should NOT get a delegation
	_, shouldGrant := sm.ShouldGrantDelegation(resultB.ClientID, fh, types.OPEN4_SHARE_ACCESS_READ)
	if shouldGrant {
		t.Error("should NOT grant delegation when another client already has one")
	}
}

// ============================================================================
// CheckDelegationConflict Tests (Plan 11-03)
// ============================================================================

func TestCheckDelegationConflict_NoConflict(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fh := []byte("fh-no-conflict")
	conflict, err := sm.CheckDelegationConflict(fh, 100, types.OPEN4_SHARE_ACCESS_READ)
	if err != nil {
		t.Fatalf("CheckDelegationConflict: %v", err)
	}
	if conflict {
		t.Error("expected no conflict on file with no delegations")
	}
}

func TestCheckDelegationConflict_WriteDelegVsAnyAccess(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fh := []byte("fh-write-deleg-conflict")
	// Client A holds a WRITE delegation
	sm.GrantDelegation(100, fh, types.OPEN_DELEGATE_WRITE)

	// Client B tries to read: should conflict
	conflict, err := sm.CheckDelegationConflict(fh, 200, types.OPEN4_SHARE_ACCESS_READ)
	if err != nil {
		t.Fatalf("CheckDelegationConflict: %v", err)
	}
	if !conflict {
		t.Error("WRITE delegation should conflict with any access from another client")
	}
}

func TestCheckDelegationConflict_ReadDelegVsWriteAccess(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fh := []byte("fh-read-deleg-write-conflict")
	// Client A holds a READ delegation
	sm.GrantDelegation(100, fh, types.OPEN_DELEGATE_READ)

	// Client B tries to write: should conflict
	conflict, err := sm.CheckDelegationConflict(fh, 200, types.OPEN4_SHARE_ACCESS_WRITE)
	if err != nil {
		t.Fatalf("CheckDelegationConflict: %v", err)
	}
	if !conflict {
		t.Error("READ delegation should conflict with WRITE access from another client")
	}
}

func TestCheckDelegationConflict_ReadDelegVsReadAccess(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fh := []byte("fh-read-deleg-read-access")
	// Client A holds a READ delegation
	sm.GrantDelegation(100, fh, types.OPEN_DELEGATE_READ)

	// Client B tries to read: should NOT conflict
	conflict, err := sm.CheckDelegationConflict(fh, 200, types.OPEN4_SHARE_ACCESS_READ)
	if err != nil {
		t.Fatalf("CheckDelegationConflict: %v", err)
	}
	if conflict {
		t.Error("READ delegation should NOT conflict with READ access from another client")
	}
}

func TestCheckDelegationConflict_SameClient(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fh := []byte("fh-same-client-conflict")
	// Client A holds a WRITE delegation
	sm.GrantDelegation(100, fh, types.OPEN_DELEGATE_WRITE)

	// Same client opens again: should NOT conflict
	conflict, err := sm.CheckDelegationConflict(fh, 100, types.OPEN4_SHARE_ACCESS_WRITE)
	if err != nil {
		t.Fatalf("CheckDelegationConflict: %v", err)
	}
	if conflict {
		t.Error("same client's own delegation should NOT be a conflict")
	}
}

func TestCheckDelegationConflict_RevokedDelegIgnored(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fh := []byte("fh-revoked-deleg")
	deleg := sm.GrantDelegation(100, fh, types.OPEN_DELEGATE_WRITE)

	// Mark as revoked
	sm.mu.Lock()
	deleg.Revoked = true
	sm.mu.Unlock()

	// Client B: should NOT conflict with revoked delegation
	conflict, err := sm.CheckDelegationConflict(fh, 200, types.OPEN4_SHARE_ACCESS_WRITE)
	if err != nil {
		t.Fatalf("CheckDelegationConflict: %v", err)
	}
	if conflict {
		t.Error("revoked delegation should NOT cause a conflict")
	}
}

func TestCheckDelegationConflict_SetsRecallSent(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fh := []byte("fh-recall-flag")
	deleg := sm.GrantDelegation(100, fh, types.OPEN_DELEGATE_WRITE)

	// Client B triggers conflict
	conflict, _ := sm.CheckDelegationConflict(fh, 200, types.OPEN4_SHARE_ACCESS_READ)
	if !conflict {
		t.Fatal("expected conflict")
	}

	// Wait briefly for goroutine to launch
	time.Sleep(10 * time.Millisecond)

	// Check that RecallSent was set
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	if !deleg.RecallSent {
		t.Error("RecallSent should be true after conflict detection")
	}
	if deleg.RecallTime.IsZero() {
		t.Error("RecallTime should be set after conflict detection")
	}
}

// ============================================================================
// ValidateDelegationStateid Tests (Plan 11-03)
// ============================================================================

func TestValidateDelegationStateid_Success(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	deleg := sm.GrantDelegation(100, []byte("fh-validate-deleg"), types.OPEN_DELEGATE_READ)

	result, err := sm.ValidateDelegationStateid(&deleg.Stateid)
	if err != nil {
		t.Fatalf("ValidateDelegationStateid: %v", err)
	}
	if result.ClientID != 100 {
		t.Errorf("ClientID = %d, want 100", result.ClientID)
	}
}

func TestValidateDelegationStateid_NotFound(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	other := sm.generateStateidOther(StateTypeDeleg)
	stateid := &types.Stateid4{Seqid: 1, Other: other}

	_, err := sm.ValidateDelegationStateid(stateid)
	if err == nil {
		t.Fatal("expected error for unknown delegation stateid")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_BAD_STATEID {
		t.Errorf("status = %d, want NFS4ERR_BAD_STATEID (%d)", stateErr.Status, types.NFS4ERR_BAD_STATEID)
	}
}

func TestValidateDelegationStateid_StaleEpoch(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	var other [types.NFS4_OTHER_SIZE]byte
	other[0] = StateTypeDeleg
	other[1] = 0xFF
	other[2] = 0xFF
	other[3] = 0xFF
	stateid := &types.Stateid4{Seqid: 1, Other: other}

	_, err := sm.ValidateDelegationStateid(stateid)
	if err == nil {
		t.Fatal("expected error for stale delegation stateid")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_STALE_STATEID {
		t.Errorf("status = %d, want NFS4ERR_STALE_STATEID (%d)", stateErr.Status, types.NFS4ERR_STALE_STATEID)
	}
}

// ============================================================================
// EncodeDelegation Tests (Plan 11-03)
// ============================================================================

func TestEncodeDelegation_None(t *testing.T) {
	var buf bytes.Buffer
	EncodeDelegation(&buf, nil)

	data := buf.Bytes()
	if len(data) != 4 {
		t.Fatalf("expected 4 bytes for OPEN_DELEGATE_NONE, got %d", len(data))
	}
	// Should be uint32(0) = OPEN_DELEGATE_NONE
	delegType := uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
	if delegType != types.OPEN_DELEGATE_NONE {
		t.Errorf("delegation type = %d, want OPEN_DELEGATE_NONE (%d)", delegType, types.OPEN_DELEGATE_NONE)
	}
}

func TestEncodeDelegation_Read(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	deleg := sm.GrantDelegation(100, []byte("fh-encode-read"), types.OPEN_DELEGATE_READ)

	var buf bytes.Buffer
	EncodeDelegation(&buf, deleg)

	data := buf.Bytes()
	// Minimum: 4 (type) + 16 (stateid) + 4 (recall) + 4+4+4 (nfsace4 type/flag/mask) + 4+len("EVERYONE@")+padding (who)
	// = 4 + 16 + 4 + 12 + 4 + 9 + 3 = 52 bytes
	if len(data) < 40 {
		t.Fatalf("read delegation encoding too short: %d bytes", len(data))
	}

	// First 4 bytes should be OPEN_DELEGATE_READ (1)
	delegType := uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
	if delegType != types.OPEN_DELEGATE_READ {
		t.Errorf("delegation type = %d, want OPEN_DELEGATE_READ (%d)", delegType, types.OPEN_DELEGATE_READ)
	}
}

func TestEncodeDelegation_Write(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	deleg := sm.GrantDelegation(200, []byte("fh-encode-write"), types.OPEN_DELEGATE_WRITE)

	var buf bytes.Buffer
	EncodeDelegation(&buf, deleg)

	data := buf.Bytes()
	// Write delegation includes space_limit (4 + 8 = 12 extra bytes)
	if len(data) < 52 {
		t.Fatalf("write delegation encoding too short: %d bytes", len(data))
	}

	// First 4 bytes should be OPEN_DELEGATE_WRITE (2)
	delegType := uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
	if delegType != types.OPEN_DELEGATE_WRITE {
		t.Errorf("delegation type = %d, want OPEN_DELEGATE_WRITE (%d)", delegType, types.OPEN_DELEGATE_WRITE)
	}
}

// ============================================================================
// Recall Timer and Revocation Tests (Plan 11-04)
// ============================================================================

func TestRecallTimer_FiresRevocation(t *testing.T) {
	sm := NewStateManager(200 * time.Millisecond)

	fh := []byte("fh-recall-timer-revoke")
	deleg := sm.GrantDelegation(100, fh, types.OPEN_DELEGATE_WRITE)

	// Start recall timer with the lease duration
	deleg.StartRecallTimer(sm.leaseDuration, func() {
		sm.RevokeDelegation(deleg.Stateid.Other)
	})

	// Wait for timer to fire (lease duration + buffer)
	time.Sleep(350 * time.Millisecond)

	// Verify delegation is revoked
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	if !deleg.Revoked {
		t.Error("delegation should be revoked after recall timer fires")
	}

	// Should still be in delegByOther (for stale detection)
	if _, exists := sm.delegByOther[deleg.Stateid.Other]; !exists {
		t.Error("revoked delegation should remain in delegByOther for stale detection")
	}

	// Should NOT be in delegByFile
	if delegs, exists := sm.delegByFile[string(fh)]; exists && len(delegs) > 0 {
		t.Error("revoked delegation should be removed from delegByFile")
	}
}

func TestRecallTimer_CancelledOnReturn(t *testing.T) {
	sm := NewStateManager(200 * time.Millisecond)

	fh := []byte("fh-recall-timer-cancel")
	deleg := sm.GrantDelegation(100, fh, types.OPEN_DELEGATE_READ)

	// Start recall timer
	deleg.StartRecallTimer(sm.leaseDuration, func() {
		sm.RevokeDelegation(deleg.Stateid.Other)
	})

	// Return delegation before timer fires (cancels timer)
	err := sm.ReturnDelegation(&deleg.Stateid)
	if err != nil {
		t.Fatalf("ReturnDelegation: %v", err)
	}

	// Wait past timer duration
	time.Sleep(350 * time.Millisecond)

	// Delegation should NOT be revoked (timer was cancelled)
	if deleg.Revoked {
		t.Error("delegation should NOT be revoked -- timer was cancelled by return")
	}
}

func TestRevokeDelegation_CleansState(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fh := []byte("fh-revoke-cleans")
	deleg := sm.GrantDelegation(100, fh, types.OPEN_DELEGATE_WRITE)

	sm.RevokeDelegation(deleg.Stateid.Other)

	sm.mu.RLock()
	defer sm.mu.RUnlock()

	// Should be marked revoked
	if !deleg.Revoked {
		t.Error("delegation should be marked as Revoked")
	}

	// Should be removed from delegByFile
	if delegs, exists := sm.delegByFile[string(fh)]; exists && len(delegs) > 0 {
		t.Error("revoked delegation should be removed from delegByFile")
	}

	// Should still be in delegByOther (for stale detection)
	if _, exists := sm.delegByOther[deleg.Stateid.Other]; !exists {
		t.Error("revoked delegation should remain in delegByOther")
	}

	// Should be in recentlyRecalled cache
	if !sm.isRecentlyRecalled(fh) {
		t.Error("file should be in recentlyRecalled cache after revocation")
	}
}

func TestRevokeDelegation_AlreadyReturned(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fh := []byte("fh-revoke-already-returned")
	deleg := sm.GrantDelegation(100, fh, types.OPEN_DELEGATE_READ)

	// Return first
	err := sm.ReturnDelegation(&deleg.Stateid)
	if err != nil {
		t.Fatalf("ReturnDelegation: %v", err)
	}

	// Revoke after return: should be a no-op (delegation gone from delegByOther)
	sm.RevokeDelegation(deleg.Stateid.Other)

	// No panic, no state corruption
	if deleg.Revoked {
		t.Error("delegation should NOT be marked Revoked (it was already returned and cleaned up)")
	}
}

func TestRevokeDelegation_Idempotent(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fh := []byte("fh-revoke-idempotent")
	deleg := sm.GrantDelegation(100, fh, types.OPEN_DELEGATE_WRITE)

	// Revoke twice -- second should be a no-op
	sm.RevokeDelegation(deleg.Stateid.Other)
	sm.RevokeDelegation(deleg.Stateid.Other)

	if !deleg.Revoked {
		t.Error("delegation should be revoked")
	}
}

func TestRevokedDelegation_BadStateidOnValidate(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fh := []byte("fh-revoked-validate")
	deleg := sm.GrantDelegation(100, fh, types.OPEN_DELEGATE_READ)

	// Revoke it
	sm.RevokeDelegation(deleg.Stateid.Other)

	// Validate should return NFS4ERR_BAD_STATEID
	_, err := sm.ValidateDelegationStateid(&deleg.Stateid)
	if err == nil {
		t.Fatal("expected error for revoked delegation stateid")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_BAD_STATEID {
		t.Errorf("status = %d, want NFS4ERR_BAD_STATEID (%d)", stateErr.Status, types.NFS4ERR_BAD_STATEID)
	}
}

func TestRevokedDelegation_ReturnSucceeds(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fh := []byte("fh-revoked-return")
	deleg := sm.GrantDelegation(100, fh, types.OPEN_DELEGATE_WRITE)

	// Revoke it
	sm.RevokeDelegation(deleg.Stateid.Other)

	// Client sends DELEGRETURN for revoked delegation: should succeed (NFS4_OK)
	err := sm.ReturnDelegation(&deleg.Stateid)
	if err != nil {
		t.Fatalf("ReturnDelegation for revoked delegation should succeed, got %v", err)
	}

	// After return, delegation should be fully cleaned up from delegByOther
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	if _, exists := sm.delegByOther[deleg.Stateid.Other]; exists {
		t.Error("delegation should be removed from delegByOther after return of revoked delegation")
	}
}

// ============================================================================
// Callback Path Tracking Tests (Plan 11-04)
// ============================================================================

func TestCBPathUp_VerifiedOnConfirm(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Start a mock TCP listener that accepts CB_NULL
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = l.Close() }()

	// Handle one CB_NULL connection
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		// Read the record mark + RPC call
		var recMark [4]byte
		if _, err := io.ReadFull(conn, recMark[:]); err != nil {
			return
		}
		fragLen := binary.BigEndian.Uint32(recMark[:]) & 0x7FFFFFFF
		callData := make([]byte, fragLen)
		if _, err := io.ReadFull(conn, callData); err != nil {
			return
		}

		// Extract XID (first 4 bytes)
		xid := binary.BigEndian.Uint32(callData[0:4])

		// Build RPC reply: accepted, success
		var reply bytes.Buffer
		_ = binary.Write(&reply, binary.BigEndian, xid)       // XID
		_ = binary.Write(&reply, binary.BigEndian, uint32(1)) // REPLY
		_ = binary.Write(&reply, binary.BigEndian, uint32(0)) // MSG_ACCEPTED
		_ = binary.Write(&reply, binary.BigEndian, uint32(0)) // AUTH_NULL
		_ = binary.Write(&reply, binary.BigEndian, uint32(0)) // opaque len 0
		_ = binary.Write(&reply, binary.BigEndian, uint32(0)) // ACCEPT_SUCCESS

		// Write record mark + reply
		replyBytes := reply.Bytes()
		var outRecMark [4]byte
		binary.BigEndian.PutUint32(outRecMark[:], uint32(len(replyBytes))|0x80000000)
		_, _ = conn.Write(outRecMark[:])
		_, _ = conn.Write(replyBytes)
	}()

	callback := makeCallbackInfoFromListener(l)
	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}

	result, err := sm.SetClientID("client-cbpath-verify", verifier, callback, "127.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	err = sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	// Wait for async CB_NULL goroutine to complete
	time.Sleep(300 * time.Millisecond)

	sm.mu.RLock()
	rec := sm.clientsByID[result.ClientID]
	cbUp := rec.CBPathUp
	sm.mu.RUnlock()

	if !cbUp {
		t.Error("CBPathUp should be true after successful CB_NULL")
	}
}

func TestCBPathUp_FailedOnConfirm(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Use an unreachable address for the callback
	callback := CallbackInfo{
		Program: 0x40000000,
		NetID:   "tcp",
		Addr:    "127.0.0.1.255.255", // port 65535 -- likely connection refused
	}
	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}

	result, err := sm.SetClientID("client-cbpath-fail", verifier, callback, "127.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	err = sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	// Wait for async CB_NULL goroutine to timeout/fail
	time.Sleep(6 * time.Second) // CB_NULL has a 5s timeout

	sm.mu.RLock()
	rec := sm.clientsByID[result.ClientID]
	cbUp := rec.CBPathUp
	sm.mu.RUnlock()

	if cbUp {
		t.Error("CBPathUp should be false after failed CB_NULL")
	}
}

func TestCBPathUp_NoCallbackAddr(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Empty callback address: CB_NULL should NOT be sent
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: ""}
	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}

	result, err := sm.SetClientID("client-no-cbaddr", verifier, callback, "127.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	err = sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	// Wait briefly
	time.Sleep(100 * time.Millisecond)

	sm.mu.RLock()
	rec := sm.clientsByID[result.ClientID]
	cbUp := rec.CBPathUp
	sm.mu.RUnlock()

	if cbUp {
		t.Error("CBPathUp should be false when callback address is empty")
	}
}

func TestShouldGrantDelegation_CBPathDown(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	result, err := sm.SetClientID("client-cbpath-down", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	err = sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	// Do NOT set CBPathUp -- it defaults to false
	fh := []byte("fh-cbpath-down-grant")
	_, shouldGrant := sm.ShouldGrantDelegation(result.ClientID, fh, types.OPEN4_SHARE_ACCESS_READ)

	if shouldGrant {
		t.Error("should NOT grant delegation when CBPathUp is false")
	}
}

// ============================================================================
// Recently-Recalled Cache Tests (Plan 11-04)
// ============================================================================

func TestRecentlyRecalled_BlocksGrant(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	result, err := sm.SetClientID("client-recently-recalled", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	err = sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}
	setCBPathUp(sm, result.ClientID)

	fh := []byte("fh-recently-recalled")

	// Add to recently-recalled cache directly
	sm.mu.Lock()
	sm.addRecentlyRecalled(fh)
	sm.mu.Unlock()

	// ShouldGrantDelegation should return false
	_, shouldGrant := sm.ShouldGrantDelegation(result.ClientID, fh, types.OPEN4_SHARE_ACCESS_READ)
	if shouldGrant {
		t.Error("should NOT grant delegation for recently-recalled file")
	}
}

func TestRecentlyRecalled_ExpiresAfterTTL(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	// Set a very short TTL for testing
	sm.recentlyRecalledTTL = 100 * time.Millisecond

	fh := []byte("fh-recently-recalled-expire")

	sm.mu.Lock()
	sm.addRecentlyRecalled(fh)
	sm.mu.Unlock()

	// Should be recently recalled now
	sm.mu.RLock()
	isRecalled := sm.isRecentlyRecalled(fh)
	sm.mu.RUnlock()
	if !isRecalled {
		t.Fatal("file should be recently recalled immediately after adding")
	}

	// Wait for TTL to expire
	time.Sleep(150 * time.Millisecond)

	// Should no longer be recently recalled
	sm.mu.RLock()
	isRecalled = sm.isRecentlyRecalled(fh)
	sm.mu.RUnlock()
	if isRecalled {
		t.Error("file should NOT be recently recalled after TTL expires")
	}
}

func TestRecentlyRecalled_DifferentFile(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fhA := []byte("fh-recalled-A")
	fhB := []byte("fh-recalled-B")

	sm.mu.Lock()
	sm.addRecentlyRecalled(fhA)
	sm.mu.Unlock()

	sm.mu.RLock()
	isRecalledA := sm.isRecentlyRecalled(fhA)
	isRecalledB := sm.isRecentlyRecalled(fhB)
	sm.mu.RUnlock()

	if !isRecalledA {
		t.Error("file A should be recently recalled")
	}
	if isRecalledB {
		t.Error("file B should NOT be recently recalled")
	}
}

func TestRecentlyRecalled_AddedOnRevocation(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fh := []byte("fh-revoke-adds-recalled")
	deleg := sm.GrantDelegation(100, fh, types.OPEN_DELEGATE_WRITE)

	// Revoke the delegation
	sm.RevokeDelegation(deleg.Stateid.Other)

	// File should now be in recently-recalled cache
	sm.mu.RLock()
	isRecalled := sm.isRecentlyRecalled(fh)
	sm.mu.RUnlock()

	if !isRecalled {
		t.Error("file should be in recently-recalled cache after revocation")
	}
}

// ============================================================================
// Shutdown Tests (Plan 11-04)
// ============================================================================

func TestShutdown_StopsRecallTimers(t *testing.T) {
	sm := NewStateManager(200 * time.Millisecond)

	fh := []byte("fh-shutdown-recall")
	deleg := sm.GrantDelegation(100, fh, types.OPEN_DELEGATE_WRITE)

	// Start recall timer
	deleg.StartRecallTimer(sm.leaseDuration, func() {
		sm.RevokeDelegation(deleg.Stateid.Other)
	})

	// Shutdown stops all recall timers
	sm.Shutdown()

	// Wait past timer duration
	time.Sleep(350 * time.Millisecond)

	// Delegation should NOT be revoked (timer was cancelled by shutdown)
	if deleg.Revoked {
		t.Error("delegation should NOT be revoked -- timer was stopped by Shutdown")
	}
}
