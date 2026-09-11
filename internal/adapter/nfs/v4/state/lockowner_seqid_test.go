package state

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// The lock-owner seqid of zero must be validated like any other value on a
// v4.0 client. The v4.1 session path is what zeroes per-owner sequencing (the
// slot table provides replay protection there), and the handler threads that
// intent into the state manager per op; a v4.0 client's own zero is an
// ordinary seqid. pynfs LKU6b exercises exactly this: LOCK (new lock-owner,
// lock seqid 0), LOCK (lock seqid 1), then LOCKU with lock seqid 0 — the
// owner's sequence has reached 1, so the LOCKU is neither the next seqid nor a
// replay and NFS4ERR_BAD_SEQID is the answer.

// TestUnlockFile_ZeroSeqidOnV40IsRejected pins that LOCKU with seqid 0 on an
// existing lock-owner (no skip requested, i.e. the v4.0 path) is rejected
// NFS4ERR_BAD_SEQID rather than succeeding through the bypass.
func TestUnlockFile_ZeroSeqidOnV40IsRejected(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	defer sm.Shutdown()

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	// LOCK #1 via open_to_lock_owner: brand-new lock-owner opens its sequence
	// at the seqid the request carries (0 here, pynfs's own first LOCK does).
	lockRes, err := sm.LockNew(context.Background(), clientID, []byte("lku6b-owner"), 0, openStateid, openSeqid+1, fileHandle, types.WRITE_LT, 0, 50, false, 0)
	if err != nil {
		t.Fatalf("LockNew with lock seqid 0 (brand-new owner) failed: %v", err)
	}

	// LOCK #2 via exist_lock_owner: advances the lock-owner sequence to 1 and
	// the lock stateid to 2; the retransmit must carry the CURRENT stateid.
	res2, err := sm.LockExisting(context.Background(), &lockRes.Stateid, 1, fileHandle, types.WRITE_LT, 100, 50, false, 0)
	if err != nil {
		t.Fatalf("LockExisting failed: %v", err)
	}

	// LOCKU with lock seqid 0 against the CURRENT lock stateid: the owner's
	// last seqid is 1, so 0 is neither the next seqid (2) nor a replay. On the
	// v4.0 path (no skip) this must be rejected NFS4ERR_BAD_SEQID; the old
	// seqid!=0 bypass let it succeed (pynfs LKU6b).
	_, err = sm.UnlockFile(&res2.Stateid, 0, types.WRITE_LT, 0, 50, 0)
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("LOCKU with lock seqid 0 on v4.0: got %v, want NFS4StateError", err)
	}
	if stateErr.Status != types.NFS4ERR_BAD_SEQID {
		t.Errorf("status = %d, want NFS4ERR_BAD_SEQID (%d)", stateErr.Status, types.NFS4ERR_BAD_SEQID)
	}
}

// TestUnlockFile_ZeroSeqidWithSkipStillBypasses pins that the v4.1 skip — a
// session client (EXCHANGE_ID flow, MinorVersion 1) as the op's caller — still
// bypasses owner sequencing for seqid 0 exactly as today: no validation, the
// LOCKU succeeds.
func TestUnlockFile_ZeroSeqidWithSkipStillBypasses(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	defer sm.Shutdown()

	// The open state must belong to the v4.1 session client: the LOCKU caller
	// client ID is both the bearer check and the skip derivation seam.
	v41ClientID := registerConfirmedV41Client(t, sm, "v41-skip-owner")
	fileHandle := []byte("/export:test-file-001")
	openRes, err := sm.OpenFile(v41ClientID, []byte("v41-open-owner"), 0, fileHandle,
		types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	if err := sm.ConfirmOpenV41(&openRes.Stateid, v41ClientID); err != nil {
		t.Fatalf("ConfirmOpenV41 failed: %v", err)
	}

	// LOCK #1 via open_to_lock_owner with the session client as caller.
	lockRes, err := sm.LockNew(context.Background(), v41ClientID, []byte("v41-lock-owner"), 0, &openRes.Stateid, 1, fileHandle, types.WRITE_LT, 0, 50, false, v41ClientID)
	if err != nil {
		t.Fatalf("LockNew failed: %v", err)
	}
	res2, err := sm.LockExisting(context.Background(), &lockRes.Stateid, 1, fileHandle, types.WRITE_LT, 100, 50, false, v41ClientID)
	if err != nil {
		t.Fatalf("LockExisting failed: %v", err)
	}

	// With the v4.1 session client as caller, seqid 0 bypasses owner sequencing
	// and the LOCKU succeeds.
	if _, err := sm.UnlockFile(&res2.Stateid, 0, types.WRITE_LT, 0, 50, v41ClientID); err != nil {
		t.Fatalf("LOCKU with lock seqid 0 under the v4.1 skip must succeed: %v", err)
	}
}

// TestLockNew_ZeroSeqidOpensSequence pins nfsd's initial-seqid rule for a
// brand-new lock-owner: the sequence starts at whatever seqid the client opens
// it with, so LOCK with lock seqid 0 succeeds and the owner's LastSeqID is 0.
// A later LOCK must then expect 1 (validated) and reject anything else.
func TestLockNew_ZeroSeqidOpensSequence(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	defer sm.Shutdown()

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	lockRes, err := sm.LockNew(context.Background(), clientID, []byte("seed-owner"), 0, openStateid, openSeqid+1, fileHandle, types.WRITE_LT, 0, 50, false, 0)
	if err != nil {
		t.Fatalf("LockNew with lock seqid 0 (brand-new owner) failed: %v", err)
	}

	loKey := makeLockOwnerKey(clientID, []byte("seed-owner"))
	owner := sm.lockOwners[loKey]
	if owner == nil {
		t.Fatal("lock owner not registered")
	}
	if owner.LastSeqID != 0 {
		t.Errorf("LastSeqID = %d, want 0 (the seqid the sequence opened with)", owner.LastSeqID)
	}

	// The sequence is now strict: LOCK at 1 (the next seqid, on the current
	// stateid) succeeds...
	res2, err := sm.LockExisting(context.Background(), &lockRes.Stateid, 1, fileHandle, types.WRITE_LT, 100, 50, false, 0)
	if err != nil {
		t.Fatalf("LockExisting at seqid 1 failed: %v", err)
	}
	// ...and LOCK at 0 again against the CURRENT stateid is a bad seqid: the
	// owner's sequence has reached 1, so 0 is neither the next seqid (2) nor a
	// replay of 1.
	_, err = sm.LockExisting(context.Background(), &res2.Stateid, 0, fileHandle, types.WRITE_LT, 150, 50, false, 0)
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("LOCK at seqid 0 after the sequence advanced: got %v, want NFS4StateError", err)
	}
	if stateErr.Status != types.NFS4ERR_BAD_SEQID {
		t.Errorf("status = %d, want NFS4ERR_BAD_SEQID (%d)", stateErr.Status, types.NFS4ERR_BAD_SEQID)
	}
}
