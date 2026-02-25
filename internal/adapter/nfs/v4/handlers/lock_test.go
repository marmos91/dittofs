package handlers

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/xdr"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// LOCK Handler Tests
// ============================================================================

func TestHandleLock_NoCurrentFH(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  nil,
	}

	// Encode minimal LOCK args (won't be read since FH check happens first)
	var args bytes.Buffer
	_ = xdr.WriteUint32(&args, types.WRITE_LT) // locktype
	_ = xdr.WriteUint32(&args, 0)              // reclaim
	_ = xdr.WriteUint64(&args, 0)              // offset
	_ = xdr.WriteUint64(&args, 100)            // length
	_ = xdr.WriteUint32(&args, 1)              // new_lock_owner = true

	result := h.handleLock(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("LOCK without FH status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
	if result.OpCode != types.OP_LOCK {
		t.Errorf("LOCK opCode = %d, want OP_LOCK (%d)",
			result.OpCode, types.OP_LOCK)
	}
}

func TestHandleLock_PseudoFS(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  pfs.GetRootHandle(),
	}

	// Encode minimal LOCK args
	var args bytes.Buffer
	_ = xdr.WriteUint32(&args, types.WRITE_LT)
	_ = xdr.WriteUint32(&args, 0)
	_ = xdr.WriteUint64(&args, 0)
	_ = xdr.WriteUint64(&args, 100)
	_ = xdr.WriteUint32(&args, 1)

	result := h.handleLock(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4ERR_INVAL {
		t.Errorf("LOCK on pseudo-fs status = %d, want NFS4ERR_INVAL (%d)",
			result.Status, types.NFS4ERR_INVAL)
	}
}

func TestHandleLock_BadXDR(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  []byte("/export:test-file"),
	}

	// Truncated input -- just 2 bytes
	result := h.handleLock(ctx, bytes.NewReader([]byte{0x00, 0x01}))

	if result.Status != types.NFS4ERR_BADXDR {
		t.Errorf("LOCK with truncated input status = %d, want NFS4ERR_BADXDR (%d)",
			result.Status, types.NFS4ERR_BADXDR)
	}
}

// setupHandlerLockClient sets up a confirmed client and open state for handler-level tests.
func setupHandlerLockClient(t *testing.T, h *Handler) (clientID uint64, openStateid *types.Stateid4, openSeqid uint32) {
	t.Helper()

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
	}

	// SETCLIENTID
	var scidArgs bytes.Buffer
	scidArgs.Write(make([]byte, 8))                          // client verifier
	_ = xdr.WriteXDRString(&scidArgs, "lock-handler-client") // client id string
	_ = xdr.WriteUint32(&scidArgs, 0x40000000)               // callback program
	_ = xdr.WriteXDRString(&scidArgs, "tcp")                 // callback netid
	_ = xdr.WriteXDRString(&scidArgs, "127.0.0.1.8.1")       // callback addr
	_ = xdr.WriteUint32(&scidArgs, 1)                        // callback_ident

	scidResult := h.handleSetClientID(ctx, bytes.NewReader(scidArgs.Bytes()))
	if scidResult.Status != types.NFS4_OK {
		t.Fatalf("SETCLIENTID failed: %d", scidResult.Status)
	}

	scidReader := bytes.NewReader(scidResult.Data)
	_, _ = xdr.DecodeUint32(scidReader) // status
	clientID, _ = xdr.DecodeUint64(scidReader)
	var confirmVerifier [8]byte
	_, _ = scidReader.Read(confirmVerifier[:])

	// SETCLIENTID_CONFIRM
	var confirmArgs bytes.Buffer
	_ = xdr.WriteUint64(&confirmArgs, clientID)
	confirmArgs.Write(confirmVerifier[:])

	confirmResult := h.handleSetClientIDConfirm(ctx, bytes.NewReader(confirmArgs.Bytes()))
	if confirmResult.Status != types.NFS4_OK {
		t.Fatalf("SETCLIENTID_CONFIRM failed: %d", confirmResult.Status)
	}

	// OPEN (create state)
	fileHandle := []byte("/export:lock-test-file")
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	openSeqid = 1
	openResult, err := h.StateManager.OpenFile(
		clientID,
		[]byte("lock-test-owner"),
		openSeqid,
		fileHandle,
		types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}

	// OPEN_CONFIRM
	confirmOpenSeqid := openSeqid + 1
	confirmedStateid, err := h.StateManager.ConfirmOpen(&openResult.Stateid, confirmOpenSeqid)
	if err != nil {
		t.Fatalf("ConfirmOpen failed: %v", err)
	}

	return clientID, confirmedStateid, confirmOpenSeqid
}

func TestHandleLock_NewLockOwner_Success(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})

	lm := lock.NewManager()
	sm := state.NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	h := NewHandler(nil, pfs, sm)

	clientID, openStateid, openSeqid := setupHandlerLockClient(t, h)

	fileHandle := []byte("/export:lock-test-file")
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(fileHandle)),
	}
	copy(ctx.CurrentFH, fileHandle)

	// Encode LOCK4args with new_lock_owner=true
	var args bytes.Buffer
	_ = xdr.WriteUint32(&args, types.WRITE_LT) // locktype
	_ = xdr.WriteUint32(&args, 0)              // reclaim = false
	_ = xdr.WriteUint64(&args, 0)              // offset
	_ = xdr.WriteUint64(&args, 100)            // length
	_ = xdr.WriteUint32(&args, 1)              // new_lock_owner = true
	// open_to_lock_owner4:
	_ = xdr.WriteUint32(&args, openSeqid+1)                // open_seqid
	types.EncodeStateid4(&args, openStateid)               // open_stateid
	_ = xdr.WriteUint32(&args, 1)                          // lock_seqid
	_ = xdr.WriteUint64(&args, clientID)                   // lock_owner clientid
	_ = xdr.WriteXDROpaque(&args, []byte("my-lock-owner")) // lock_owner data

	result := h.handleLock(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4_OK {
		t.Fatalf("LOCK status = %d, want NFS4_OK (%d)", result.Status, types.NFS4_OK)
	}
	if result.OpCode != types.OP_LOCK {
		t.Errorf("LOCK opCode = %d, want OP_LOCK (%d)", result.OpCode, types.OP_LOCK)
	}

	// Parse response: status + stateid4
	reader := bytes.NewReader(result.Data)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("encoded status = %d, want NFS4_OK", status)
	}

	lockStateid, err := types.DecodeStateid4(reader)
	if err != nil {
		t.Fatalf("failed to decode lock stateid: %v", err)
	}
	if lockStateid.Seqid == 0 {
		t.Error("lock stateid seqid should be non-zero")
	}
}

func TestHandleLock_ExistingLockOwner_Success(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})

	lm := lock.NewManager()
	sm := state.NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	h := NewHandler(nil, pfs, sm)

	clientID, openStateid, openSeqid := setupHandlerLockClient(t, h)

	fileHandle := []byte("/export:lock-test-file")
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(fileHandle)),
	}
	copy(ctx.CurrentFH, fileHandle)

	// First: create lock with new_lock_owner=true
	var args1 bytes.Buffer
	_ = xdr.WriteUint32(&args1, types.WRITE_LT)
	_ = xdr.WriteUint32(&args1, 0) // reclaim
	_ = xdr.WriteUint64(&args1, 0)
	_ = xdr.WriteUint64(&args1, 50)
	_ = xdr.WriteUint32(&args1, 1) // new_lock_owner = true
	_ = xdr.WriteUint32(&args1, openSeqid+1)
	types.EncodeStateid4(&args1, openStateid)
	_ = xdr.WriteUint32(&args1, 1) // lock_seqid
	_ = xdr.WriteUint64(&args1, clientID)
	_ = xdr.WriteXDROpaque(&args1, []byte("existing-test-owner"))

	result1 := h.handleLock(ctx, bytes.NewReader(args1.Bytes()))
	if result1.Status != types.NFS4_OK {
		t.Fatalf("first LOCK status = %d, want NFS4_OK", result1.Status)
	}

	// Parse lock stateid from first response
	reader1 := bytes.NewReader(result1.Data)
	_, _ = xdr.DecodeUint32(reader1) // status
	lockStateid, err := types.DecodeStateid4(reader1)
	if err != nil {
		t.Fatalf("failed to decode lock stateid: %v", err)
	}

	// Second: extend lock with new_lock_owner=false (existing)
	var args2 bytes.Buffer
	_ = xdr.WriteUint32(&args2, types.WRITE_LT)
	_ = xdr.WriteUint32(&args2, 0) // reclaim
	_ = xdr.WriteUint64(&args2, 100)
	_ = xdr.WriteUint64(&args2, 50)
	_ = xdr.WriteUint32(&args2, 0) // new_lock_owner = false
	// exist_lock_owner4:
	types.EncodeStateid4(&args2, lockStateid) // lock_stateid
	_ = xdr.WriteUint32(&args2, 2)            // lock_seqid

	result2 := h.handleLock(ctx, bytes.NewReader(args2.Bytes()))
	if result2.Status != types.NFS4_OK {
		t.Fatalf("second LOCK (existing) status = %d, want NFS4_OK (%d)", result2.Status, types.NFS4_OK)
	}

	// Parse second lock stateid
	reader2 := bytes.NewReader(result2.Data)
	_, _ = xdr.DecodeUint32(reader2) // status
	lockStateid2, err := types.DecodeStateid4(reader2)
	if err != nil {
		t.Fatalf("failed to decode second lock stateid: %v", err)
	}

	// Seqid should have been incremented
	if lockStateid2.Seqid <= lockStateid.Seqid {
		t.Errorf("lock stateid seqid not incremented: %d <= %d",
			lockStateid2.Seqid, lockStateid.Seqid)
	}
}

func TestHandleLock_Dispatched(t *testing.T) {
	// Verify OP_LOCK is registered in the dispatch table
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	if _, exists := h.opDispatchTable[types.OP_LOCK]; !exists {
		t.Fatal("OP_LOCK not registered in dispatch table")
	}
}

// ============================================================================
// Helper: acquireLockForTest uses LOCK to create a lock-owner + acquire a lock,
// returns the lock stateid and lock-owner seqid for further use.
// ============================================================================

func acquireLockForTest(t *testing.T, h *Handler, clientID uint64, openStateid *types.Stateid4, openSeqid uint32, lockOwnerData []byte, lockType uint32, offset, length uint64) (*types.Stateid4, uint32) {
	t.Helper()

	fileHandle := []byte("/export:lock-test-file")
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(fileHandle)),
	}
	copy(ctx.CurrentFH, fileHandle)

	var args bytes.Buffer
	_ = xdr.WriteUint32(&args, lockType)
	_ = xdr.WriteUint32(&args, 0) // reclaim = false
	_ = xdr.WriteUint64(&args, offset)
	_ = xdr.WriteUint64(&args, length)
	_ = xdr.WriteUint32(&args, 1) // new_lock_owner = true
	// open_to_lock_owner4:
	_ = xdr.WriteUint32(&args, openSeqid+1)
	types.EncodeStateid4(&args, openStateid)
	_ = xdr.WriteUint32(&args, 1) // lock_seqid
	_ = xdr.WriteUint64(&args, clientID)
	_ = xdr.WriteXDROpaque(&args, lockOwnerData)

	result := h.handleLock(ctx, bytes.NewReader(args.Bytes()))
	if result.Status != types.NFS4_OK {
		t.Fatalf("LOCK failed: status=%d", result.Status)
	}

	reader := bytes.NewReader(result.Data)
	_, _ = xdr.DecodeUint32(reader) // status
	lockStateid, err := types.DecodeStateid4(reader)
	if err != nil {
		t.Fatalf("failed to decode lock stateid: %v", err)
	}

	// lock_seqid was 1, so next seqid for lock-owner is 2
	return lockStateid, 2
}

// ============================================================================
// LOCKT Handler Tests
// ============================================================================

func TestHandleLockT_NoConflict(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})

	lm := lock.NewManager()
	sm := state.NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	h := NewHandler(nil, pfs, sm)

	clientID, _, _ := setupHandlerLockClient(t, h)

	fileHandle := []byte("/export:lock-test-file")
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(fileHandle)),
	}
	copy(ctx.CurrentFH, fileHandle)

	// LOCKT on unlocked file: should return NFS4_OK
	var args bytes.Buffer
	_ = xdr.WriteUint32(&args, types.WRITE_LT) // locktype
	_ = xdr.WriteUint64(&args, 0)              // offset
	_ = xdr.WriteUint64(&args, 100)            // length
	_ = xdr.WriteUint64(&args, clientID)       // lock_owner4.clientid
	_ = xdr.WriteXDROpaque(&args, []byte("lockt-test-owner"))

	result := h.handleLockT(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4_OK {
		t.Errorf("LOCKT on unlocked file status = %d, want NFS4_OK (%d)",
			result.Status, types.NFS4_OK)
	}
	if result.OpCode != types.OP_LOCKT {
		t.Errorf("LOCKT opCode = %d, want OP_LOCKT (%d)",
			result.OpCode, types.OP_LOCKT)
	}
}

func TestHandleLockT_Conflict(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})

	lm := lock.NewManager()
	sm := state.NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	h := NewHandler(nil, pfs, sm)

	clientID, openStateid, openSeqid := setupHandlerLockClient(t, h)

	// First: acquire an exclusive lock via LOCK
	acquireLockForTest(t, h, clientID, openStateid, openSeqid, []byte("holder-owner"), types.WRITE_LT, 0, 100)

	fileHandle := []byte("/export:lock-test-file")
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(fileHandle)),
	}
	copy(ctx.CurrentFH, fileHandle)

	// LOCKT from a DIFFERENT owner: should return NFS4ERR_DENIED
	var args bytes.Buffer
	_ = xdr.WriteUint32(&args, types.WRITE_LT)
	_ = xdr.WriteUint64(&args, 0)
	_ = xdr.WriteUint64(&args, 100)
	_ = xdr.WriteUint64(&args, clientID)
	_ = xdr.WriteXDROpaque(&args, []byte("different-owner")) // different owner

	result := h.handleLockT(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4ERR_DENIED {
		t.Fatalf("LOCKT with conflict status = %d, want NFS4ERR_DENIED (%d)",
			result.Status, types.NFS4ERR_DENIED)
	}

	// Decode LOCK4denied from response
	reader := bytes.NewReader(result.Data)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4ERR_DENIED {
		t.Errorf("encoded status = %d, want NFS4ERR_DENIED", status)
	}

	// LOCK4denied fields: offset (uint64), length (uint64), locktype (uint32), lock_owner4
	deniedOffset, _ := xdr.DecodeUint64(reader)
	deniedLength, _ := xdr.DecodeUint64(reader)
	deniedLockType, _ := xdr.DecodeUint32(reader)

	if deniedOffset != 0 {
		t.Errorf("denied offset = %d, want 0", deniedOffset)
	}
	if deniedLength != 100 {
		t.Errorf("denied length = %d, want 100", deniedLength)
	}
	if deniedLockType != types.WRITE_LT {
		t.Errorf("denied locktype = %d, want WRITE_LT (%d)", deniedLockType, types.WRITE_LT)
	}
}

func TestHandleLockT_SharedNoConflict(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})

	lm := lock.NewManager()
	sm := state.NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	h := NewHandler(nil, pfs, sm)

	clientID, openStateid, openSeqid := setupHandlerLockClient(t, h)

	// First: acquire a shared (read) lock
	acquireLockForTest(t, h, clientID, openStateid, openSeqid, []byte("reader-owner"), types.READ_LT, 0, 100)

	fileHandle := []byte("/export:lock-test-file")
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(fileHandle)),
	}
	copy(ctx.CurrentFH, fileHandle)

	// LOCKT for another shared lock from different owner: should NOT conflict
	var args bytes.Buffer
	_ = xdr.WriteUint32(&args, types.READ_LT)
	_ = xdr.WriteUint64(&args, 0)
	_ = xdr.WriteUint64(&args, 100)
	_ = xdr.WriteUint64(&args, clientID)
	_ = xdr.WriteXDROpaque(&args, []byte("another-reader")) // different owner

	result := h.handleLockT(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4_OK {
		t.Errorf("LOCKT shared vs shared status = %d, want NFS4_OK (%d)",
			result.Status, types.NFS4_OK)
	}
}

func TestHandleLockT_NoCurrentFH(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  nil,
	}

	var args bytes.Buffer
	_ = xdr.WriteUint32(&args, types.WRITE_LT)
	_ = xdr.WriteUint64(&args, 0)
	_ = xdr.WriteUint64(&args, 100)
	_ = xdr.WriteUint64(&args, 12345)
	_ = xdr.WriteXDROpaque(&args, []byte("owner"))

	result := h.handleLockT(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("LOCKT without FH status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

func TestHandleLockT_PseudoFS(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  pfs.GetRootHandle(),
	}

	var args bytes.Buffer
	_ = xdr.WriteUint32(&args, types.WRITE_LT)
	_ = xdr.WriteUint64(&args, 0)
	_ = xdr.WriteUint64(&args, 100)
	_ = xdr.WriteUint64(&args, 12345)
	_ = xdr.WriteXDROpaque(&args, []byte("owner"))

	result := h.handleLockT(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4ERR_INVAL {
		t.Errorf("LOCKT on pseudo-fs status = %d, want NFS4ERR_INVAL (%d)",
			result.Status, types.NFS4ERR_INVAL)
	}
}

func TestHandleLockT_Dispatched(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	if _, exists := h.opDispatchTable[types.OP_LOCKT]; !exists {
		t.Fatal("OP_LOCKT not registered in dispatch table")
	}
}

// ============================================================================
// LOCKU Handler Tests
// ============================================================================

func TestHandleLockU_Success(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})

	lm := lock.NewManager()
	sm := state.NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	h := NewHandler(nil, pfs, sm)

	clientID, openStateid, openSeqid := setupHandlerLockClient(t, h)

	// Acquire a lock
	lockStateid, lockSeqid := acquireLockForTest(t, h, clientID, openStateid, openSeqid, []byte("unlock-owner"), types.WRITE_LT, 0, 100)

	fileHandle := []byte("/export:lock-test-file")
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(fileHandle)),
	}
	copy(ctx.CurrentFH, fileHandle)

	// LOCKU to release the lock
	var args bytes.Buffer
	_ = xdr.WriteUint32(&args, types.WRITE_LT) // locktype
	_ = xdr.WriteUint32(&args, lockSeqid)      // seqid
	types.EncodeStateid4(&args, lockStateid)   // lock_stateid
	_ = xdr.WriteUint64(&args, 0)              // offset
	_ = xdr.WriteUint64(&args, 100)            // length

	result := h.handleLockU(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4_OK {
		t.Fatalf("LOCKU status = %d, want NFS4_OK (%d)",
			result.Status, types.NFS4_OK)
	}
	if result.OpCode != types.OP_LOCKU {
		t.Errorf("LOCKU opCode = %d, want OP_LOCKU (%d)",
			result.OpCode, types.OP_LOCKU)
	}

	// Parse response: status + stateid4
	reader := bytes.NewReader(result.Data)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("encoded status = %d, want NFS4_OK", status)
	}

	updatedStateid, err := types.DecodeStateid4(reader)
	if err != nil {
		t.Fatalf("failed to decode updated stateid: %v", err)
	}

	// Seqid should have been incremented
	if updatedStateid.Seqid <= lockStateid.Seqid {
		t.Errorf("lock stateid seqid not incremented: %d <= %d",
			updatedStateid.Seqid, lockStateid.Seqid)
	}
}

func TestHandleLockU_BadStateid(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})

	lm := lock.NewManager()
	sm := state.NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	h := NewHandler(nil, pfs, sm)

	fileHandle := []byte("/export:lock-test-file")
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(fileHandle)),
	}
	copy(ctx.CurrentFH, fileHandle)

	// LOCKU with a fabricated stateid that doesn't exist
	fakeStateid := &types.Stateid4{
		Seqid: 1,
	}
	// Set the epoch bytes to match current boot epoch so we get BAD_STATEID not STALE
	fakeStateid.Other[0] = state.StateTypeLock
	fakeStateid.Other[1] = byte(sm.BootEpoch() >> 16)
	fakeStateid.Other[2] = byte(sm.BootEpoch() >> 8)
	fakeStateid.Other[3] = byte(sm.BootEpoch())
	// Bytes 4-11: random non-matching sequence
	fakeStateid.Other[4] = 0xFF
	fakeStateid.Other[5] = 0xFF

	var args bytes.Buffer
	_ = xdr.WriteUint32(&args, types.WRITE_LT) // locktype
	_ = xdr.WriteUint32(&args, 1)              // seqid
	types.EncodeStateid4(&args, fakeStateid)   // lock_stateid
	_ = xdr.WriteUint64(&args, 0)              // offset
	_ = xdr.WriteUint64(&args, 100)            // length

	result := h.handleLockU(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4ERR_BAD_STATEID {
		t.Errorf("LOCKU with unknown stateid status = %d, want NFS4ERR_BAD_STATEID (%d)",
			result.Status, types.NFS4ERR_BAD_STATEID)
	}
}

func TestHandleLockU_BadSeqid(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})

	lm := lock.NewManager()
	sm := state.NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	h := NewHandler(nil, pfs, sm)

	clientID, openStateid, openSeqid := setupHandlerLockClient(t, h)

	// Acquire a lock
	lockStateid, _ := acquireLockForTest(t, h, clientID, openStateid, openSeqid, []byte("seqid-test-owner"), types.WRITE_LT, 0, 100)

	fileHandle := []byte("/export:lock-test-file")
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(fileHandle)),
	}
	copy(ctx.CurrentFH, fileHandle)

	// LOCKU with wrong seqid (999 is way off)
	var args bytes.Buffer
	_ = xdr.WriteUint32(&args, types.WRITE_LT)
	_ = xdr.WriteUint32(&args, 999) // wrong seqid
	types.EncodeStateid4(&args, lockStateid)
	_ = xdr.WriteUint64(&args, 0)
	_ = xdr.WriteUint64(&args, 100)

	result := h.handleLockU(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4ERR_BAD_SEQID {
		t.Errorf("LOCKU with bad seqid status = %d, want NFS4ERR_BAD_SEQID (%d)",
			result.Status, types.NFS4ERR_BAD_SEQID)
	}
}

func TestHandleLockU_PartialUnlock(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})

	lm := lock.NewManager()
	sm := state.NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	h := NewHandler(nil, pfs, sm)

	clientID, openStateid, openSeqid := setupHandlerLockClient(t, h)

	// Acquire a lock on full range [0, 100)
	lockStateid, lockSeqid := acquireLockForTest(t, h, clientID, openStateid, openSeqid, []byte("partial-owner"), types.WRITE_LT, 0, 100)

	fileHandle := []byte("/export:lock-test-file")
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(fileHandle)),
	}
	copy(ctx.CurrentFH, fileHandle)

	// LOCKU: unlock first half [0, 50)
	var unlockArgs bytes.Buffer
	_ = xdr.WriteUint32(&unlockArgs, types.WRITE_LT)
	_ = xdr.WriteUint32(&unlockArgs, lockSeqid) // seqid
	types.EncodeStateid4(&unlockArgs, lockStateid)
	_ = xdr.WriteUint64(&unlockArgs, 0)  // offset
	_ = xdr.WriteUint64(&unlockArgs, 50) // length

	unlockResult := h.handleLockU(ctx, bytes.NewReader(unlockArgs.Bytes()))
	if unlockResult.Status != types.NFS4_OK {
		t.Fatalf("partial LOCKU status = %d, want NFS4_OK", unlockResult.Status)
	}

	// Now LOCKT from a different owner on the freed first half [0, 50): should succeed
	var testArgs1 bytes.Buffer
	_ = xdr.WriteUint32(&testArgs1, types.WRITE_LT)
	_ = xdr.WriteUint64(&testArgs1, 0)  // offset
	_ = xdr.WriteUint64(&testArgs1, 50) // length
	_ = xdr.WriteUint64(&testArgs1, clientID)
	_ = xdr.WriteXDROpaque(&testArgs1, []byte("tester-owner"))

	testResult1 := h.handleLockT(ctx, bytes.NewReader(testArgs1.Bytes()))
	if testResult1.Status != types.NFS4_OK {
		t.Errorf("LOCKT on freed range [0,50) status = %d, want NFS4_OK (%d)",
			testResult1.Status, types.NFS4_OK)
	}

	// LOCKT from a different owner on the still-locked second half [50, 100): should DENY
	var testArgs2 bytes.Buffer
	_ = xdr.WriteUint32(&testArgs2, types.WRITE_LT)
	_ = xdr.WriteUint64(&testArgs2, 50) // offset
	_ = xdr.WriteUint64(&testArgs2, 50) // length
	_ = xdr.WriteUint64(&testArgs2, clientID)
	_ = xdr.WriteXDROpaque(&testArgs2, []byte("tester-owner"))

	testResult2 := h.handleLockT(ctx, bytes.NewReader(testArgs2.Bytes()))
	if testResult2.Status != types.NFS4ERR_DENIED {
		t.Errorf("LOCKT on still-locked range [50,100) status = %d, want NFS4ERR_DENIED (%d)",
			testResult2.Status, types.NFS4ERR_DENIED)
	}
}

func TestHandleLockU_NoCurrentFH(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  nil,
	}

	var args bytes.Buffer
	_ = xdr.WriteUint32(&args, types.WRITE_LT)
	_ = xdr.WriteUint32(&args, 1) // seqid
	// Fake stateid
	var fakeStateid types.Stateid4
	fakeStateid.Seqid = 1
	types.EncodeStateid4(&args, &fakeStateid)
	_ = xdr.WriteUint64(&args, 0)
	_ = xdr.WriteUint64(&args, 100)

	result := h.handleLockU(ctx, bytes.NewReader(args.Bytes()))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("LOCKU without FH status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

func TestHandleLockU_Dispatched(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	if _, exists := h.opDispatchTable[types.OP_LOCKU]; !exists {
		t.Fatal("OP_LOCKU not registered in dispatch table")
	}
}

// ============================================================================
// RELEASE_LOCKOWNER Handler Tests (Plan 10-03)
// ============================================================================

func TestHandleReleaseLockOwner_NoLocks(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})

	lm := lock.NewManager()
	sm := state.NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	h := NewHandler(nil, pfs, sm)

	clientID, openStateid, openSeqid := setupHandlerLockClient(t, h)

	// Acquire a lock, then release it
	lockStateid, lockSeqid := acquireLockForTest(t, h, clientID, openStateid, openSeqid,
		[]byte("release-handler-owner"), types.WRITE_LT, 0, 100)

	fileHandle := []byte("/export:lock-test-file")
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(fileHandle)),
	}
	copy(ctx.CurrentFH, fileHandle)

	// LOCKU to release the actual lock
	var unlockArgs bytes.Buffer
	_ = xdr.WriteUint32(&unlockArgs, types.WRITE_LT)
	_ = xdr.WriteUint32(&unlockArgs, lockSeqid)
	types.EncodeStateid4(&unlockArgs, lockStateid)
	_ = xdr.WriteUint64(&unlockArgs, 0)
	_ = xdr.WriteUint64(&unlockArgs, 100)

	unlockResult := h.handleLockU(ctx, bytes.NewReader(unlockArgs.Bytes()))
	if unlockResult.Status != types.NFS4_OK {
		t.Fatalf("LOCKU failed: status=%d", unlockResult.Status)
	}

	// Now RELEASE_LOCKOWNER should succeed
	var relArgs bytes.Buffer
	_ = xdr.WriteUint64(&relArgs, clientID)
	_ = xdr.WriteXDROpaque(&relArgs, []byte("release-handler-owner"))

	relResult := h.handleReleaseLockOwner(ctx, bytes.NewReader(relArgs.Bytes()))

	if relResult.Status != types.NFS4_OK {
		t.Errorf("RELEASE_LOCKOWNER status = %d, want NFS4_OK (%d)",
			relResult.Status, types.NFS4_OK)
	}
	if relResult.OpCode != types.OP_RELEASE_LOCKOWNER {
		t.Errorf("RELEASE_LOCKOWNER opCode = %d, want OP_RELEASE_LOCKOWNER (%d)",
			relResult.OpCode, types.OP_RELEASE_LOCKOWNER)
	}
}

func TestHandleReleaseLockOwner_LocksHeld(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})

	lm := lock.NewManager()
	sm := state.NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	h := NewHandler(nil, pfs, sm)

	clientID, openStateid, openSeqid := setupHandlerLockClient(t, h)

	// Acquire a lock but do NOT release it
	acquireLockForTest(t, h, clientID, openStateid, openSeqid,
		[]byte("held-handler-owner"), types.WRITE_LT, 0, 100)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
	}

	// RELEASE_LOCKOWNER should fail with NFS4ERR_LOCKS_HELD
	var relArgs bytes.Buffer
	_ = xdr.WriteUint64(&relArgs, clientID)
	_ = xdr.WriteXDROpaque(&relArgs, []byte("held-handler-owner"))

	relResult := h.handleReleaseLockOwner(ctx, bytes.NewReader(relArgs.Bytes()))

	if relResult.Status != types.NFS4ERR_LOCKS_HELD {
		t.Errorf("RELEASE_LOCKOWNER with held locks status = %d, want NFS4ERR_LOCKS_HELD (%d)",
			relResult.Status, types.NFS4ERR_LOCKS_HELD)
	}
}

// ============================================================================
// CLOSE with Locks Held Handler Tests (Plan 10-03)
// ============================================================================

func TestHandleClose_LocksHeld(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})

	lm := lock.NewManager()
	sm := state.NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	h := NewHandler(nil, pfs, sm)

	clientID, openStateid, openSeqid := setupHandlerLockClient(t, h)

	// Acquire a lock
	acquireLockForTest(t, h, clientID, openStateid, openSeqid,
		[]byte("close-held-owner"), types.WRITE_LT, 0, 100)

	fileHandle := []byte("/export:lock-test-file")
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(fileHandle)),
	}
	copy(ctx.CurrentFH, fileHandle)

	// CLOSE should fail with NFS4ERR_LOCKS_HELD
	var closeArgs bytes.Buffer
	_ = xdr.WriteUint32(&closeArgs, openSeqid+2)  // seqid
	types.EncodeStateid4(&closeArgs, openStateid) // open_stateid

	closeResult := h.handleClose(ctx, bytes.NewReader(closeArgs.Bytes()))

	if closeResult.Status != types.NFS4ERR_LOCKS_HELD {
		t.Errorf("CLOSE with held locks status = %d, want NFS4ERR_LOCKS_HELD (%d)",
			closeResult.Status, types.NFS4ERR_LOCKS_HELD)
	}
}

func TestHandleClose_AfterUnlock(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})

	lm := lock.NewManager()
	sm := state.NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	h := NewHandler(nil, pfs, sm)

	clientID, openStateid, openSeqid := setupHandlerLockClient(t, h)

	// Acquire a lock
	lockStateid, lockSeqid := acquireLockForTest(t, h, clientID, openStateid, openSeqid,
		[]byte("close-unlock-owner"), types.WRITE_LT, 0, 100)

	fileHandle := []byte("/export:lock-test-file")
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(fileHandle)),
	}
	copy(ctx.CurrentFH, fileHandle)

	// LOCKU to release the lock
	var unlockArgs bytes.Buffer
	_ = xdr.WriteUint32(&unlockArgs, types.WRITE_LT)
	_ = xdr.WriteUint32(&unlockArgs, lockSeqid)
	types.EncodeStateid4(&unlockArgs, lockStateid)
	_ = xdr.WriteUint64(&unlockArgs, 0)
	_ = xdr.WriteUint64(&unlockArgs, 100)

	unlockResult := h.handleLockU(ctx, bytes.NewReader(unlockArgs.Bytes()))
	if unlockResult.Status != types.NFS4_OK {
		t.Fatalf("LOCKU failed: status=%d", unlockResult.Status)
	}

	// RELEASE_LOCKOWNER to clean up lock state
	relErr := sm.ReleaseLockOwner(clientID, []byte("close-unlock-owner"))
	if relErr != nil {
		t.Fatalf("ReleaseLockOwner failed: %v", relErr)
	}

	// CLOSE should succeed
	var closeArgs bytes.Buffer
	_ = xdr.WriteUint32(&closeArgs, openSeqid+2)  // seqid
	types.EncodeStateid4(&closeArgs, openStateid) // open_stateid

	closeResult := h.handleClose(ctx, bytes.NewReader(closeArgs.Bytes()))

	if closeResult.Status != types.NFS4_OK {
		t.Errorf("CLOSE after unlock status = %d, want NFS4_OK (%d)",
			closeResult.Status, types.NFS4_OK)
	}
}

// ============================================================================
// Full Lock Lifecycle End-to-End Test (Plan 10-03)
// ============================================================================

func TestFullLockLifecycle(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})

	lm := lock.NewManager()
	sm := state.NewStateManager(90 * time.Second)
	sm.SetLockManager(lm)
	h := NewHandler(nil, pfs, sm)

	// 1. SETCLIENTID + CONFIRM
	clientID, openStateid, openSeqid := setupHandlerLockClient(t, h)

	fileHandle := []byte("/export:lock-test-file")
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(fileHandle)),
	}
	copy(ctx.CurrentFH, fileHandle)

	// 2. LOCK with new_lock_owner on range [0, 100)
	var lockArgs1 bytes.Buffer
	_ = xdr.WriteUint32(&lockArgs1, types.WRITE_LT)
	_ = xdr.WriteUint32(&lockArgs1, 0)           // reclaim = false
	_ = xdr.WriteUint64(&lockArgs1, 0)           // offset
	_ = xdr.WriteUint64(&lockArgs1, 100)         // length
	_ = xdr.WriteUint32(&lockArgs1, 1)           // new_lock_owner = true
	_ = xdr.WriteUint32(&lockArgs1, openSeqid+1) // open_seqid
	types.EncodeStateid4(&lockArgs1, openStateid)
	_ = xdr.WriteUint32(&lockArgs1, 1)        // lock_seqid
	_ = xdr.WriteUint64(&lockArgs1, clientID) // lock_owner clientid
	_ = xdr.WriteXDROpaque(&lockArgs1, []byte("lifecycle-owner"))

	lockResult1 := h.handleLock(ctx, bytes.NewReader(lockArgs1.Bytes()))
	if lockResult1.Status != types.NFS4_OK {
		t.Fatalf("Step 2: LOCK failed: status=%d", lockResult1.Status)
	}

	// Parse lock stateid
	reader1 := bytes.NewReader(lockResult1.Data)
	_, _ = xdr.DecodeUint32(reader1) // status
	lockStateid1, err := types.DecodeStateid4(reader1)
	if err != nil {
		t.Fatalf("failed to decode lock stateid: %v", err)
	}
	t.Logf("Step 2: LOCK OK, lock stateid seqid=%d", lockStateid1.Seqid)

	// 3. LOCKT from same owner on same range: should show no conflict (same owner)
	var locktArgs1 bytes.Buffer
	_ = xdr.WriteUint32(&locktArgs1, types.WRITE_LT)
	_ = xdr.WriteUint64(&locktArgs1, 0)   // offset
	_ = xdr.WriteUint64(&locktArgs1, 100) // length
	_ = xdr.WriteUint64(&locktArgs1, clientID)
	_ = xdr.WriteXDROpaque(&locktArgs1, []byte("lifecycle-owner"))

	locktResult1 := h.handleLockT(ctx, bytes.NewReader(locktArgs1.Bytes()))
	if locktResult1.Status != types.NFS4_OK {
		t.Errorf("Step 3: LOCKT same owner status = %d, want NFS4_OK", locktResult1.Status)
	}

	// 4. LOCK existing owner on range [200, 300)
	var lockArgs2 bytes.Buffer
	_ = xdr.WriteUint32(&lockArgs2, types.WRITE_LT)
	_ = xdr.WriteUint32(&lockArgs2, 0)             // reclaim
	_ = xdr.WriteUint64(&lockArgs2, 200)           // offset
	_ = xdr.WriteUint64(&lockArgs2, 100)           // length
	_ = xdr.WriteUint32(&lockArgs2, 0)             // new_lock_owner = false
	types.EncodeStateid4(&lockArgs2, lockStateid1) // existing lock stateid
	_ = xdr.WriteUint32(&lockArgs2, 2)             // lock_seqid

	lockResult2 := h.handleLock(ctx, bytes.NewReader(lockArgs2.Bytes()))
	if lockResult2.Status != types.NFS4_OK {
		t.Fatalf("Step 4: second LOCK failed: status=%d", lockResult2.Status)
	}

	// Parse second lock stateid
	reader2 := bytes.NewReader(lockResult2.Data)
	_, _ = xdr.DecodeUint32(reader2)
	lockStateid2, err := types.DecodeStateid4(reader2)
	if err != nil {
		t.Fatalf("failed to decode second lock stateid: %v", err)
	}
	if lockStateid2.Seqid <= lockStateid1.Seqid {
		t.Errorf("Step 4: lock stateid seqid not incremented: %d <= %d",
			lockStateid2.Seqid, lockStateid1.Seqid)
	}
	t.Logf("Step 4: second LOCK OK, lock stateid seqid=%d", lockStateid2.Seqid)

	// 5. LOCKU first range [0, 100)
	var unlockArgs1 bytes.Buffer
	_ = xdr.WriteUint32(&unlockArgs1, types.WRITE_LT)
	_ = xdr.WriteUint32(&unlockArgs1, 3)             // lock-owner seqid
	types.EncodeStateid4(&unlockArgs1, lockStateid2) // use latest lock stateid
	_ = xdr.WriteUint64(&unlockArgs1, 0)             // offset
	_ = xdr.WriteUint64(&unlockArgs1, 100)           // length

	unlockResult1 := h.handleLockU(ctx, bytes.NewReader(unlockArgs1.Bytes()))
	if unlockResult1.Status != types.NFS4_OK {
		t.Fatalf("Step 5: LOCKU first range failed: status=%d", unlockResult1.Status)
	}

	// Parse updated lock stateid
	reader3 := bytes.NewReader(unlockResult1.Data)
	_, _ = xdr.DecodeUint32(reader3)
	lockStateid3, err := types.DecodeStateid4(reader3)
	if err != nil {
		t.Fatalf("failed to decode lock stateid after LOCKU: %v", err)
	}
	t.Logf("Step 5: LOCKU OK, lock stateid seqid=%d", lockStateid3.Seqid)

	// 6. LOCKT from different owner on second range [200, 300): should conflict
	var locktArgs2 bytes.Buffer
	_ = xdr.WriteUint32(&locktArgs2, types.WRITE_LT)
	_ = xdr.WriteUint64(&locktArgs2, 200)
	_ = xdr.WriteUint64(&locktArgs2, 100)
	_ = xdr.WriteUint64(&locktArgs2, clientID)
	_ = xdr.WriteXDROpaque(&locktArgs2, []byte("different-lifecycle-owner"))

	locktResult2 := h.handleLockT(ctx, bytes.NewReader(locktArgs2.Bytes()))
	if locktResult2.Status != types.NFS4ERR_DENIED {
		t.Errorf("Step 6: LOCKT different owner on locked range status = %d, want NFS4ERR_DENIED",
			locktResult2.Status)
	}

	// 7. LOCKU second range [200, 300)
	var unlockArgs2 bytes.Buffer
	_ = xdr.WriteUint32(&unlockArgs2, types.WRITE_LT)
	_ = xdr.WriteUint32(&unlockArgs2, 4) // lock-owner seqid
	types.EncodeStateid4(&unlockArgs2, lockStateid3)
	_ = xdr.WriteUint64(&unlockArgs2, 200)
	_ = xdr.WriteUint64(&unlockArgs2, 100)

	unlockResult2 := h.handleLockU(ctx, bytes.NewReader(unlockArgs2.Bytes()))
	if unlockResult2.Status != types.NFS4_OK {
		t.Fatalf("Step 7: LOCKU second range failed: status=%d", unlockResult2.Status)
	}
	t.Logf("Step 7: LOCKU OK")

	// 8. RELEASE_LOCKOWNER
	relErr := sm.ReleaseLockOwner(clientID, []byte("lifecycle-owner"))
	if relErr != nil {
		t.Fatalf("Step 8: ReleaseLockOwner failed: %v", relErr)
	}
	t.Logf("Step 8: RELEASE_LOCKOWNER OK")

	// 9. CLOSE (should succeed since no locks held)
	var closeArgs bytes.Buffer
	_ = xdr.WriteUint32(&closeArgs, openSeqid+2) // seqid
	types.EncodeStateid4(&closeArgs, openStateid)

	closeResult := h.handleClose(ctx, bytes.NewReader(closeArgs.Bytes()))
	if closeResult.Status != types.NFS4_OK {
		t.Fatalf("Step 9: CLOSE failed: status=%d", closeResult.Status)
	}
	t.Logf("Step 9: CLOSE OK -- full lifecycle complete")
}
