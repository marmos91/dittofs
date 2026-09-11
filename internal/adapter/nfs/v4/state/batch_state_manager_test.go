package state

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// Finding 1 — LockNew must not leak lock-owner / lock-state on bad seqid
// ============================================================================

// TestLockNew_BrandNewOwnerOpensSequenceAtRequestSeqid pins the initial-seqid
// rule for a brand-new lock-owner: the sequence opens at whatever seqid the
// request carries (the same rule nfsd applies to a stateowner it has never
// seen), and the lock-owner is registered with no orphaned state either way.
func TestLockNew_BrandNewOwnerOpensSequenceAtRequestSeqid(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	ownerData := []byte("new-owner")

	// A brand-new lock-owner opens its sequence at the request's seqid.
	res, err := sm.LockNew(context.Background(), clientID, ownerData, 99, openStateid, openSeqid+1, fileHandle, types.WRITE_LT, 0, 100, false, 0)
	if err != nil {
		t.Fatalf("LockNew on brand-new owner with seqid 99 failed: %v", err)
	}
	if res.Denied != nil {
		t.Fatal("expected no conflict on brand-new owner")
	}

	// The lock-owner must be registered, seeded at the request's seqid.
	loKey := makeLockOwnerKey(clientID, ownerData)
	owner := sm.lockOwners[loKey]
	if owner == nil {
		t.Fatal("lock owner must be registered after LockNew")
	}
	if owner.LastSeqID != 99 {
		t.Errorf("LastSeqID = %d, want 99 (the seqid the sequence opened with)", owner.LastSeqID)
	}

	// The sequence is now strict: LOCK at 1 is not the next seqid (100) nor a
	// replay, and must be rejected.
	_, err = sm.LockExisting(context.Background(), &res.Stateid, 1, fileHandle, types.WRITE_LT, 200, 100, false, 0)
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("LOCK at wrong seqid after the sequence opened: got %v, want NFS4StateError", err)
	}
	if stateErr.Status != types.NFS4ERR_BAD_SEQID {
		t.Errorf("status = %d, want NFS4ERR_BAD_SEQID (%d)", stateErr.Status, types.NFS4ERR_BAD_SEQID)
	}
}

// TestLockNew_BadLockSeqidDoesNotLeakLockState verifies that no LockState is
// appended to the open-state when a bad seqid is rejected for an EXISTING
// lock-owner. A subsequent valid LOCK must therefore allocate a fresh lock
// state (seqid starts at 1) rather than reuse a leaked one.
func TestLockNew_BadLockSeqidDoesNotLeakLockState(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	openState := sm.openStateByOther[openStateid.Other]
	if openState == nil {
		t.Fatal("open state not found")
	}

	ownerData := []byte("new-owner")

	// Brand-new lock-owner opens its sequence at the request's seqid.
	res, err := sm.LockNew(context.Background(), clientID, ownerData, 1, openStateid, openSeqid+1, fileHandle, types.WRITE_LT, 0, 100, false, 0)
	if err != nil {
		t.Fatalf("LockNew on brand-new owner failed: %v", err)
	}
	if len(openState.LockStates) != 1 {
		t.Fatalf("open state must have one lock state after LockNew, got %d", len(openState.LockStates))
	}

	// Bad seqid rejection for the now-EXISTING lock-owner: LOCK at 99 is
	// neither the next seqid (2) nor a replay, and must be rejected before any
	// state is inserted, so no second lock state leaks.
	if _, err := sm.LockNew(context.Background(), clientID, ownerData, 99, &res.Stateid, 2, fileHandle, types.WRITE_LT, 200, 100, false, 0); err == nil {
		t.Fatal("expected ErrBadSeqid for bad lock seqid on existing owner")
	}

	if len(openState.LockStates) != 1 {
		t.Fatalf("open state must have no new lock states after bad seqid, got %d", len(openState.LockStates))
	}

	// A valid follow-up LOCK (the next seqids) must now succeed cleanly. The
	// open stateid is unchanged by LOCK, so it is still the open's current one.
	res2, err := sm.LockNew(context.Background(), clientID, ownerData, 2, &openState.Stateid, openSeqid+2, fileHandle, types.WRITE_LT, 300, 100, false, 0)
	if err != nil {
		t.Fatalf("valid LockNew after rejection failed: %v", err)
	}
	if res2.Denied != nil {
		t.Fatal("expected no conflict for valid LOCK after rejection")
	}
	// The follow-up LOCK reuses the same lock state (second byte range): its
	// seqid advanced 2 -> 3; a leaked-then-recreated state would reset it.
	if res2.Stateid.Seqid != 3 {
		t.Errorf("lock stateid seqid = %d, want 3 (same state reused)", res2.Stateid.Seqid)
	}
}

// ============================================================================
// Finding 2 — lease-expiry reap must stop each session's backchannel sender
// ============================================================================

// TestReapExpiredSessions_StopsBackchannelSender verifies that when a v4.1
// client's lease expires, the session reaper stops the backchannel sender
// (closing its stopCh) instead of leaking the goroutine.
func TestReapExpiredSessions_StopsBackchannelSender(t *testing.T) {
	// Very short lease so it expires almost immediately.
	sm := NewStateManager(1 * time.Millisecond)
	clientID, seqID := registerV41Client(t, sm)

	csResult, _, err := sm.CreateSession(
		clientID, seqID, types.CREATE_SESSION4_FLAG_CONN_BACK_CHAN,
		defaultForeAttrs(), defaultBackAttrs(), 0x40000000, nil,
	)
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	// Plant a backchannel sender on the session whose Stop() we can observe.
	session := sm.GetSession(csResult.SessionID)
	if session == nil {
		t.Fatal("GetSession returned nil")
	}
	sender := NewBackchannelSender(csResult.SessionID, clientID, 0x40000000, session.BackChannelSlots, sm)
	sm.mu.Lock()
	session.backchannelSender = sender
	sm.mu.Unlock()
	go sender.Run(context.Background())

	// Let the lease expire, then run the reaper.
	time.Sleep(10 * time.Millisecond)
	sm.reapExpiredSessions()

	// The sender's stopCh must be closed (Stop was called by purgeV41Client).
	select {
	case <-sender.stopCh:
		// pass
	case <-time.After(1 * time.Second):
		t.Error("BackchannelSender.Stop not called after lease-expiry reap")
	}

	// And the session/client must be fully purged.
	if sm.GetSession(csResult.SessionID) != nil {
		t.Error("session should be destroyed after lease-expiry reap")
	}
}

// ============================================================================
// Finding 5 — acquireLock LOCK4denied must contain decoded owner bytes
// ============================================================================

// setupClientAndOpenStateForClient is a variant of setupClientAndOpenState
// that accepts a client name + file handle, so two distinct clients can open
// the same file in one test.
func setupClientAndOpenStateForClient(t *testing.T, sm *StateManager, clientName string, fileHandle []byte, verifier [8]byte) (clientID uint64, openStateid *types.Stateid4, openSeqid uint32) {
	t.Helper()

	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	result, err := sm.SetClientID(clientName, verifier, callback, "10.0.0.9:1234")
	if err != nil {
		t.Fatalf("SetClientID(%s) failed: %v", clientName, err)
	}
	clientID = result.ClientID

	if err := sm.ConfirmClientID(clientID, result.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID(%s) failed: %v", clientName, err)
	}

	openSeqid = 1
	openResult, err := sm.OpenFile(
		clientID,
		[]byte("open-owner-"+clientName),
		openSeqid,
		fileHandle,
		types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err != nil {
		t.Fatalf("OpenFile(%s) failed: %v", clientName, err)
	}

	confirmSeqid := openSeqid + 1
	confirmRes, err := sm.ConfirmOpen(&openResult.Stateid, confirmSeqid, 0)
	if err != nil {
		t.Fatalf("ConfirmOpen(%s) failed: %v", clientName, err)
	}

	return clientID, &confirmRes.Stateid, confirmSeqid
}

// TestAcquireLock_DeniedOwnerDataIsDecodedBytes verifies the LOCK4denied
// returned to a conflicting client carries the decoded opaque owner bytes and
// the conflicting client's ID, not the raw "nfs4:{id}:{hex}" format string.
func TestAcquireLock_DeniedOwnerDataIsDecodedBytes(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)

	fh := []byte("/export:denied-owner-file")
	verifierA := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	verifierB := [8]byte{9, 10, 11, 12, 13, 14, 15, 16}

	clientA, stateidA, seqA := setupClientAndOpenStateForClient(t, sm, "client-a", fh, verifierA)
	clientB, stateidB, seqB := setupClientAndOpenStateForClient(t, sm, "client-b", fh, verifierB)

	// Client A acquires an exclusive lock on [0, 100).
	ownerA := []byte("owner-a")
	if _, err := sm.LockNew(context.Background(), clientA, ownerA, 1, stateidA, seqA+1, fh, types.WRITE_LT, 0, 100, false, 0); err != nil {
		t.Fatalf("LockNew for A failed: %v", err)
	}

	// Client B requests an overlapping exclusive lock -> DENIED.
	resB, err := sm.LockNew(context.Background(), clientB, []byte("owner-b"), 1, stateidB, seqB+1, fh, types.WRITE_LT, 0, 100, false, 0)
	if err != nil {
		t.Fatalf("LockNew for B error: %v", err)
	}
	if resB.Denied == nil {
		t.Fatal("expected LOCK4denied for overlapping exclusive lock")
	}

	if !bytes.Equal(resB.Denied.Owner.OwnerData, ownerA) {
		t.Errorf("Denied.Owner.OwnerData = %q, want %q (decoded bytes)", resB.Denied.Owner.OwnerData, ownerA)
	}
	if resB.Denied.Owner.ClientID != clientA {
		t.Errorf("Denied.Owner.ClientID = %d, want %d", resB.Denied.Owner.ClientID, clientA)
	}
}

// ============================================================================
// Finding 3 — SETCLIENTID principal-mismatch must return CLID_INUSE
// ============================================================================

func TestSetClientID_PrincipalMismatchReturnsClientIDInUse(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	cb := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	r1, err := sm.SetClientID("hijack-target", verifier, cb, "10.0.0.1:1234", "uid:1000")
	if err != nil {
		t.Fatalf("SetClientID failed: %v", err)
	}
	if err := sm.ConfirmClientID(r1.ClientID, r1.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID failed: %v", err)
	}
	openClientState(t, sm, r1.ClientID, "hijack-owner", []byte("target-file"))

	// Case 5 (same verifier) from a DIFFERENT principal -> CLID_INUSE.
	_, err = sm.SetClientID("hijack-target", verifier, cb, "10.0.0.2:5678", "uid:9999")
	if !errors.Is(err, ErrClientIDInUse) {
		t.Errorf("expected ErrClientIDInUse, got %v", err)
	}
}

// TestSetClientID_PrincipalMismatchAllowedWithoutState pins the condition on
// that refusal. RFC 7530 Section 9.1.2 requires the SETCLIENTID to be allowed
// when the recorded client ID holds no state, because the rule the principal
// check enforces (Section 9.1.1) is a MUST NOT on cancelling *leased state*
// established by another principal, and there is none here to cancel.
//
// Without the condition, whichever principal touches a client id string first
// owns it for the life of the server: clients derive that string from the
// hostname, not from their credential, so a second user on the same host can
// never establish a client id at all.
func TestSetClientID_PrincipalMismatchAllowedWithoutState(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	cb := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	r1, err := sm.SetClientID("stateless-target", verifier, cb, "10.0.0.1:1234", "uid:1000")
	if err != nil {
		t.Fatalf("SetClientID failed: %v", err)
	}
	if err := sm.ConfirmClientID(r1.ClientID, r1.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID failed: %v", err)
	}

	if _, err := sm.SetClientID("stateless-target", verifier, cb, "10.0.0.2:5678", "uid:9999"); err != nil {
		t.Errorf("SETCLIENTID from a different principal against a stateless client ID: got %v, want success", err)
	}
}

// openClientState gives clientID a confirmed open so that it counts as holding
// leased state.
func openClientState(t *testing.T, sm *StateManager, clientID uint64, owner string, fh []byte) {
	t.Helper()

	if _, err := sm.OpenFile(
		clientID, []byte(owner), 1, fh,
		types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	); err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
}

func TestSetClientID_RebootPrincipalMismatchReturnsClientIDInUse(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	cb := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	r1, err := sm.SetClientID("hijack-target", verifier, cb, "10.0.0.1:1234", "uid:1000")
	if err != nil {
		t.Fatalf("SetClientID failed: %v", err)
	}
	if err := sm.ConfirmClientID(r1.ClientID, r1.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID failed: %v", err)
	}
	openClientState(t, sm, r1.ClientID, "reboot-hijack-owner", []byte("reboot-target-file"))

	// Case 3 (different verifier = reboot) from a DIFFERENT principal -> CLID_INUSE.
	verifier2 := [8]byte{9, 10, 11, 12, 13, 14, 15, 16}
	_, err = sm.SetClientID("hijack-target", verifier2, cb, "10.0.0.2:5678", "uid:9999")
	if !errors.Is(err, ErrClientIDInUse) {
		t.Errorf("expected ErrClientIDInUse for reboot from different principal, got %v", err)
	}
}

func TestSetClientID_SamePrincipalAllowed(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	cb := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	r1, err := sm.SetClientID("same-principal", verifier, cb, "10.0.0.1:1234", "uid:1000")
	if err != nil {
		t.Fatalf("SetClientID failed: %v", err)
	}
	if err := sm.ConfirmClientID(r1.ClientID, r1.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID failed: %v", err)
	}

	// Re-SETCLIENTID from the SAME principal (Case 5) must still succeed.
	if _, err := sm.SetClientID("same-principal", verifier, cb, "10.0.0.1:1234", "uid:1000"); err != nil {
		t.Errorf("re-SETCLIENTID from same principal should succeed, got %v", err)
	}

	// Reboot (Case 3) from the SAME principal must also succeed.
	verifier2 := [8]byte{9, 10, 11, 12, 13, 14, 15, 16}
	if _, err := sm.SetClientID("same-principal", verifier2, cb, "10.0.0.1:1234", "uid:1000"); err != nil {
		t.Errorf("reboot from same principal should succeed, got %v", err)
	}
}

// ============================================================================
// Finding 4 — CB_NULL goroutine must not corrupt a replaced record
// ============================================================================

// TestConfirmClientID_CBNullDoesNotUpdateReplacedRecord verifies the CB_NULL
// goroutine launched by SETCLIENTID_CONFIRM compares record pointer identity
// before writing CBPathUp. If the map entry for the client ID was swapped to a
// different record generation while CB_NULL was in flight, the stale goroutine
// must NOT touch the replacement.
//
// Determinism: the sendCBNull hook blocks until the test swaps
// clientsByID[clientID] to a fresh record generation, then returns err==nil
// (a "successful" CB path). Absent the pointer-identity guard, the stale
// goroutine reads the swapped-in record from the map and writes CBPathUp=true
// onto it -- the WRONG generation. The replacement's sentinel starts false, so
// such a write is detectable. With the guard, the goroutine sees the map slot
// no longer holds its captured pointer and returns without writing.
func TestConfirmClientID_CBNullDoesNotUpdateReplacedRecord(t *testing.T) {
	released := make(chan struct{})
	entered := make(chan struct{})
	done := make(chan struct{})

	sm := NewStateManager(90 * time.Second)
	// Override the per-manager CB_NULL hook (set before any goroutine runs).
	sm.cbNullFunc = func(_ context.Context, _ CallbackInfo) error {
		close(entered)
		<-released
		return nil // "success" -- buggy path would set CBPathUp=true
	}

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	cb := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.0.1"}

	r1, err := sm.SetClientID("cb-stale", verifier, cb, "10.0.0.1:1234", "uid:1000")
	if err != nil {
		t.Fatalf("SetClientID failed: %v", err)
	}
	// CONFIRM launches the CB_NULL goroutine capturing this record's pointer.
	if err := sm.ConfirmClientID(r1.ClientID, r1.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID failed: %v", err)
	}

	// Wait until the goroutine entered the hook (pointer already captured).
	<-entered

	oldRec := sm.GetClient(r1.ClientID)
	if oldRec == nil {
		t.Fatal("confirmed record not found")
	}

	// Swap the map slot to a fresh record generation at the SAME client ID.
	// Sentinel CBPathUp=false: the stale goroutine must NOT flip it to true.
	replacement := &ClientRecord{
		ClientID:       r1.ClientID,
		ClientIDString: oldRec.ClientIDString,
		Confirmed:      true,
		CBPathUp:       false,
	}
	sm.mu.Lock()
	sm.clientsByID[r1.ClientID] = replacement
	sm.mu.Unlock()

	// Release the hook; signal completion from a watcher so we can join.
	go func() {
		close(released)
		// Best-effort: give the goroutine time to run its critical section.
		time.Sleep(200 * time.Millisecond)
		close(done)
	}()
	<-done

	sm.mu.Lock()
	defer sm.mu.Unlock()
	if replacement.CBPathUp {
		t.Error("stale CB_NULL goroutine wrote CBPathUp onto the replacement (wrong) record generation")
	}
}
