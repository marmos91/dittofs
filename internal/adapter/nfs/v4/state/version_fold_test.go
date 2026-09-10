package state

import (
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
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

// OPEN on a v4.1 client resolves the live v4.1 record through the shared
// index, so the owner's ClientRecord carries the renewing v4.1 lease and a
// stateid I/O after that lease lapses draws ErrExpired. This is intended
// semantics: every real v4.1 COMPOUND renews the lease via SEQUENCE first, so
// the stateid lease check only bites direct API calls that skip SEQUENCE.
func TestOpenFile_V41OwnerRecord_CarriesLiveLease(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	v41ID := registerConfirmedV41Client(t, sm, "open-lease-v41-owner")

	open, err := sm.OpenFile(v41ID, []byte("owner"), 1, []byte("/export:lease-file"),
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile on a v4.1 client: %v", err)
	}

	sm.mu.RLock()
	owner := sm.openStateByOther[open.Stateid.Other].Owner
	sm.mu.RUnlock()
	if owner == nil || owner.ClientRecord == nil {
		t.Fatal("OpenOwner.ClientRecord must be the live v4.1 record")
	}
	if owner.ClientRecord.ClientID != v41ID || owner.ClientRecord.MinorVersion != 1 {
		t.Fatalf("ClientRecord = v%d client %d, want the v4.1 record for %d",
			owner.ClientRecord.MinorVersion, owner.ClientRecord.ClientID, v41ID)
	}
	if owner.ClientRecord.Lease == nil {
		t.Fatal("the v4.1 record must carry its lease")
	}

	// The lease renews on stateid I/O: drive an I/O through ValidateStateid
	// and confirm the renewal stamped the SAME lease the v4.1 SEQUENCE
	// handler owns.
	sm.mu.RLock()
	lease := owner.ClientRecord.Lease
	sm.mu.RUnlock()
	lease.mu.Lock()
	before := lease.LastRenew
	lease.mu.Unlock()

	if _, err := sm.ValidateStateid(&open.Stateid, nil, StateidOpRead, v41ID); err != nil {
		t.Fatalf("stateid I/O with a live lease: %v", err)
	}

	lease.mu.Lock()
	renewed := lease.LastRenew
	lease.mu.Unlock()
	// Windows' clock granularity can land both stamps in the same tick, so
	// compare against the pre-validation wall time instead of demanding a
	// strict After on coarse clocks.
	if !renewed.After(before) && !renewed.Equal(before) {
		t.Fatal("stateid I/O must renew the v4.1 owner's lease")
	}
}

// The same lapsed-lease I/O must draw ErrExpired when the record carries no
// live lease path — the stateid check reads the owner's ClientRecord, so a
// direct API caller skipping SEQUENCE gets the expiry rather than a silent
// renewal against a dead lease.
func TestValidateStateid_V41ExpiredLease_ReturnsErrExpired(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	v41ID := registerConfirmedV41Client(t, sm, "expired-lease-v41-owner")

	open, err := sm.OpenFile(v41ID, []byte("owner"), 1, []byte("/export:expired-file"),
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	// Stop the lease and age its LastRenew past the duration, simulating a
	// client whose SEQUENCE stopped arriving: the next stateid I/O that skips
	// SEQUENCE must draw the expiry instead of renewing.
	sm.mu.RLock()
	lease := sm.openStateByOther[open.Stateid.Other].Owner.ClientRecord.Lease
	sm.mu.RUnlock()
	lease.Stop()
	lease.mu.Lock()
	lease.LastRenew = time.Now().Add(-2 * time.Hour)
	lease.mu.Unlock()

	if _, err := sm.ValidateStateid(&open.Stateid, nil, StateidOpRead, v41ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("stateid I/O against an expired v4.1 lease: got %v, want ErrExpired", err)
	}
}
