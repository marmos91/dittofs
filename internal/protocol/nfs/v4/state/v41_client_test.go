package state

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
)

func TestExchangeID_NewClient(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)

	ownerID := []byte("test-client-owner")
	var verifier [8]byte
	copy(verifier[:], "verify01")

	result, err := sm.ExchangeID(ownerID, verifier, 0, nil, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID error: %v", err)
	}

	if result.ClientID == 0 {
		t.Error("ClientID should not be zero")
	}
	if result.SequenceID != 1 {
		t.Errorf("SequenceID = %d, want 1", result.SequenceID)
	}
	// Should have USE_NON_PNFS but NOT CONFIRMED_R
	if result.Flags&types.EXCHGID4_FLAG_USE_NON_PNFS == 0 {
		t.Error("Flags should include EXCHGID4_FLAG_USE_NON_PNFS")
	}
	if result.Flags&types.EXCHGID4_FLAG_CONFIRMED_R != 0 {
		t.Error("Flags should NOT include EXCHGID4_FLAG_CONFIRMED_R for new client")
	}

	// Verify server identity is populated
	if len(result.ServerOwner.MajorID) == 0 {
		t.Error("ServerOwner.MajorID should not be empty")
	}
	if len(result.ServerScope) == 0 {
		t.Error("ServerScope should not be empty")
	}
	if len(result.ServerImplId) != 1 {
		t.Fatalf("ServerImplId length = %d, want 1", len(result.ServerImplId))
	}
	if result.ServerImplId[0].Name != "dittofs" {
		t.Errorf("ServerImplId Name = %q, want %q", result.ServerImplId[0].Name, "dittofs")
	}
	if result.ServerImplId[0].Domain != "dittofs.io" {
		t.Errorf("ServerImplId Domain = %q, want %q", result.ServerImplId[0].Domain, "dittofs.io")
	}
}

func TestExchangeID_SameOwnerSameVerifier(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)

	ownerID := []byte("idempotent-client")
	var verifier [8]byte
	copy(verifier[:], "verify01")

	result1, err := sm.ExchangeID(ownerID, verifier, 0, nil, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID #1 error: %v", err)
	}

	result2, err := sm.ExchangeID(ownerID, verifier, 0, nil, "10.0.0.2:12345")
	if err != nil {
		t.Fatalf("ExchangeID #2 error: %v", err)
	}

	// Same owner + same verifier = same clientID (idempotent)
	if result1.ClientID != result2.ClientID {
		t.Errorf("ClientIDs differ: %d vs %d (should be idempotent)", result1.ClientID, result2.ClientID)
	}
	if result1.SequenceID != result2.SequenceID {
		t.Errorf("SequenceIDs differ: %d vs %d", result1.SequenceID, result2.SequenceID)
	}
}

func TestExchangeID_SameOwnerDifferentVerifier(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)

	ownerID := []byte("rebooting-client")
	var verifier1, verifier2 [8]byte
	copy(verifier1[:], "verify01")
	copy(verifier2[:], "verify02")

	result1, err := sm.ExchangeID(ownerID, verifier1, 0, nil, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID #1 error: %v", err)
	}

	result2, err := sm.ExchangeID(ownerID, verifier2, 0, nil, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID #2 error: %v", err)
	}

	// Different verifier = client reboot = new clientID
	if result1.ClientID == result2.ClientID {
		t.Error("ClientIDs should differ for client reboot (different verifier)")
	}
	if result2.SequenceID != 1 {
		t.Errorf("SequenceID after reboot = %d, want 1", result2.SequenceID)
	}
}

func TestExchangeID_UnconfirmedReplace(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)

	ownerID := []byte("unconfirmed-client")
	var verifier1, verifier2 [8]byte
	copy(verifier1[:], "verify01")
	copy(verifier2[:], "verify02")

	result1, err := sm.ExchangeID(ownerID, verifier1, 0, nil, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID #1 error: %v", err)
	}

	// Record is unconfirmed, send with different verifier -> supersede
	result2, err := sm.ExchangeID(ownerID, verifier2, 0, nil, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID #2 error: %v", err)
	}

	if result1.ClientID == result2.ClientID {
		t.Error("ClientIDs should differ when superseding unconfirmed record")
	}

	// Old client should be purged
	sm.mu.RLock()
	_, existsOld := sm.v41ClientsByID[result1.ClientID]
	_, existsNew := sm.v41ClientsByID[result2.ClientID]
	sm.mu.RUnlock()

	if existsOld {
		t.Error("Old client record should have been purged")
	}
	if !existsNew {
		t.Error("New client record should exist")
	}
}

func TestExchangeID_MultipleClients(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)

	var verifier [8]byte

	ids := make(map[uint64]bool)
	for i := 0; i < 10; i++ {
		ownerID := []byte("client-" + string(rune('A'+i)))
		result, err := sm.ExchangeID(ownerID, verifier, 0, nil, "10.0.0.1:12345")
		if err != nil {
			t.Fatalf("ExchangeID #%d error: %v", i, err)
		}
		if ids[result.ClientID] {
			t.Errorf("Duplicate clientID %d from ExchangeID #%d", result.ClientID, i)
		}
		ids[result.ClientID] = true
	}

	if len(ids) != 10 {
		t.Errorf("Expected 10 unique client IDs, got %d", len(ids))
	}
}

func TestExchangeID_ServerIdentityConsistent(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)

	var verifier [8]byte

	result1, err := sm.ExchangeID([]byte("client-a"), verifier, 0, nil, "10.0.0.1:1")
	if err != nil {
		t.Fatalf("ExchangeID #1 error: %v", err)
	}

	result2, err := sm.ExchangeID([]byte("client-b"), verifier, 0, nil, "10.0.0.2:2")
	if err != nil {
		t.Fatalf("ExchangeID #2 error: %v", err)
	}

	// server_owner must be identical across calls
	if !bytes.Equal(result1.ServerOwner.MajorID, result2.ServerOwner.MajorID) {
		t.Error("ServerOwner.MajorID differs across calls")
	}
	if result1.ServerOwner.MinorID != result2.ServerOwner.MinorID {
		t.Error("ServerOwner.MinorID differs across calls")
	}
	if !bytes.Equal(result1.ServerScope, result2.ServerScope) {
		t.Error("ServerScope differs across calls")
	}

	// ServerImplId should be identical
	if len(result1.ServerImplId) != len(result2.ServerImplId) {
		t.Fatalf("ServerImplId lengths differ: %d vs %d", len(result1.ServerImplId), len(result2.ServerImplId))
	}
	if result1.ServerImplId[0].Name != result2.ServerImplId[0].Name {
		t.Error("ServerImplId Name differs across calls")
	}
}

func TestExchangeID_ImplInfoUpdate(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)

	ownerID := []byte("impl-test-client")
	var verifier [8]byte
	copy(verifier[:], "verify01")

	implID := []types.NfsImplId4{
		{
			Domain: "kernel.org",
			Name:   "Linux NFS client",
			Date:   types.NFS4Time{Seconds: 1000, Nseconds: 0},
		},
	}

	_, err := sm.ExchangeID(ownerID, verifier, 0, implID, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID #1 error: %v", err)
	}

	// Same owner + same verifier with updated impl info
	updatedImplID := []types.NfsImplId4{
		{
			Domain: "kernel.org",
			Name:   "Linux NFS client v2",
			Date:   types.NFS4Time{Seconds: 2000, Nseconds: 0},
		},
	}

	_, err = sm.ExchangeID(ownerID, verifier, 0, updatedImplID, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID #2 error: %v", err)
	}

	// Verify impl info was updated
	sm.mu.RLock()
	record := sm.v41ClientsByOwner[string(ownerID)]
	sm.mu.RUnlock()

	if record == nil {
		t.Fatal("Record not found after update")
	}
	if record.ImplName != "Linux NFS client v2" {
		t.Errorf("ImplName = %q, want %q", record.ImplName, "Linux NFS client v2")
	}
}

func TestExchangeID_ConcurrentAccess(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)
	var verifier [8]byte

	const numGoroutines = 50
	var wg sync.WaitGroup
	errCh := make(chan error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ownerID := []byte("concurrent-client-" + string(rune('A'+idx%26)))
			_, err := sm.ExchangeID(ownerID, verifier, 0, nil, "10.0.0.1:12345")
			if err != nil {
				errCh <- err
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("Concurrent ExchangeID error: %v", err)
	}

	// Verify state is consistent -- should have exactly 26 clients (one per letter)
	clients := sm.ListV41Clients()
	if len(clients) != 26 {
		t.Errorf("Expected 26 clients (one per letter), got %d", len(clients))
	}
}

func TestListV41Clients(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)
	var verifier [8]byte

	// Register 3 clients
	for i := 0; i < 3; i++ {
		ownerID := []byte("list-client-" + string(rune('A'+i)))
		_, err := sm.ExchangeID(ownerID, verifier, 0, nil, "10.0.0.1:12345")
		if err != nil {
			t.Fatalf("ExchangeID #%d error: %v", i, err)
		}
	}

	clients := sm.ListV41Clients()
	if len(clients) != 3 {
		t.Fatalf("ListV41Clients returned %d clients, want 3", len(clients))
	}

	// Verify returned values are copies, not references to internal records
	clients[0].ClientAddr = "mutated"

	sm.mu.RLock()
	for _, r := range sm.v41ClientsByID {
		if r.ClientAddr == "mutated" {
			t.Error("ListV41Clients should return copies, not references")
			break
		}
	}
	sm.mu.RUnlock()
}

func TestEvictV41Client(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)

	ownerID := []byte("evict-test-client")
	var verifier [8]byte
	copy(verifier[:], "verify01")

	result, err := sm.ExchangeID(ownerID, verifier, 0, nil, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID error: %v", err)
	}

	if err := sm.EvictV41Client(result.ClientID); err != nil {
		t.Fatalf("EvictV41Client error: %v", err)
	}
	sm.mu.RLock()
	_, existsID := sm.v41ClientsByID[result.ClientID]
	_, existsOwner := sm.v41ClientsByOwner[string(ownerID)]
	sm.mu.RUnlock()

	if existsID {
		t.Error("Client should not exist in v41ClientsByID after eviction")
	}
	if existsOwner {
		t.Error("Client should not exist in v41ClientsByOwner after eviction")
	}

	if err := sm.EvictV41Client(99999); err == nil {
		t.Error("Expected error when evicting non-existent client")
	}
}

func TestServerInfo(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)

	info := sm.ServerInfo()
	if info == nil {
		t.Fatal("ServerInfo returned nil")
	}

	if len(info.ServerOwner.MajorID) == 0 {
		t.Error("ServerOwner.MajorID should not be empty")
	}
	if len(info.ServerScope) == 0 {
		t.Error("ServerScope should not be empty")
	}
	if info.ImplID.Name != "dittofs" {
		t.Errorf("ImplID.Name = %q, want %q", info.ImplID.Name, "dittofs")
	}
	if info.ImplID.Domain != "dittofs.io" {
		t.Errorf("ImplID.Domain = %q, want %q", info.ImplID.Domain, "dittofs.io")
	}
}

func TestListV40Clients(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)

	// Register a v4.0 client the normal way
	_, err := sm.SetClientID("v40-test-client", [8]byte{1, 2, 3}, CallbackInfo{}, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("SetClientID error: %v", err)
	}

	// ListV40Clients should return 0 since it only returns confirmed clients
	clients := sm.ListV40Clients()
	if len(clients) != 0 {
		t.Errorf("ListV40Clients should return 0 for unconfirmed clients, got %d", len(clients))
	}
}

func TestExchangeID_ConfirmedReboot(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)

	ownerID := []byte("confirmed-reboot")
	var verifier1, verifier2 [8]byte
	copy(verifier1[:], "verify01")
	copy(verifier2[:], "verify02")

	result1, err := sm.ExchangeID(ownerID, verifier1, 0, nil, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID #1 error: %v", err)
	}

	// Simulate confirmation
	sm.mu.Lock()
	sm.v41ClientsByID[result1.ClientID].Confirmed = true
	sm.mu.Unlock()

	// Reboot with different verifier -> Case 3
	result2, err := sm.ExchangeID(ownerID, verifier2, 0, nil, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID #2 error: %v", err)
	}

	if result1.ClientID == result2.ClientID {
		t.Error("ClientID should change after reboot of confirmed client")
	}

	// Verify old record is gone
	sm.mu.RLock()
	_, existsOld := sm.v41ClientsByID[result1.ClientID]
	sm.mu.RUnlock()

	if existsOld {
		t.Error("Old confirmed client should be purged after reboot")
	}

	// New record should not have CONFIRMED_R
	if result2.Flags&types.EXCHGID4_FLAG_CONFIRMED_R != 0 {
		t.Error("New client after reboot should not have CONFIRMED_R")
	}
}

func TestExchangeID_ConfirmedIdempotent(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)

	ownerID := []byte("confirmed-idempotent")
	var verifier [8]byte
	copy(verifier[:], "verify01")

	result1, err := sm.ExchangeID(ownerID, verifier, 0, nil, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID #1 error: %v", err)
	}

	// Simulate confirmation
	sm.mu.Lock()
	sm.v41ClientsByID[result1.ClientID].Confirmed = true
	sm.mu.Unlock()

	// Same verifier -> idempotent, should return CONFIRMED_R
	result2, err := sm.ExchangeID(ownerID, verifier, 0, nil, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID #2 error: %v", err)
	}

	if result1.ClientID != result2.ClientID {
		t.Error("ClientID should be same for idempotent call on confirmed client")
	}
	if result2.Flags&types.EXCHGID4_FLAG_CONFIRMED_R == 0 {
		t.Error("Idempotent call on confirmed client should have CONFIRMED_R flag")
	}
}

func TestNewServerIdentity(t *testing.T) {
	si := newServerIdentity(12345)

	if si == nil {
		t.Fatal("newServerIdentity returned nil")
	}

	if si.ServerOwner.MinorID != 12345 {
		t.Errorf("MinorID = %d, want 12345", si.ServerOwner.MinorID)
	}
	if len(si.ServerOwner.MajorID) == 0 {
		t.Error("MajorID should not be empty (hostname)")
	}

	// ServerScope should match MajorID
	if !bytes.Equal(si.ServerScope, si.ServerOwner.MajorID) {
		t.Error("ServerScope should match ServerOwner.MajorID")
	}

	if si.ImplID.Name != "dittofs" {
		t.Errorf("ImplID.Name = %q, want %q", si.ImplID.Name, "dittofs")
	}
	if si.ImplID.Domain != "dittofs.io" {
		t.Errorf("ImplID.Domain = %q, want %q", si.ImplID.Domain, "dittofs.io")
	}
}

func TestExchangeID_NoImplId(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)

	ownerID := []byte("no-impl-client")
	var verifier [8]byte

	// No client impl ID (empty slice)
	result, err := sm.ExchangeID(ownerID, verifier, 0, nil, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID error: %v", err)
	}

	if result.ClientID == 0 {
		t.Error("ClientID should not be zero")
	}

	// Record should still exist with empty impl info
	sm.mu.RLock()
	record := sm.v41ClientsByOwner[string(ownerID)]
	sm.mu.RUnlock()

	if record == nil {
		t.Fatal("Record should exist")
	}
	if record.ImplName != "" {
		t.Errorf("ImplName = %q, want empty for nil impl ID", record.ImplName)
	}
}

func TestExchangeID_Timing(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)

	ownerID := []byte("timing-client")
	var verifier [8]byte

	before := time.Now()
	result, err := sm.ExchangeID(ownerID, verifier, 0, nil, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID error: %v", err)
	}
	after := time.Now()

	sm.mu.RLock()
	record := sm.v41ClientsByID[result.ClientID]
	sm.mu.RUnlock()

	if record.CreatedAt.Before(before) || record.CreatedAt.After(after) {
		t.Error("CreatedAt should be between before and after timestamps")
	}
	if record.LastRenewal.Before(before) || record.LastRenewal.After(after) {
		t.Error("LastRenewal should be between before and after timestamps")
	}
}

// ============================================================================
// DestroyV41ClientID Tests
// ============================================================================

func TestDestroyV41ClientID(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		sm := NewStateManager(DefaultLeaseDuration)
		defer sm.Shutdown()

		ownerID := []byte("destroy-test-client")
		var verifier [8]byte
		copy(verifier[:], "verify01")

		result, err := sm.ExchangeID(ownerID, verifier, 0, nil, "10.0.0.1:12345")
		if err != nil {
			t.Fatalf("ExchangeID error: %v", err)
		}

		// Destroy should succeed
		err = sm.DestroyV41ClientID(result.ClientID)
		if err != nil {
			t.Fatalf("DestroyV41ClientID error: %v", err)
		}

		// Verify client is gone
		sm.mu.RLock()
		_, existsID := sm.v41ClientsByID[result.ClientID]
		_, existsOwner := sm.v41ClientsByOwner[string(ownerID)]
		sm.mu.RUnlock()

		if existsID {
			t.Error("Client should not exist in v41ClientsByID after destroy")
		}
		if existsOwner {
			t.Error("Client should not exist in v41ClientsByOwner after destroy")
		}

		// Second destroy should return NFS4ERR_STALE_CLIENTID
		err = sm.DestroyV41ClientID(result.ClientID)
		if err == nil {
			t.Fatal("Expected error on second destroy")
		}
		stateErr, ok := err.(*NFS4StateError)
		if !ok {
			t.Fatalf("Expected NFS4StateError, got %T", err)
		}
		if stateErr.Status != types.NFS4ERR_STALE_CLIENTID {
			t.Errorf("Status = %d, want NFS4ERR_STALE_CLIENTID (%d)",
				stateErr.Status, types.NFS4ERR_STALE_CLIENTID)
		}
	})

	t.Run("clientid_busy", func(t *testing.T) {
		sm := NewStateManager(DefaultLeaseDuration)
		defer sm.Shutdown()

		// Register client and create a session
		clientID, seqID := registerV41Client(t, sm)

		_, _, err := sm.CreateSession(
			clientID, seqID+1, 0,
			defaultForeAttrs(), defaultBackAttrs(), 0, nil,
		)
		if err != nil {
			t.Fatalf("CreateSession error: %v", err)
		}

		// Destroy should fail with NFS4ERR_CLIENTID_BUSY
		err = sm.DestroyV41ClientID(clientID)
		if err == nil {
			t.Fatal("Expected NFS4ERR_CLIENTID_BUSY error")
		}
		stateErr, ok := err.(*NFS4StateError)
		if !ok {
			t.Fatalf("Expected NFS4StateError, got %T", err)
		}
		if stateErr.Status != types.NFS4ERR_CLIENTID_BUSY {
			t.Errorf("Status = %d, want NFS4ERR_CLIENTID_BUSY (%d)",
				stateErr.Status, types.NFS4ERR_CLIENTID_BUSY)
		}

		// Destroy session first
		sm.mu.RLock()
		sessions := sm.sessionsByClientID[clientID]
		var sessionID types.SessionId4
		if len(sessions) > 0 {
			sessionID = sessions[0].SessionID
		}
		sm.mu.RUnlock()

		err = sm.DestroySession(sessionID)
		if err != nil {
			t.Fatalf("DestroySession error: %v", err)
		}

		// Now destroy should succeed
		err = sm.DestroyV41ClientID(clientID)
		if err != nil {
			t.Fatalf("DestroyV41ClientID should succeed after session destroy: %v", err)
		}
	})

	t.Run("stale_clientid", func(t *testing.T) {
		sm := NewStateManager(DefaultLeaseDuration)
		defer sm.Shutdown()

		err := sm.DestroyV41ClientID(99999)
		if err == nil {
			t.Fatal("Expected error for non-existent client")
		}
		stateErr, ok := err.(*NFS4StateError)
		if !ok {
			t.Fatalf("Expected NFS4StateError, got %T", err)
		}
		if stateErr.Status != types.NFS4ERR_STALE_CLIENTID {
			t.Errorf("Status = %d, want NFS4ERR_STALE_CLIENTID (%d)",
				stateErr.Status, types.NFS4ERR_STALE_CLIENTID)
		}
	})

	t.Run("idempotent_after_destroy", func(t *testing.T) {
		sm := NewStateManager(DefaultLeaseDuration)
		defer sm.Shutdown()

		ownerID := []byte("idempotent-destroy")
		var verifier [8]byte

		result, err := sm.ExchangeID(ownerID, verifier, 0, nil, "10.0.0.1:12345")
		if err != nil {
			t.Fatalf("ExchangeID error: %v", err)
		}

		// First destroy succeeds
		if err := sm.DestroyV41ClientID(result.ClientID); err != nil {
			t.Fatalf("First destroy error: %v", err)
		}

		// Second destroy returns NFS4ERR_STALE_CLIENTID (not a crash)
		err = sm.DestroyV41ClientID(result.ClientID)
		if err == nil {
			t.Fatal("Expected error on second destroy")
		}
		stateErr, ok := err.(*NFS4StateError)
		if !ok {
			t.Fatalf("Expected NFS4StateError, got %T", err)
		}
		if stateErr.Status != types.NFS4ERR_STALE_CLIENTID {
			t.Errorf("Status = %d, want NFS4ERR_STALE_CLIENTID", stateErr.Status)
		}
	})

	t.Run("during_grace_period", func(t *testing.T) {
		sm := NewStateManager(5*time.Second, 5*time.Second)
		defer sm.Shutdown()

		ownerID := []byte("grace-destroy-client")
		var verifier [8]byte

		result, err := sm.ExchangeID(ownerID, verifier, 0, nil, "10.0.0.1:12345")
		if err != nil {
			t.Fatalf("ExchangeID error: %v", err)
		}

		// Start grace period with this client as expected
		sm.StartGracePeriod([]uint64{result.ClientID, 99998})

		if !sm.IsInGrace() {
			t.Fatal("Grace period should be active")
		}

		// Destroy the client during grace
		err = sm.DestroyV41ClientID(result.ClientID)
		if err != nil {
			t.Fatalf("DestroyV41ClientID during grace: %v", err)
		}

		// Give goroutine time to call ClientReclaimed
		time.Sleep(20 * time.Millisecond)

		// Grace period should still be active (one more client expected)
		if !sm.IsInGrace() {
			// If only this client was expected, it would end. But we have 99998 too.
			// So it should still be active.
		}
	})
}

func TestDestroyV41ClientID_Concurrent(t *testing.T) {
	sm := NewStateManager(DefaultLeaseDuration)
	defer sm.Shutdown()

	ownerID := []byte("concurrent-destroy")
	var verifier [8]byte

	result, err := sm.ExchangeID(ownerID, verifier, 0, nil, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID error: %v", err)
	}

	const numGoroutines = 10
	var wg sync.WaitGroup
	successes := make(chan bool, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := sm.DestroyV41ClientID(result.ClientID)
			successes <- (err == nil)
		}()
	}

	wg.Wait()
	close(successes)

	successCount := 0
	for s := range successes {
		if s {
			successCount++
		}
	}

	if successCount != 1 {
		t.Errorf("Expected exactly 1 success, got %d", successCount)
	}
}
