package state

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// These tests are negative controls for the NFSv4/4.1 state & stateid audit
// fixes (issue #1128). Each one fails against the pre-fix code.

// newLockedFile opens, confirms, and acquires a WRITE lock for the given
// client, returning the lock stateid and the locked file handle. It is the
// shared setup for the lock-stateid and FREE_STATEID ownership tests.
func newLockedFile(t *testing.T, sm *StateManager, clientID uint64, fh []byte) types.Stateid4 {
	t.Helper()
	openResult, err := sm.OpenFile(clientID, []byte("open-owner"), 1, fh,
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	confirmed, err := sm.ConfirmOpen(&openResult.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}
	lockResult, err := sm.LockNew(
		clientID, []byte("lock-owner"), 1,
		&confirmed.Stateid, 3,
		fh, types.WRITE_LT, 0, 100, false,
	)
	if err != nil {
		t.Fatalf("LockNew: %v", err)
	}
	if lockResult.Denied != nil {
		t.Fatalf("LockNew unexpectedly denied")
	}
	return lockResult.Stateid
}

// TestValidateStateid_LockStateid_RoutedToLockMap is the negative control for
// the lock-stateid routing fix (stateid.go). RFC 7530 Section 9.1.4.1 permits
// a lock stateid on READ/WRITE; before the fix ValidateStateid looked lock
// stateids up in openStateByOther and returned NFS4ERR_BAD_STATEID.
func TestValidateStateid_LockStateid_RoutedToLockMap(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	defer sm.Shutdown()

	fh := []byte("fh-lock-stateid-io")
	lockStateid := newLockedFile(t, sm, 0, fh)

	// A lock stateid presented to WRITE must validate and return the parent
	// open state (carrying the share-access bits the caller enforces).
	openState, err := sm.ValidateStateid(&lockStateid, fh, StateidOpWrite)
	if err != nil {
		t.Fatalf("ValidateStateid on lock stateid (WRITE): %v", err)
	}
	if openState == nil {
		t.Fatal("lock stateid must return its parent open state, got nil")
	}
	if openState.ShareAccess&types.OPEN4_SHARE_ACCESS_WRITE == 0 {
		t.Errorf("parent open state ShareAccess = %d, missing WRITE", openState.ShareAccess)
	}

	// Same lock stateid must also validate on READ.
	if _, err := sm.ValidateStateid(&lockStateid, fh, StateidOpRead); err != nil {
		t.Fatalf("ValidateStateid on lock stateid (READ): %v", err)
	}
}

// TestValidateStateid_LockStateid_Seqid0 confirms the RFC 8881 Section 8.2.2
// seqid=0 "current stateid" bypass works for lock stateids too.
func TestValidateStateid_LockStateid_Seqid0(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	defer sm.Shutdown()

	fh := []byte("fh-lock-stateid-seqid0")
	lockStateid := newLockedFile(t, sm, 0, fh)
	lockStateid.Seqid = 0

	if _, err := sm.ValidateStateid(&lockStateid, fh, StateidOpRead); err != nil {
		t.Fatalf("ValidateStateid on lock stateid with seqid=0: %v", err)
	}
}

// TestFreeStateid_CrossClientLock is the negative control for the FREE_STATEID
// lock ownership fix. RFC 8881 Section 18.38.3 requires the stateid to belong
// to the requesting client; before the fix any client could free another
// client's lock stateid.
func TestFreeStateid_CrossClientLock(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	defer sm.Shutdown()

	const victimClientID = uint64(1)
	const attackerClientID = uint64(2)

	fh := []byte("fh-cross-client-lock")
	lockStateid := newLockedFile(t, sm, victimClientID, fh)

	// Attacker tries to free the victim's lock stateid.
	err := sm.FreeStateid(attackerClientID, &lockStateid)
	if err == nil {
		t.Fatal("FREE_STATEID by a different client must be rejected")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_BAD_STATEID {
		t.Errorf("status = %d, want NFS4ERR_BAD_STATEID", stateErr.Status)
	}

	// The victim's lock state must still exist.
	sm.mu.RLock()
	_, exists := sm.lockStateByOther[lockStateid.Other]
	sm.mu.RUnlock()
	if !exists {
		t.Error("victim's lock stateid was destroyed by another client")
	}

	// The owning client can still free it.
	if err := sm.FreeStateid(victimClientID, &lockStateid); err != nil {
		t.Fatalf("FreeStateid by owning client: %v", err)
	}
}

// TestFreeStateid_CrossClientOpen is the negative control for the FREE_STATEID
// open ownership fix.
func TestFreeStateid_CrossClientOpen(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	const victimClientID = uint64(1)
	const attackerClientID = uint64(2)

	fh := []byte("fh-cross-client-open")
	openResult, err := sm.OpenFile(victimClientID, []byte("owner1"), 1, fh,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := sm.ConfirmOpen(&openResult.Stateid, 2); err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}

	err = sm.FreeStateid(attackerClientID, &openResult.Stateid)
	if err == nil {
		t.Fatal("FREE_STATEID of another client's open stateid must be rejected")
	}
	if stateErr, ok := err.(*NFS4StateError); !ok || stateErr.Status != types.NFS4ERR_BAD_STATEID {
		t.Errorf("expected NFS4ERR_BAD_STATEID, got %v", err)
	}
	if sm.GetOpenState(openResult.Stateid.Other) == nil {
		t.Error("victim's open stateid was destroyed by another client")
	}

	// Owning client can still free it.
	if err := sm.FreeStateid(victimClientID, &openResult.Stateid); err != nil {
		t.Fatalf("FreeStateid by owning client: %v", err)
	}
}

// TestFreeStateid_CrossClientDelegation is the negative control for the
// FREE_STATEID delegation ownership fix.
func TestFreeStateid_CrossClientDelegation(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	const victimClientID = uint64(100)
	const attackerClientID = uint64(200)

	deleg := sm.GrantDelegation(victimClientID, []byte("fh-cross-client-deleg"), types.OPEN_DELEGATE_WRITE)

	err := sm.FreeStateid(attackerClientID, &deleg.Stateid)
	if err == nil {
		t.Fatal("FREE_STATEID of another client's delegation must be rejected")
	}
	if stateErr, ok := err.(*NFS4StateError); !ok || stateErr.Status != types.NFS4ERR_BAD_STATEID {
		t.Errorf("expected NFS4ERR_BAD_STATEID, got %v", err)
	}

	sm.mu.RLock()
	_, exists := sm.delegByOther[deleg.Stateid.Other]
	sm.mu.RUnlock()
	if !exists {
		t.Error("victim's delegation was destroyed by another client")
	}

	if err := sm.FreeStateid(victimClientID, &deleg.Stateid); err != nil {
		t.Fatalf("FreeStateid by owning client: %v", err)
	}
}

// TestLockExisting_V41Seqid0 is the negative control for the LockExisting
// stateid seqid=0 bypass (manager.go). A v4.1 client sends stateid seqid=0;
// before the fix `0 < lockState.Stateid.Seqid` always returned
// NFS4ERR_OLD_STATEID, making every additional LOCK fail.
func TestLockExisting_V41Seqid0(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	defer sm.Shutdown()

	fh := []byte("fh-lockexisting-seqid0")
	lockStateid := newLockedFile(t, sm, 0, fh)

	// v4.1 client: stateid seqid=0, owner seqid=0 (slot table provides replay
	// protection). Extend the lock with a second byte range via LockExisting.
	v41Stateid := lockStateid
	v41Stateid.Seqid = 0
	result, err := sm.LockExisting(&v41Stateid, 0, fh, types.WRITE_LT, 200, 100, false)
	if err != nil {
		t.Fatalf("LockExisting with v4.1 stateid seqid=0: %v", err)
	}
	if result.Denied != nil {
		t.Fatalf("LockExisting unexpectedly denied")
	}
}

// TestUnlockFile_V41Seqid0 is the negative control for the UnlockFile (LOCKU)
// stateid seqid=0 bypass. After a prior LOCK incremented lockState.Stateid.Seqid,
// a v4.1 LOCKU (stateid seqid=0) always returned NFS4ERR_OLD_STATEID before the
// fix, making unlock impossible.
func TestUnlockFile_V41Seqid0(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	defer sm.Shutdown()

	fh := []byte("fh-locku-seqid0")
	lockStateid := newLockedFile(t, sm, 0, fh)

	// Advance lockState.Stateid.Seqid to >=2 via a successful LockExisting so
	// the LOCKU below is unambiguously after a seqid increment.
	v41Lock := lockStateid
	v41Lock.Seqid = 0
	if _, err := sm.LockExisting(&v41Lock, 0, fh, types.WRITE_LT, 200, 100, false); err != nil {
		t.Fatalf("LockExisting setup: %v", err)
	}

	// v4.1 LOCKU: stateid seqid=0, owner seqid=0.
	unlockStateid := lockStateid
	unlockStateid.Seqid = 0
	if _, err := sm.UnlockFile(&unlockStateid, 0, types.WRITE_LT, 0, 100); err != nil {
		t.Fatalf("UnlockFile with v4.1 stateid seqid=0: %v", err)
	}
}

// TestExchangeID_Case2_PrincipalMismatch is the negative control for the
// EXCHANGE_ID Case 2 principal-mismatch fix (v41_client.go). RFC 8881
// Section 18.35.4 requires NFS4ERR_CLID_INUSE when the existing record's
// principal differs from the incoming one. Before the fix Case 2 unconditionally
// overwrote the principal, allowing a peer that knows the owner ID + verifier to
// hijack the client.
func TestExchangeID_Case2_PrincipalMismatch(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	ownerID := []byte("client-owner-principal")
	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}

	// Establish the record under principal "alice".
	if _, err := sm.ExchangeID(ownerID, verifier, 0, nil, "10.0.0.1:12345", "alice"); err != nil {
		t.Fatalf("ExchangeID (alice): %v", err)
	}

	// A peer that knows the owner ID + verifier replays EXCHANGE_ID under a
	// different principal -- must be rejected with NFS4ERR_CLID_INUSE.
	_, err := sm.ExchangeID(ownerID, verifier, 0, nil, "10.0.0.9:12345", "mallory")
	if err == nil {
		t.Fatal("EXCHANGE_ID Case 2 with a different principal must be rejected")
	}
	if err != ErrClientIDInUse {
		t.Errorf("expected ErrClientIDInUse, got %v", err)
	}

	// The stored principal must remain "alice".
	sm.mu.RLock()
	rec := sm.v41ClientsByOwner[string(ownerID)]
	sm.mu.RUnlock()
	if rec == nil || rec.Principal != "alice" {
		t.Errorf("stored principal = %q, want \"alice\" (hijack not prevented)", recPrincipal(rec))
	}

	// The legitimate principal can still EXCHANGE_ID idempotently.
	if _, err := sm.ExchangeID(ownerID, verifier, 0, nil, "10.0.0.1:12345", "alice"); err != nil {
		t.Fatalf("idempotent EXCHANGE_ID by owning principal: %v", err)
	}

	// An idempotent call with no principal (e.g. AUTH_NONE) must not clear the
	// stored principal — otherwise the hijack guard could be reset.
	if _, err := sm.ExchangeID(ownerID, verifier, 0, nil, "10.0.0.1:12345"); err != nil {
		t.Fatalf("idempotent EXCHANGE_ID with empty principal: %v", err)
	}
	sm.mu.RLock()
	rec = sm.v41ClientsByOwner[string(ownerID)]
	sm.mu.RUnlock()
	if rec == nil || rec.Principal != "alice" {
		t.Errorf("empty-principal call cleared stored principal: got %q", recPrincipal(rec))
	}
}

func recPrincipal(rec *V41ClientRecord) string {
	if rec == nil {
		return ""
	}
	return rec.Principal
}
