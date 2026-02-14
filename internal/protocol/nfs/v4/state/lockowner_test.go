package state

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// LockOwner.ValidateSeqID Tests
// ============================================================================

func TestLockOwnerValidateSeqID_OK(t *testing.T) {
	lo := &LockOwner{LastSeqID: 5}
	if v := lo.ValidateSeqID(6); v != SeqIDOK {
		t.Errorf("ValidateSeqID(6) = %d, want SeqIDOK", v)
	}
}

func TestLockOwnerValidateSeqID_Replay(t *testing.T) {
	lo := &LockOwner{LastSeqID: 5}
	if v := lo.ValidateSeqID(5); v != SeqIDReplay {
		t.Errorf("ValidateSeqID(5) = %d, want SeqIDReplay", v)
	}
}

func TestLockOwnerValidateSeqID_Bad(t *testing.T) {
	lo := &LockOwner{LastSeqID: 5}
	if v := lo.ValidateSeqID(10); v != SeqIDBad {
		t.Errorf("ValidateSeqID(10) = %d, want SeqIDBad", v)
	}
}

func TestLockOwnerValidateSeqID_WrapAround(t *testing.T) {
	lo := &LockOwner{LastSeqID: 0xFFFFFFFF}
	// Wrap: 0xFFFFFFFF + 1 = 1 (not 0)
	if v := lo.ValidateSeqID(1); v != SeqIDOK {
		t.Errorf("ValidateSeqID(1) after 0xFFFFFFFF = %d, want SeqIDOK", v)
	}
}

// ============================================================================
// makeLockOwnerKey Tests
// ============================================================================

func TestMakeLockOwnerKey(t *testing.T) {
	key1 := makeLockOwnerKey(12345, []byte("owner-a"))
	key2 := makeLockOwnerKey(12345, []byte("owner-a"))
	key3 := makeLockOwnerKey(12345, []byte("owner-b"))
	key4 := makeLockOwnerKey(54321, []byte("owner-a"))

	if key1 != key2 {
		t.Errorf("same inputs should produce same key: %q != %q", key1, key2)
	}
	if key1 == key3 {
		t.Error("different owner data should produce different key")
	}
	if key1 == key4 {
		t.Error("different client ID should produce different key")
	}
}

// ============================================================================
// validateOpenModeForLock Tests
// ============================================================================

func TestValidateOpenModeForLock_WriteLTOnReadOnly(t *testing.T) {
	os := &OpenState{ShareAccess: types.OPEN4_SHARE_ACCESS_READ}
	err := validateOpenModeForLock(os, types.WRITE_LT)
	if err == nil {
		t.Fatal("expected NFS4ERR_OPENMODE for write lock on read-only open")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_OPENMODE {
		t.Errorf("status = %d, want NFS4ERR_OPENMODE (%d)", stateErr.Status, types.NFS4ERR_OPENMODE)
	}
}

func TestValidateOpenModeForLock_ReadLTOnWriteOnly(t *testing.T) {
	os := &OpenState{ShareAccess: types.OPEN4_SHARE_ACCESS_WRITE}
	err := validateOpenModeForLock(os, types.READ_LT)
	if err == nil {
		t.Fatal("expected NFS4ERR_OPENMODE for read lock on write-only open")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_OPENMODE {
		t.Errorf("status = %d, want NFS4ERR_OPENMODE (%d)", stateErr.Status, types.NFS4ERR_OPENMODE)
	}
}

func TestValidateOpenModeForLock_WriteLTOnRW(t *testing.T) {
	os := &OpenState{ShareAccess: types.OPEN4_SHARE_ACCESS_BOTH}
	if err := validateOpenModeForLock(os, types.WRITE_LT); err != nil {
		t.Errorf("unexpected error for write lock on RW open: %v", err)
	}
}

func TestValidateOpenModeForLock_ReadLTOnRW(t *testing.T) {
	os := &OpenState{ShareAccess: types.OPEN4_SHARE_ACCESS_BOTH}
	if err := validateOpenModeForLock(os, types.READ_LT); err != nil {
		t.Errorf("unexpected error for read lock on RW open: %v", err)
	}
}

func TestValidateOpenModeForLock_WritewLTOnReadOnly(t *testing.T) {
	os := &OpenState{ShareAccess: types.OPEN4_SHARE_ACCESS_READ}
	err := validateOpenModeForLock(os, types.WRITEW_LT)
	if err == nil {
		t.Fatal("expected NFS4ERR_OPENMODE for WRITEW_LT on read-only open")
	}
}

func TestValidateOpenModeForLock_ReadwLTOnWriteOnly(t *testing.T) {
	os := &OpenState{ShareAccess: types.OPEN4_SHARE_ACCESS_WRITE}
	err := validateOpenModeForLock(os, types.READW_LT)
	if err == nil {
		t.Fatal("expected NFS4ERR_OPENMODE for READW_LT on write-only open")
	}
}

// ============================================================================
// Helper: Set up a confirmed client + open state for lock tests
// ============================================================================

func setupClientAndOpenState(t *testing.T, sm *StateManager) (clientID uint64, fileHandle []byte, openStateid *types.Stateid4, openSeqid uint32) {
	t.Helper()

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	result, err := sm.SetClientID("lock-test-client", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID failed: %v", err)
	}
	clientID = result.ClientID

	if err := sm.ConfirmClientID(clientID, result.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID failed: %v", err)
	}

	fileHandle = []byte("/export:test-file-001")
	openSeqid = 1

	openResult, err := sm.OpenFile(
		clientID,
		[]byte("open-owner"),
		openSeqid,
		fileHandle,
		types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}

	// Confirm the open
	confirmSeqid := openSeqid + 1
	confirmedStateid, err := sm.ConfirmOpen(&openResult.Stateid, confirmSeqid)
	if err != nil {
		t.Fatalf("ConfirmOpen failed: %v", err)
	}

	return clientID, fileHandle, confirmedStateid, confirmSeqid
}

// ============================================================================
// LockNew Tests
// ============================================================================

func TestLockNew_CreatesLockOwnerAndState(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	lockResult, err := sm.LockNew(
		clientID, []byte("lock-owner-1"), 1,
		openStateid, openSeqid+1,
		fileHandle, types.WRITE_LT, 0, 100, false,
	)
	if err != nil {
		t.Fatalf("LockNew failed: %v", err)
	}
	if lockResult.Denied != nil {
		t.Fatal("expected no conflict, got LOCK4denied")
	}
	if lockResult.Stateid.Seqid == 0 {
		t.Error("expected non-zero stateid seqid")
	}
}

func TestLockNew_ExistingLockOwner(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	// First lock
	_, err := sm.LockNew(
		clientID, []byte("lock-owner-1"), 1,
		openStateid, openSeqid+1,
		fileHandle, types.WRITE_LT, 0, 50, false,
	)
	if err != nil {
		t.Fatalf("first LockNew failed: %v", err)
	}

	// Second lock with same owner on different range
	result, err := sm.LockNew(
		clientID, []byte("lock-owner-1"), 2,
		openStateid, openSeqid+2,
		fileHandle, types.WRITE_LT, 100, 50, false,
	)
	if err != nil {
		t.Fatalf("second LockNew failed: %v", err)
	}
	if result.Denied != nil {
		t.Fatal("expected no conflict for non-overlapping range")
	}
}

func TestLockNew_BadOpenStateid(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)

	clientID, fileHandle, _, _ := setupClientAndOpenState(t, sm)

	// Use a bogus open stateid
	bogusStateid := &types.Stateid4{Seqid: 1}

	_, err := sm.LockNew(
		clientID, []byte("lock-owner-1"), 1,
		bogusStateid, 1,
		fileHandle, types.WRITE_LT, 0, 100, false,
	)
	if err == nil {
		t.Fatal("expected error for bad open stateid")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T: %v", err, err)
	}
	if stateErr.Status != types.NFS4ERR_BAD_STATEID {
		t.Errorf("status = %d, want NFS4ERR_BAD_STATEID (%d)", stateErr.Status, types.NFS4ERR_BAD_STATEID)
	}
}

func TestLockNew_BadOpenSeqid(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	// Use a wrong open seqid (too high)
	_, err := sm.LockNew(
		clientID, []byte("lock-owner-1"), 1,
		openStateid, openSeqid+100,
		fileHandle, types.WRITE_LT, 0, 100, false,
	)
	if err == nil {
		t.Fatal("expected error for bad open seqid")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T: %v", err, err)
	}
	if stateErr.Status != types.NFS4ERR_BAD_SEQID {
		t.Errorf("status = %d, want NFS4ERR_BAD_SEQID (%d)", stateErr.Status, types.NFS4ERR_BAD_SEQID)
	}
}

func TestLockNew_OpenModeViolation(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)

	// Create client with read-only open
	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	result, err := sm.SetClientID("ro-lock-client", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID failed: %v", err)
	}
	if err := sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID failed: %v", err)
	}

	fh := []byte("/export:read-only-file")
	openResult, err := sm.OpenFile(
		result.ClientID,
		[]byte("ro-owner"),
		1,
		fh,
		types.OPEN4_SHARE_ACCESS_READ, // Read-only open
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}

	confirmedStateid, err := sm.ConfirmOpen(&openResult.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen failed: %v", err)
	}

	// Try to get a write lock on a read-only open
	_, lockErr := sm.LockNew(
		result.ClientID, []byte("lock-owner"), 1,
		confirmedStateid, 3,
		fh, types.WRITE_LT, 0, 100, false,
	)
	if lockErr == nil {
		t.Fatal("expected NFS4ERR_OPENMODE for write lock on read-only open")
	}
	stateErr, ok := lockErr.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T: %v", lockErr, lockErr)
	}
	if stateErr.Status != types.NFS4ERR_OPENMODE {
		t.Errorf("status = %d, want NFS4ERR_OPENMODE (%d)", stateErr.Status, types.NFS4ERR_OPENMODE)
	}
}

func TestLockNew_GracePeriod(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90*time.Second, 5*time.Second)
	sm.SetLockManager(lm)

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	// Start grace period
	sm.StartGracePeriod([]uint64{clientID})

	// Non-reclaim should be blocked
	_, err := sm.LockNew(
		clientID, []byte("lock-owner-1"), 1,
		openStateid, openSeqid+1,
		fileHandle, types.WRITE_LT, 0, 100, false,
	)
	if err == nil {
		t.Fatal("expected NFS4ERR_GRACE for non-reclaim lock during grace period")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T: %v", err, err)
	}
	if stateErr.Status != types.NFS4ERR_GRACE {
		t.Errorf("status = %d, want NFS4ERR_GRACE (%d)", stateErr.Status, types.NFS4ERR_GRACE)
	}

	// Reclaim should be allowed
	result, err := sm.LockNew(
		clientID, []byte("lock-owner-1"), 1,
		openStateid, openSeqid+1,
		fileHandle, types.WRITE_LT, 0, 100, true,
	)
	if err != nil {
		t.Fatalf("reclaim LockNew during grace period failed: %v", err)
	}
	if result.Denied != nil {
		t.Fatal("expected no conflict for reclaim lock")
	}

	// Clean up
	sm.Shutdown()
}

// ============================================================================
// LockExisting Tests
// ============================================================================

func TestLockExisting_Success(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	// First lock to get a lock stateid
	lockResult, err := sm.LockNew(
		clientID, []byte("lock-owner-1"), 1,
		openStateid, openSeqid+1,
		fileHandle, types.WRITE_LT, 0, 50, false,
	)
	if err != nil {
		t.Fatalf("LockNew failed: %v", err)
	}

	// Second lock using existing lock stateid
	existResult, err := sm.LockExisting(
		&lockResult.Stateid, 2,
		fileHandle, types.WRITE_LT, 100, 50, false,
	)
	if err != nil {
		t.Fatalf("LockExisting failed: %v", err)
	}
	if existResult.Denied != nil {
		t.Fatal("expected no conflict for non-overlapping range")
	}
	// Stateid seqid should have been incremented
	if existResult.Stateid.Seqid <= lockResult.Stateid.Seqid {
		t.Errorf("stateid seqid not incremented: %d <= %d",
			existResult.Stateid.Seqid, lockResult.Stateid.Seqid)
	}
}

func TestLockExisting_BadStateid(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)

	_, fileHandle, _, _ := setupClientAndOpenState(t, sm)

	// Use a bogus lock stateid with all-zero Other field.
	// The epoch bytes won't match current boot epoch, so it's stale.
	bogusStateid := &types.Stateid4{Seqid: 1}

	_, err := sm.LockExisting(
		bogusStateid, 1,
		fileHandle, types.WRITE_LT, 0, 100, false,
	)
	if err == nil {
		t.Fatal("expected error for bad lock stateid")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T: %v", err, err)
	}
	// Zero Other field has epoch bytes that don't match current boot epoch,
	// so it's classified as STALE_STATEID per RFC 7530 Section 9.1.4.
	if stateErr.Status != types.NFS4ERR_STALE_STATEID && stateErr.Status != types.NFS4ERR_BAD_STATEID {
		t.Errorf("status = %d, want NFS4ERR_STALE_STATEID or NFS4ERR_BAD_STATEID", stateErr.Status)
	}
}

func TestLockExisting_BadSeqid(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	// First lock
	lockResult, err := sm.LockNew(
		clientID, []byte("lock-owner-1"), 1,
		openStateid, openSeqid+1,
		fileHandle, types.WRITE_LT, 0, 50, false,
	)
	if err != nil {
		t.Fatalf("LockNew failed: %v", err)
	}

	// LockExisting with wrong seqid (too high)
	_, err = sm.LockExisting(
		&lockResult.Stateid, 100,
		fileHandle, types.WRITE_LT, 100, 50, false,
	)
	if err == nil {
		t.Fatal("expected error for bad lock seqid")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T: %v", err, err)
	}
	if stateErr.Status != types.NFS4ERR_BAD_SEQID {
		t.Errorf("status = %d, want NFS4ERR_BAD_SEQID (%d)", stateErr.Status, types.NFS4ERR_BAD_SEQID)
	}
}

// ============================================================================
// Conflict Tests
// ============================================================================

func TestLockNew_Conflict(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)

	// Set up two different clients
	verifier1 := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	verifier2 := [8]byte{9, 10, 11, 12, 13, 14, 15, 16}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}
	fh := []byte("/export:conflict-file")

	// Client 1
	res1, err := sm.SetClientID("client-1", verifier1, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID 1 failed: %v", err)
	}
	if err := sm.ConfirmClientID(res1.ClientID, res1.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID 1 failed: %v", err)
	}
	open1, err := sm.OpenFile(res1.ClientID, []byte("owner-1"), 1, fh, types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile 1 failed: %v", err)
	}
	confirmed1, err := sm.ConfirmOpen(&open1.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen 1 failed: %v", err)
	}

	// Client 2
	res2, err := sm.SetClientID("client-2", verifier2, callback, "10.0.0.2:1234")
	if err != nil {
		t.Fatalf("SetClientID 2 failed: %v", err)
	}
	if err := sm.ConfirmClientID(res2.ClientID, res2.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID 2 failed: %v", err)
	}
	open2, err := sm.OpenFile(res2.ClientID, []byte("owner-2"), 1, fh, types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	if err != nil {
		t.Fatalf("OpenFile 2 failed: %v", err)
	}
	confirmed2, err := sm.ConfirmOpen(&open2.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen 2 failed: %v", err)
	}

	// Client 1 acquires exclusive lock on range [0, 100)
	lockResult1, err := sm.LockNew(
		res1.ClientID, []byte("lock-owner-1"), 1,
		confirmed1, 3,
		fh, types.WRITE_LT, 0, 100, false,
	)
	if err != nil {
		t.Fatalf("LockNew client 1 failed: %v", err)
	}
	if lockResult1.Denied != nil {
		t.Fatal("expected no conflict for first lock")
	}

	// Client 2 tries to acquire exclusive lock on overlapping range [50, 150)
	lockResult2, err := sm.LockNew(
		res2.ClientID, []byte("lock-owner-2"), 1,
		confirmed2, 3,
		fh, types.WRITE_LT, 50, 100, false,
	)
	if err != nil {
		t.Fatalf("LockNew client 2 error: %v", err)
	}
	if lockResult2.Denied == nil {
		t.Fatal("expected LOCK4denied for overlapping exclusive locks")
	}
	// Verify denied info
	if lockResult2.Denied.Offset != 0 {
		t.Errorf("denied offset = %d, want 0", lockResult2.Denied.Offset)
	}
	if lockResult2.Denied.Length != 100 {
		t.Errorf("denied length = %d, want 100", lockResult2.Denied.Length)
	}
}

func TestLockNew_SharedNoConflict(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)

	// Two clients with shared locks on same range
	verifier1 := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	verifier2 := [8]byte{9, 10, 11, 12, 13, 14, 15, 16}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}
	fh := []byte("/export:shared-file")

	// Client 1
	res1, _ := sm.SetClientID("client-shared-1", verifier1, callback, "10.0.0.1:1234")
	_ = sm.ConfirmClientID(res1.ClientID, res1.ConfirmVerifier)
	open1, _ := sm.OpenFile(res1.ClientID, []byte("owner-1"), 1, fh, types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	confirmed1, _ := sm.ConfirmOpen(&open1.Stateid, 2)

	// Client 2
	res2, _ := sm.SetClientID("client-shared-2", verifier2, callback, "10.0.0.2:1234")
	_ = sm.ConfirmClientID(res2.ClientID, res2.ConfirmVerifier)
	open2, _ := sm.OpenFile(res2.ClientID, []byte("owner-2"), 1, fh, types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	confirmed2, _ := sm.ConfirmOpen(&open2.Stateid, 2)

	// Client 1 acquires shared lock
	lockResult1, err := sm.LockNew(
		res1.ClientID, []byte("lock-owner-1"), 1,
		confirmed1, 3,
		fh, types.READ_LT, 0, 100, false,
	)
	if err != nil {
		t.Fatalf("LockNew client 1 failed: %v", err)
	}
	if lockResult1.Denied != nil {
		t.Fatal("expected no conflict for first shared lock")
	}

	// Client 2 acquires shared lock on same range -- should succeed
	lockResult2, err := sm.LockNew(
		res2.ClientID, []byte("lock-owner-2"), 1,
		confirmed2, 3,
		fh, types.READ_LT, 0, 100, false,
	)
	if err != nil {
		t.Fatalf("LockNew client 2 failed: %v", err)
	}
	if lockResult2.Denied != nil {
		t.Fatal("expected no conflict for two shared locks on same range")
	}
}

// ============================================================================
// CloseFile with Locks Held Tests (Plan 10-03)
// ============================================================================

func TestCloseFile_LocksHeld(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	// Acquire a lock
	lockResult, err := sm.LockNew(
		clientID, []byte("close-lock-owner"), 1,
		openStateid, openSeqid+1,
		fileHandle, types.WRITE_LT, 0, 100, false,
	)
	if err != nil {
		t.Fatalf("LockNew failed: %v", err)
	}
	if lockResult.Denied != nil {
		t.Fatal("unexpected lock conflict")
	}

	// CLOSE should fail with NFS4ERR_LOCKS_HELD
	_, closeErr := sm.CloseFile(openStateid, openSeqid+2)
	if closeErr == nil {
		t.Fatal("expected NFS4ERR_LOCKS_HELD error from CloseFile")
	}
	stateErr, ok := closeErr.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T: %v", closeErr, closeErr)
	}
	if stateErr.Status != types.NFS4ERR_LOCKS_HELD {
		t.Errorf("status = %d, want NFS4ERR_LOCKS_HELD (%d)", stateErr.Status, types.NFS4ERR_LOCKS_HELD)
	}

	// LOCKU the lock
	_, unlockErr := sm.UnlockFile(&lockResult.Stateid, 2, types.WRITE_LT, 0, 100)
	if unlockErr != nil {
		t.Fatalf("UnlockFile failed: %v", unlockErr)
	}

	// Now CLOSE should succeed (lock state still exists but no active locks)
	// However, LockStates slice still has the LockState entry (it persists after LOCKU).
	// We need to RELEASE_LOCKOWNER first to clean up the LockStates list.
	relErr := sm.ReleaseLockOwner(clientID, []byte("close-lock-owner"))
	if relErr != nil {
		t.Fatalf("ReleaseLockOwner failed: %v", relErr)
	}

	// Now CLOSE should succeed
	closedStateid, closeErr := sm.CloseFile(openStateid, openSeqid+2)
	if closeErr != nil {
		t.Fatalf("CloseFile after unlock+release failed: %v", closeErr)
	}
	if closedStateid.Seqid != 0 {
		t.Errorf("closed stateid seqid = %d, want 0 (zeroed)", closedStateid.Seqid)
	}
}

// ============================================================================
// ReleaseLockOwner Tests (Plan 10-03)
// ============================================================================

func TestReleaseLockOwner_NoLocks(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	// Create lock-owner via LockNew
	lockResult, err := sm.LockNew(
		clientID, []byte("release-owner"), 1,
		openStateid, openSeqid+1,
		fileHandle, types.WRITE_LT, 0, 100, false,
	)
	if err != nil {
		t.Fatalf("LockNew failed: %v", err)
	}

	// LOCKU to remove the lock
	_, unlockErr := sm.UnlockFile(&lockResult.Stateid, 2, types.WRITE_LT, 0, 100)
	if unlockErr != nil {
		t.Fatalf("UnlockFile failed: %v", unlockErr)
	}

	// RELEASE_LOCKOWNER should succeed (no active locks)
	relErr := sm.ReleaseLockOwner(clientID, []byte("release-owner"))
	if relErr != nil {
		t.Fatalf("ReleaseLockOwner failed: %v", relErr)
	}

	// Verify lock stateid is now invalid
	_, valErr := sm.LockExisting(&lockResult.Stateid, 3, fileHandle, types.WRITE_LT, 0, 100, false)
	if valErr == nil {
		t.Fatal("expected error for lock stateid after RELEASE_LOCKOWNER")
	}
}

func TestReleaseLockOwner_WithLocks(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	// Create lock-owner and hold a lock
	_, err := sm.LockNew(
		clientID, []byte("held-lock-owner"), 1,
		openStateid, openSeqid+1,
		fileHandle, types.WRITE_LT, 0, 100, false,
	)
	if err != nil {
		t.Fatalf("LockNew failed: %v", err)
	}

	// RELEASE_LOCKOWNER should fail with NFS4ERR_LOCKS_HELD
	relErr := sm.ReleaseLockOwner(clientID, []byte("held-lock-owner"))
	if relErr == nil {
		t.Fatal("expected NFS4ERR_LOCKS_HELD from ReleaseLockOwner")
	}
	stateErr, ok := relErr.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T: %v", relErr, relErr)
	}
	if stateErr.Status != types.NFS4ERR_LOCKS_HELD {
		t.Errorf("status = %d, want NFS4ERR_LOCKS_HELD (%d)", stateErr.Status, types.NFS4ERR_LOCKS_HELD)
	}
}

func TestReleaseLockOwner_Unknown(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Release unknown lock-owner should return nil (NFS4_OK)
	err := sm.ReleaseLockOwner(99999, []byte("nonexistent-owner"))
	if err != nil {
		t.Errorf("ReleaseLockOwner for unknown owner returned error: %v", err)
	}
}

// ============================================================================
// Lease Expiry Lock Cleanup Tests (Plan 10-03)
// ============================================================================

func TestLeaseExpiry_CleansLockState(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(50 * time.Millisecond) // short lease for fast expiry
	sm.SetLockManager(lm)

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	// Acquire a lock
	lockResult, err := sm.LockNew(
		clientID, []byte("expiry-lock-owner"), 1,
		openStateid, openSeqid+1,
		fileHandle, types.WRITE_LT, 0, 100, false,
	)
	if err != nil {
		t.Fatalf("LockNew failed: %v", err)
	}
	if lockResult.Denied != nil {
		t.Fatal("unexpected lock conflict")
	}

	// Verify the lock exists in the lock manager
	locks := lm.ListEnhancedLocks(string(fileHandle))
	if len(locks) == 0 {
		t.Fatal("expected lock to exist in lock manager before expiry")
	}

	// Wait for lease to expire
	time.Sleep(150 * time.Millisecond)

	// Verify lock stateid is now invalid (cleaned up by onLeaseExpired)
	_, valErr := sm.LockExisting(&lockResult.Stateid, 2, fileHandle, types.WRITE_LT, 0, 100, false)
	if valErr == nil {
		t.Fatal("expected error for lock stateid after lease expiry")
	}

	// Verify locks removed from lock manager
	locks = lm.ListEnhancedLocks(string(fileHandle))
	if len(locks) != 0 {
		t.Errorf("expected 0 locks in lock manager after lease expiry, got %d", len(locks))
	}

	// Verify open state is also removed
	_, openValErr := sm.ValidateStateid(openStateid, fileHandle)
	if openValErr == nil {
		t.Fatal("expected error for open stateid after lease expiry")
	}
}

func TestLeaseExpiry_CleansLockManager(t *testing.T) {
	lm := lock.NewManager()
	sm := NewStateManager(50 * time.Millisecond)
	sm.SetLockManager(lm)

	// Client 1: short lease, will expire
	clientID1, fileHandle, openStateid1, openSeqid1 := setupClientAndOpenState(t, sm)

	// Acquire a lock with client 1
	_, err := sm.LockNew(
		clientID1, []byte("client1-lock-owner"), 1,
		openStateid1, openSeqid1+1,
		fileHandle, types.WRITE_LT, 0, 100, false,
	)
	if err != nil {
		t.Fatalf("LockNew failed: %v", err)
	}

	// Wait for client 1's lease to expire
	time.Sleep(150 * time.Millisecond)

	// Set up client 2 (fresh StateManager or new client to avoid test interference)
	// Since the lease expired, we create a new client
	verifier2 := [8]byte{20, 21, 22, 23, 24, 25, 26, 27}
	callback2 := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.2.8.1"}

	res2, err := sm.SetClientID("lock-test-client-2", verifier2, callback2, "10.0.0.2:1234")
	if err != nil {
		t.Fatalf("SetClientID for client 2 failed: %v", err)
	}
	if err := sm.ConfirmClientID(res2.ClientID, res2.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID for client 2 failed: %v", err)
	}

	openResult2, err := sm.OpenFile(
		res2.ClientID, []byte("open-owner-2"), 1,
		fileHandle, types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL,
	)
	if err != nil {
		t.Fatalf("OpenFile for client 2 failed: %v", err)
	}
	confirmed2, err := sm.ConfirmOpen(&openResult2.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen for client 2 failed: %v", err)
	}

	// Client 2 should be able to acquire a lock on the same range
	// (previously held by expired client 1)
	lockResult2, err := sm.LockNew(
		res2.ClientID, []byte("client2-lock-owner"), 1,
		confirmed2, 3,
		fileHandle, types.WRITE_LT, 0, 100, false,
	)
	if err != nil {
		t.Fatalf("LockNew for client 2 failed: %v", err)
	}
	if lockResult2.Denied != nil {
		t.Fatal("expected no conflict: client 1's lock should have been cleaned up on lease expiry")
	}

	sm.Shutdown()
}

func TestLockNew_BlockingType(t *testing.T) {
	// READW_LT and WRITEW_LT should return NFS4ERR_DENIED (not block)
	lm := lock.NewManager()
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)

	// Set up two clients
	verifier1 := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	verifier2 := [8]byte{9, 10, 11, 12, 13, 14, 15, 16}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}
	fh := []byte("/export:blocking-file")

	res1, _ := sm.SetClientID("client-block-1", verifier1, callback, "10.0.0.1:1234")
	_ = sm.ConfirmClientID(res1.ClientID, res1.ConfirmVerifier)
	open1, _ := sm.OpenFile(res1.ClientID, []byte("owner-1"), 1, fh, types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	confirmed1, _ := sm.ConfirmOpen(&open1.Stateid, 2)

	res2, _ := sm.SetClientID("client-block-2", verifier2, callback, "10.0.0.2:1234")
	_ = sm.ConfirmClientID(res2.ClientID, res2.ConfirmVerifier)
	open2, _ := sm.OpenFile(res2.ClientID, []byte("owner-2"), 1, fh, types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
	confirmed2, _ := sm.ConfirmOpen(&open2.Stateid, 2)

	// Client 1 acquires exclusive lock
	_, err := sm.LockNew(
		res1.ClientID, []byte("lock-owner-1"), 1,
		confirmed1, 3,
		fh, types.WRITE_LT, 0, 100, false,
	)
	if err != nil {
		t.Fatalf("LockNew failed: %v", err)
	}

	// Client 2 tries blocking write lock (WRITEW_LT) -- should get DENIED, not block
	lockResult, err := sm.LockNew(
		res2.ClientID, []byte("lock-owner-2"), 1,
		confirmed2, 3,
		fh, types.WRITEW_LT, 0, 100, false,
	)
	if err != nil {
		t.Fatalf("LockNew with WRITEW_LT failed: %v", err)
	}
	if lockResult.Denied == nil {
		t.Fatal("expected LOCK4denied for WRITEW_LT on conflicting range (should not block)")
	}

	// Also test READW_LT with a new lock-owner (seqids not advanced from denied)
	lockResult2, err := sm.LockNew(
		res2.ClientID, []byte("lock-owner-2b"), 1,
		confirmed2, 3,
		fh, types.READW_LT, 0, 100, false,
	)
	if err != nil {
		t.Fatalf("LockNew with READW_LT failed: %v", err)
	}
	if lockResult2.Denied == nil {
		t.Fatal("expected LOCK4denied for READW_LT on conflicting exclusive range")
	}
}
