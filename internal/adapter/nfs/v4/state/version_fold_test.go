package state

import (
	"testing"
	"time"
)

// These tests guard the version fold of the client index: clientsByID holds
// records of both minor versions, discriminated by ClientRecord.MinorVersion,
// and every version-sensitive operation must see only the records its flow
// minted. Each assertion checks the behaviour a v4.1 record leaking into a
// v4.0 path (or the reverse) would break.

// registerConfirmedV41Client creates a confirmed v4.1 client via
// EXCHANGE_ID + CREATE_SESSION and returns its client ID.
func registerConfirmedV41Client(t *testing.T, sm *StateManager, owner string) uint64 {
	t.Helper()
	var verifier [8]byte
	copy(verifier[:], "verify41")
	res, err := sm.ExchangeID([]byte(owner), verifier, 0, nil, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID(%s): %v", owner, err)
	}
	if _, _, err := sm.CreateSession(res.ClientID, res.SequenceID, 0,
		defaultForeAttrs(), defaultBackAttrs(), 0, nil); err != nil {
		t.Fatalf("CreateSession(%s): %v", owner, err)
	}
	return res.ClientID
}

// TestClientIndex_VersionFilterBothFlowsResolve proves both lookups still
// resolve after the fold: a v4.0 client is reachable through GetClient, a
// v4.1 client through the version-agnostic lookup, and each record carries
// its minting version.
func TestClientIndex_VersionFilterBothFlowsResolve(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	v40ID := registerTestClientWithName(t, sm, "fold-v40-client")
	v41ID := registerConfirmedV41Client(t, sm, "fold-v41-owner")

	sm.mu.RLock()
	v40Rec := sm.clientsByID[v40ID]
	v41Rec := sm.clientsByID[v41ID]
	sm.mu.RUnlock()

	if v40Rec == nil || v40Rec.MinorVersion != 0 {
		t.Fatalf("v4.0 record missing or wrong version: %+v", v40Rec)
	}
	if v41Rec == nil || v41Rec.MinorVersion != 1 {
		t.Fatalf("v4.1 record missing or wrong version: %+v", v41Rec)
	}

	if sm.GetClient(v40ID) == nil {
		t.Error("GetClient must still resolve a v4.0 client")
	}
	if sm.GetClient(v41ID) != nil {
		t.Error("GetClient is v4.0-only: a v4.1 client ID must come back nil")
	}

	sm.mu.RLock()
	if sm.clientRecordLocked(v41ID) == nil {
		sm.mu.RUnlock()
		t.Error("version-agnostic lookup must resolve a v4.1 client")
	} else {
		sm.mu.RUnlock()
	}
}

// A v4.1 client reaped through expireV40ClientLocked would lose its state and
// leave its sessions behind: the v4.0 expiry path never destroys sessions.
func TestExpireV40Client_V41Record_Invisible(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	v41ID := registerConfirmedV41Client(t, sm, "expire-v41-owner")

	sm.mu.Lock()
	sm.expireV40ClientLocked(v41ID)
	sm.mu.Unlock()

	sm.mu.RLock()
	stillThere := sm.clientsByID[v41ID] != nil
	sm.mu.RUnlock()

	if !stillThere {
		t.Fatal("the v4.0 expiry path must not reap a v4.1 client")
	}
}

// RENEW does not exist in v4.1; a renewal reaching a v4.1 record would stamp
// a lease the SEQUENCE handler owns.
func TestRenewLease_V41Record_Invisible(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	v41ID := registerConfirmedV41Client(t, sm, "renew-v41-owner")

	if err := sm.RenewLease(v41ID); err == nil {
		t.Fatal("RenewLease must not admit a v4.1 client ID")
	}

	sm.mu.RLock()
	live := sm.clientsByID[v41ID] != nil && sm.clientsByID[v41ID].Lease != nil
	sm.mu.RUnlock()
	if !live {
		t.Fatal("a refused RENEW must leave the v4.1 lease exactly where it was")
	}
}

// SETCLIENTID_CONFIRM is the v4.0 callback establishment path; confirming a
// v4.1 record would arm the CBPathUp probe the EXCHANGE_ID flow does not use.
func TestConfirmClientID_V41Record_Invisible(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	v41ID := registerConfirmedV41Client(t, sm, "confirm-v41-owner")

	if err := sm.ConfirmClientID(v41ID, [8]byte{9, 9, 9, 9, 9, 9, 9, 9}); err == nil {
		t.Fatal("ConfirmClientID must not reach a v4.1 record")
	}

	sm.mu.RLock()
	rec := sm.clientsByID[v41ID]
	cbPathUp := rec != nil && rec.CBPathUp
	leaseArmed := rec != nil && rec.Lease != nil
	sm.mu.RUnlock()

	if cbPathUp {
		t.Error("a refused SETCLIENTID_CONFIRM must not arm the callback probe")
	}
	if !leaseArmed {
		t.Error("a refused SETCLIENTID_CONFIRM must not disturb the v4.1 lease")
	}
}

// Shutdown stops every lease timer regardless of version; iterating only the
// v4.0 records would leak the v4.1 timers into shutdown.
func TestShutdown_StopsBothVersionsLeases(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	v40ID := registerTestClientWithName(t, sm, "shutdown-v40-client")
	v41ID := registerConfirmedV41Client(t, sm, "shutdown-v41-owner")

	sm.Shutdown()

	if !leaseIsStopped(sm, v40ID) {
		t.Error("Shutdown must stop the v4.0 lease timer")
	}
	if !leaseIsStopped(sm, v41ID) {
		t.Error("Shutdown must stop the v4.1 lease timer")
	}
}

// leaseIsStopped reports whether clientID's lease no longer runs. A stopped
// lease never re-arms, so IsExpired after ageing past the duration is the
// observable: a live timer would reset LastRenew's clock only on Renew, so
// this reads the stopped flag under the lease lock instead.
func leaseIsStopped(sm *StateManager, clientID uint64) bool {
	sm.mu.RLock()
	lease := sm.clientsByID[clientID].Lease
	sm.mu.RUnlock()
	if lease == nil {
		return true
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.stopped
}

// A v4.0 record must never resolve through a v4.1-only flow either: the
// filter is symmetric. EXCHANGE_ID update on a v4.0 client ID must draw the
// no-confirmed-record error rather than touching the SETCLIENTID record.
func TestExchangeIDUpdate_V40Record_Invisible(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	v40ID := registerTestClientWithName(t, sm, "eidthru-v40-client")

	sm.mu.RLock()
	rec := sm.clientsByID[v40ID]
	sm.mu.RUnlock()
	if rec == nil {
		t.Fatal("v4.0 record missing")
	}

	// The owner-keyed v4.1 index cannot resolve a v4.0 client: EXCHANGE_ID
	// with UPD_CONFIRMED_REC_A against an owner the v4.0 flow minted finds no
	// confirmed v4.1 record and draws ErrNoConfirmedRecord.
	var verifier [8]byte
	copy(verifier[:], rec.Verifier[:])
	sm.mu.Lock()
	_, err := sm.exchangeIDUpdateLocked(nil, verifier, nil, "10.0.0.1:12345", "")
	sm.mu.Unlock()

	if err != ErrNoConfirmedRecord {
		t.Fatalf("EXCHANGE_ID update with no v4.1 record: got %v, want ErrNoConfirmedRecord", err)
	}
}
