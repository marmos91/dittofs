package state

import (
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// Stateid Generation Tests
// ============================================================================

func TestGenerateStateidOther_TypeTag(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	openOther := sm.generateStateidOther(StateTypeOpen)
	if openOther[0] != StateTypeOpen {
		t.Errorf("open other[0] = %d, want %d", openOther[0], StateTypeOpen)
	}

	lockOther := sm.generateStateidOther(StateTypeLock)
	if lockOther[0] != StateTypeLock {
		t.Errorf("lock other[0] = %d, want %d", lockOther[0], StateTypeLock)
	}

	delegOther := sm.generateStateidOther(StateTypeDeleg)
	if delegOther[0] != StateTypeDeleg {
		t.Errorf("deleg other[0] = %d, want %d", delegOther[0], StateTypeDeleg)
	}
}

func TestGenerateStateidOther_BootEpoch(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	other := sm.generateStateidOther(StateTypeOpen)

	// Bytes 1-3 should contain boot epoch low 24 bits
	expectedByte1 := byte(sm.bootEpoch >> 16)
	expectedByte2 := byte(sm.bootEpoch >> 8)
	expectedByte3 := byte(sm.bootEpoch)

	if other[1] != expectedByte1 || other[2] != expectedByte2 || other[3] != expectedByte3 {
		t.Errorf("boot epoch bytes = [%x, %x, %x], want [%x, %x, %x]",
			other[1], other[2], other[3],
			expectedByte1, expectedByte2, expectedByte3)
	}
}

func TestGenerateStateidOther_Uniqueness(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	seen := make(map[[types.NFS4_OTHER_SIZE]byte]bool)
	for i := 0; i < 1000; i++ {
		other := sm.generateStateidOther(StateTypeOpen)
		if seen[other] {
			t.Fatalf("duplicate stateid other at iteration %d", i)
		}
		seen[other] = true
	}
}

func TestGenerateStateidOther_ConcurrentUniqueness(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	const numGoroutines = 100
	var wg sync.WaitGroup
	results := make([][types.NFS4_OTHER_SIZE]byte, numGoroutines)

	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			results[idx] = sm.generateStateidOther(StateTypeOpen)
		}(i)
	}
	wg.Wait()

	seen := make(map[[types.NFS4_OTHER_SIZE]byte]bool)
	for i, other := range results {
		if seen[other] {
			t.Fatalf("duplicate concurrent stateid other at index %d", i)
		}
		seen[other] = true
	}
}

func TestIsCurrentEpoch(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Current epoch
	other := sm.generateStateidOther(StateTypeOpen)
	if !sm.isCurrentEpoch(other) {
		t.Error("generated stateid should match current epoch")
	}

	// Wrong epoch (zero out epoch bytes)
	var badOther [types.NFS4_OTHER_SIZE]byte
	badOther[0] = StateTypeOpen
	badOther[1] = 0
	badOther[2] = 0
	badOther[3] = 0
	// Only matches if current epoch low 24 bits are 0 (extremely unlikely)
	if sm.bootEpoch&0xFFFFFF != 0 && sm.isCurrentEpoch(badOther) {
		t.Error("zeroed epoch bytes should not match current epoch")
	}
}

// ============================================================================
// Stateid Validation Tests
// ============================================================================

func TestValidateStateid_SpecialStateid_AllZeros(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	stateid := &types.Stateid4{Seqid: 0}
	// Other is default zero
	openState, err := sm.ValidateStateid(stateid, nil)
	if err != nil {
		t.Fatalf("ValidateStateid for all-zeros: %v", err)
	}
	if openState != nil {
		t.Error("special stateid should return nil openState")
	}
}

func TestValidateStateid_SpecialStateid_AllOnes(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// RFC 7530 Section 9.1.4.3: READ bypass stateid has seqid=0xFFFFFFFF, other=all-ones
	stateid := &types.Stateid4{Seqid: 0xFFFFFFFF}
	for i := range stateid.Other {
		stateid.Other[i] = 0xFF
	}

	openState, err := sm.ValidateStateid(stateid, nil)
	if err != nil {
		t.Fatalf("ValidateStateid for all-ones READ bypass: %v", err)
	}
	if openState != nil {
		t.Error("special stateid should return nil openState")
	}
}

func TestValidateStateid_NotFound(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Use a stateid with current epoch but not in the map
	other := sm.generateStateidOther(StateTypeOpen)
	stateid := &types.Stateid4{Seqid: 1, Other: other}

	_, err := sm.ValidateStateid(stateid, nil)
	if err == nil {
		t.Fatal("ValidateStateid should fail for unknown stateid")
	}

	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_BAD_STATEID {
		t.Errorf("status = %d, want NFS4ERR_BAD_STATEID (%d)",
			stateErr.Status, types.NFS4ERR_BAD_STATEID)
	}
}

func TestValidateStateid_StaleStateid(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Create a stateid with a wrong epoch
	var other [types.NFS4_OTHER_SIZE]byte
	other[0] = StateTypeOpen
	// Use different epoch bytes
	other[1] = 0xFF
	other[2] = 0xFF
	other[3] = 0xFF
	stateid := &types.Stateid4{Seqid: 1, Other: other}

	_, err := sm.ValidateStateid(stateid, nil)
	if err == nil {
		t.Fatal("ValidateStateid should fail for stale epoch")
	}

	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_STALE_STATEID {
		t.Errorf("status = %d, want NFS4ERR_STALE_STATEID (%d)",
			stateErr.Status, types.NFS4ERR_STALE_STATEID)
	}
}

func TestValidateStateid_Success(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Create an open state via OpenFile
	fileHandle := []byte("test-handle-123")
	result, err := sm.OpenFile(0, []byte("owner1"), 1, fileHandle, 1, 0, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	// Validate the returned stateid
	openState, err := sm.ValidateStateid(&result.Stateid, fileHandle)
	if err != nil {
		t.Fatalf("ValidateStateid: %v", err)
	}
	if openState == nil {
		t.Fatal("openState should not be nil for valid stateid")
	}
	if openState.ShareAccess != 1 {
		t.Errorf("ShareAccess = %d, want 1", openState.ShareAccess)
	}
}

func TestValidateStateid_OldSeqid(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fileHandle := []byte("test-handle-123")
	result, err := sm.OpenFile(0, []byte("owner1"), 1, fileHandle, 1, 0, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	// Confirm to increment seqid
	_, err = sm.ConfirmOpen(&result.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}

	// Now use the OLD seqid (1), current is 2
	oldStateid := result.Stateid // has seqid=1
	_, err = sm.ValidateStateid(&oldStateid, nil)
	if err == nil {
		t.Fatal("ValidateStateid should fail for old seqid")
	}

	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_OLD_STATEID {
		t.Errorf("status = %d, want NFS4ERR_OLD_STATEID (%d)",
			stateErr.Status, types.NFS4ERR_OLD_STATEID)
	}
}

func TestValidateStateid_FutureSeqid(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fileHandle := []byte("test-handle-123")
	result, err := sm.OpenFile(0, []byte("owner1"), 1, fileHandle, 1, 0, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	// Use a seqid higher than current
	futureStateid := result.Stateid
	futureStateid.Seqid = 99
	_, err = sm.ValidateStateid(&futureStateid, nil)
	if err == nil {
		t.Fatal("ValidateStateid should fail for future seqid")
	}

	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_BAD_STATEID {
		t.Errorf("status = %d, want NFS4ERR_BAD_STATEID (%d)",
			stateErr.Status, types.NFS4ERR_BAD_STATEID)
	}
}

func TestValidateStateid_FilehandleMismatch(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fileHandle := []byte("test-handle-123")
	result, err := sm.OpenFile(0, []byte("owner1"), 1, fileHandle, 1, 0, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	// Validate with different filehandle
	wrongFH := []byte("wrong-handle-456")
	_, err = sm.ValidateStateid(&result.Stateid, wrongFH)
	if err == nil {
		t.Fatal("ValidateStateid should fail for filehandle mismatch")
	}

	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_BAD_STATEID {
		t.Errorf("status = %d, want NFS4ERR_BAD_STATEID (%d)",
			stateErr.Status, types.NFS4ERR_BAD_STATEID)
	}
}

// ============================================================================
// Delegation Stateid Validation Tests
// ============================================================================

func TestValidateStateid_DelegationStateid_Success(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fileHandle := []byte("fh-deleg-validate")
	deleg := sm.GrantDelegation(100, fileHandle, types.OPEN_DELEGATE_WRITE)

	// Validate the delegation stateid
	openState, err := sm.ValidateStateid(&deleg.Stateid, fileHandle)
	if err != nil {
		t.Fatalf("ValidateStateid for delegation: %v", err)
	}
	if openState != nil {
		t.Error("delegation stateid should return nil openState")
	}
}

func TestValidateStateid_DelegationStateid_NotFound(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Create a delegation-type stateid that isn't in the map
	other := sm.generateStateidOther(StateTypeDeleg)
	stateid := &types.Stateid4{Seqid: 1, Other: other}

	_, err := sm.ValidateStateid(stateid, nil)
	if err == nil {
		t.Fatal("ValidateStateid should fail for unknown delegation stateid")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_BAD_STATEID {
		t.Errorf("status = %d, want NFS4ERR_BAD_STATEID (%d)",
			stateErr.Status, types.NFS4ERR_BAD_STATEID)
	}
}

func TestValidateStateid_DelegationStateid_Revoked(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fileHandle := []byte("fh-deleg-revoked")
	deleg := sm.GrantDelegation(100, fileHandle, types.OPEN_DELEGATE_WRITE)

	// Mark as revoked
	sm.mu.Lock()
	deleg.Revoked = true
	sm.mu.Unlock()

	_, err := sm.ValidateStateid(&deleg.Stateid, fileHandle)
	if err == nil {
		t.Fatal("ValidateStateid should fail for revoked delegation")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_BAD_STATEID {
		t.Errorf("status = %d, want NFS4ERR_BAD_STATEID (%d)",
			stateErr.Status, types.NFS4ERR_BAD_STATEID)
	}
}

func TestValidateStateid_DelegationStateid_FilehandleMismatch(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	deleg := sm.GrantDelegation(100, []byte("fh-deleg-mismatch"), types.OPEN_DELEGATE_READ)

	wrongFH := []byte("fh-wrong-handle")
	_, err := sm.ValidateStateid(&deleg.Stateid, wrongFH)
	if err == nil {
		t.Fatal("ValidateStateid should fail for filehandle mismatch")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_BAD_STATEID {
		t.Errorf("status = %d, want NFS4ERR_BAD_STATEID (%d)",
			stateErr.Status, types.NFS4ERR_BAD_STATEID)
	}
}

func TestValidateStateid_DelegationStateid_OldSeqid(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	deleg := sm.GrantDelegation(100, []byte("fh-deleg-old"), types.OPEN_DELEGATE_WRITE)

	// Use a seqid older than the current one
	oldStateid := types.Stateid4{Seqid: 0, Other: deleg.Stateid.Other}
	_, err := sm.ValidateStateid(&oldStateid, nil)
	if err == nil {
		t.Fatal("ValidateStateid should fail for old delegation seqid")
	}
	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_OLD_STATEID {
		t.Errorf("status = %d, want NFS4ERR_OLD_STATEID (%d)",
			stateErr.Status, types.NFS4ERR_OLD_STATEID)
	}
}

// ============================================================================
// IsSpecialStateid Tests
// ============================================================================

func TestIsSpecialStateid_Anonymous(t *testing.T) {
	stateid := &types.Stateid4{Seqid: 0}
	// Other is default all-zeros
	if !stateid.IsSpecialStateid() {
		t.Error("anonymous stateid (seqid=0, other=all-zeros) should be special")
	}
}

func TestIsSpecialStateid_ReadBypass(t *testing.T) {
	stateid := &types.Stateid4{Seqid: 0xFFFFFFFF}
	for i := range stateid.Other {
		stateid.Other[i] = 0xFF
	}
	if !stateid.IsSpecialStateid() {
		t.Error("READ bypass stateid (seqid=0xFFFFFFFF, other=all-ones) should be special")
	}
}

func TestIsSpecialStateid_NotSpecial(t *testing.T) {
	// seqid=1 with all-zeros other is NOT special
	stateid := &types.Stateid4{Seqid: 1}
	if stateid.IsSpecialStateid() {
		t.Error("stateid with seqid=1, other=all-zeros should NOT be special")
	}

	// seqid=0 with all-ones other is NOT special (wrong combination)
	stateid2 := &types.Stateid4{Seqid: 0}
	for i := range stateid2.Other {
		stateid2.Other[i] = 0xFF
	}
	if stateid2.IsSpecialStateid() {
		t.Error("stateid with seqid=0, other=all-ones should NOT be special")
	}

	// seqid=0xFFFFFFFF with all-zeros other is NOT special
	stateid3 := &types.Stateid4{Seqid: 0xFFFFFFFF}
	if stateid3.IsSpecialStateid() {
		t.Error("stateid with seqid=0xFFFFFFFF, other=all-zeros should NOT be special")
	}
}

// ============================================================================
// OpenFile Tests
// ============================================================================

func TestOpenFile_NewOwner(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	result, err := sm.OpenFile(0, []byte("owner1"), 1,
		[]byte("file-handle-1"),
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	// New owner: should require confirmation
	if result.RFlags&types.OPEN4_RESULT_CONFIRM == 0 {
		t.Error("new owner should have OPEN4_RESULT_CONFIRM set")
	}
	if result.Stateid.Seqid != 1 {
		t.Errorf("initial stateid seqid = %d, want 1", result.Stateid.Seqid)
	}
}

func TestOpenFile_ConfirmedOwer_SecondOpen(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// First OPEN
	result1, err := sm.OpenFile(0, []byte("owner1"), 1,
		[]byte("file-handle-1"),
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err != nil {
		t.Fatalf("OpenFile 1: %v", err)
	}

	// Confirm
	_, err = sm.ConfirmOpen(&result1.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}

	// Second OPEN (different file) with same confirmed owner
	result2, err := sm.OpenFile(0, []byte("owner1"), 3,
		[]byte("file-handle-2"),
		types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err != nil {
		t.Fatalf("OpenFile 2: %v", err)
	}

	// Confirmed owner: should NOT require confirmation
	if result2.RFlags&types.OPEN4_RESULT_CONFIRM != 0 {
		t.Error("confirmed owner should NOT have OPEN4_RESULT_CONFIRM")
	}
}

func TestOpenFile_SameFile_ShareAccumulation(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	fh := []byte("file-handle-1")

	// First OPEN with READ access
	result1, err := sm.OpenFile(0, []byte("owner1"), 1,
		fh,
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err != nil {
		t.Fatalf("OpenFile 1: %v", err)
	}

	// Confirm
	_, err = sm.ConfirmOpen(&result1.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}

	// Second OPEN on same file with WRITE access
	result2, err := sm.OpenFile(0, []byte("owner1"), 3,
		fh,
		types.OPEN4_SHARE_ACCESS_WRITE,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err != nil {
		t.Fatalf("OpenFile 2: %v", err)
	}

	// Validate the accumulated state
	openState := sm.GetOpenState(result2.Stateid.Other)
	if openState == nil {
		t.Fatal("open state not found")
	}

	// Should have both READ and WRITE accumulated (OR'd)
	expectedAccess := uint32(types.OPEN4_SHARE_ACCESS_READ | types.OPEN4_SHARE_ACCESS_WRITE)
	if openState.ShareAccess != expectedAccess {
		t.Errorf("ShareAccess = %d, want %d (READ|WRITE)", openState.ShareAccess, expectedAccess)
	}
}

func TestOpenFile_SeqidReplay(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// First OPEN with seqid=1
	_, err := sm.OpenFile(0, []byte("owner1"), 1,
		[]byte("file-handle-1"),
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	// Cache a result
	sm.CacheOpenResult(0, []byte("owner1"), types.NFS4_OK, []byte("cached-data"))

	// Replay with same seqid=1
	replay, err := sm.OpenFile(0, []byte("owner1"), 1,
		[]byte("file-handle-1"),
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err != nil {
		t.Fatalf("OpenFile replay: %v", err)
	}
	if !replay.IsReplay {
		t.Error("should be detected as replay")
	}
	if replay.CachedStatus != types.NFS4_OK {
		t.Errorf("cached status = %d, want NFS4_OK", replay.CachedStatus)
	}
}

func TestOpenFile_BadSeqid(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// First OPEN with seqid=1
	_, err := sm.OpenFile(0, []byte("owner1"), 1,
		[]byte("file-handle-1"),
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	// Use bad seqid (3 instead of expected 2)
	_, err = sm.OpenFile(0, []byte("owner1"), 3,
		[]byte("file-handle-1"),
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err == nil {
		t.Fatal("OpenFile should fail with bad seqid")
	}
	if err != ErrBadSeqid {
		t.Errorf("expected ErrBadSeqid, got %v", err)
	}
}

// ============================================================================
// ConfirmOpen Tests
// ============================================================================

func TestConfirmOpen_Success(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	result, err := sm.OpenFile(0, []byte("owner1"), 1,
		[]byte("fh"), 1, 0, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	confirmed, err := sm.ConfirmOpen(&result.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}

	// Seqid should be incremented
	if confirmed.Seqid != result.Stateid.Seqid+1 {
		t.Errorf("confirmed seqid = %d, want %d", confirmed.Seqid, result.Stateid.Seqid+1)
	}
}

func TestConfirmOpen_BadStateid(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	badStateid := &types.Stateid4{Seqid: 1}
	_, err := sm.ConfirmOpen(badStateid, 1)
	if err == nil {
		t.Fatal("ConfirmOpen should fail for unknown stateid")
	}
}

func TestConfirmOpen_BadSeqid(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	result, err := sm.OpenFile(0, []byte("owner1"), 1,
		[]byte("fh"), 1, 0, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	// Use wrong seqid (5 instead of expected 2)
	_, err = sm.ConfirmOpen(&result.Stateid, 5)
	if err == nil {
		t.Fatal("ConfirmOpen should fail with bad seqid")
	}
}

// ============================================================================
// CloseFile Tests
// ============================================================================

func TestCloseFile_Success(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Open a file
	result, err := sm.OpenFile(0, []byte("owner1"), 1,
		[]byte("fh"), 1, 0, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	// Confirm
	confirmedStateid, err := sm.ConfirmOpen(&result.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}

	// Close
	closedStateid, err := sm.CloseFile(confirmedStateid, 3)
	if err != nil {
		t.Fatalf("CloseFile: %v", err)
	}

	// Should return zeroed stateid
	if closedStateid.Seqid != 0 {
		t.Errorf("closed seqid = %d, want 0", closedStateid.Seqid)
	}
	for i, b := range closedStateid.Other {
		if b != 0 {
			t.Errorf("closed other[%d] = %d, want 0", i, b)
		}
	}

	// State should be removed
	if sm.GetOpenState(result.Stateid.Other) != nil {
		t.Error("open state should be removed after CLOSE")
	}
}

func TestCloseFile_SpecialStateid(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Close with anonymous (all-zeros) stateid
	zeroed := &types.Stateid4{Seqid: 0}
	closedStateid, err := sm.CloseFile(zeroed, 1)
	if err != nil {
		t.Fatalf("CloseFile with special stateid: %v", err)
	}
	if closedStateid.Seqid != 0 {
		t.Errorf("closed seqid = %d, want 0", closedStateid.Seqid)
	}
}

func TestCloseFile_CleansUpOwner(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Open a file
	result, err := sm.OpenFile(0, []byte("owner1"), 1,
		[]byte("fh"), 1, 0, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	// Confirm
	confirmed, err := sm.ConfirmOpen(&result.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}

	// Close
	_, err = sm.CloseFile(confirmed, 3)
	if err != nil {
		t.Fatalf("CloseFile: %v", err)
	}

	// Owner should be removed (no more open states)
	sm.mu.RLock()
	ownerKey := makeOwnerKey(0, []byte("owner1"))
	_, ownerExists := sm.openOwners[ownerKey]
	sm.mu.RUnlock()

	if ownerExists {
		t.Error("owner should be removed when last open state is closed")
	}
}

func TestCloseFile_BadStateid(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Close with unknown stateid (non-special)
	badStateid := &types.Stateid4{Seqid: 1}
	badStateid.Other[0] = StateTypeOpen
	badStateid.Other[1] = byte(sm.bootEpoch >> 16)
	badStateid.Other[2] = byte(sm.bootEpoch >> 8)
	badStateid.Other[3] = byte(sm.bootEpoch)
	badStateid.Other[4] = 0xFF // unique sequence that doesn't exist

	_, err := sm.CloseFile(badStateid, 1)
	if err == nil {
		t.Fatal("CloseFile should fail for unknown stateid")
	}
}

// ============================================================================
// DowngradeOpen Tests
// ============================================================================

func TestDowngradeOpen_Success(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Open with BOTH access
	result, err := sm.OpenFile(0, []byte("owner1"), 1,
		[]byte("fh"),
		types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	// Confirm
	confirmed, err := sm.ConfirmOpen(&result.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}

	// Downgrade to READ only
	downgraded, err := sm.DowngradeOpen(confirmed, 3,
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
	)
	if err != nil {
		t.Fatalf("DowngradeOpen: %v", err)
	}

	// Seqid should be incremented
	if downgraded.Seqid != confirmed.Seqid+1 {
		t.Errorf("downgraded seqid = %d, want %d", downgraded.Seqid, confirmed.Seqid+1)
	}

	// Verify share_access was updated
	openState := sm.GetOpenState(result.Stateid.Other)
	if openState.ShareAccess != types.OPEN4_SHARE_ACCESS_READ {
		t.Errorf("ShareAccess = %d, want OPEN4_SHARE_ACCESS_READ (%d)",
			openState.ShareAccess, types.OPEN4_SHARE_ACCESS_READ)
	}
}

func TestDowngradeOpen_CannotAddBits(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Open with READ only
	result, err := sm.OpenFile(0, []byte("owner1"), 1,
		[]byte("fh"),
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	// Confirm
	confirmed, err := sm.ConfirmOpen(&result.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}

	// Try to "downgrade" to BOTH (adds WRITE bit) - should fail
	_, err = sm.DowngradeOpen(confirmed, 3,
		types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE,
	)
	if err == nil {
		t.Fatal("DowngradeOpen should fail when trying to add bits")
	}

	stateErr, ok := err.(*NFS4StateError)
	if !ok {
		t.Fatalf("expected NFS4StateError, got %T", err)
	}
	if stateErr.Status != types.NFS4ERR_INVAL {
		t.Errorf("status = %d, want NFS4ERR_INVAL", stateErr.Status)
	}
}

func TestDowngradeOpen_ZeroAccess(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// Open with READ access
	result, err := sm.OpenFile(0, []byte("owner1"), 1,
		[]byte("fh"),
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	// Confirm
	confirmed, err := sm.ConfirmOpen(&result.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}

	// Downgrade to zero access - should fail
	_, err = sm.DowngradeOpen(confirmed, 3, 0, 0)
	if err == nil {
		t.Fatal("DowngradeOpen should fail with zero share_access")
	}
}

// ============================================================================
// Open-Owner SeqID Validation Tests
// ============================================================================

func TestOpenOwner_ValidateSeqID(t *testing.T) {
	owner := &OpenOwner{LastSeqID: 5}

	tests := []struct {
		name     string
		seqid    uint32
		expected SeqIDValidation
	}{
		{"expected", 6, SeqIDOK},
		{"replay", 5, SeqIDReplay},
		{"too_old", 4, SeqIDBad},
		{"too_new", 8, SeqIDBad},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := owner.ValidateSeqID(tt.seqid)
			if got != tt.expected {
				t.Errorf("ValidateSeqID(%d) = %d, want %d", tt.seqid, got, tt.expected)
			}
		})
	}
}

func TestNextSeqID_WrapAround(t *testing.T) {
	// Normal increment
	if next := nextSeqID(1); next != 2 {
		t.Errorf("nextSeqID(1) = %d, want 2", next)
	}

	// Wrap around at max uint32
	if next := nextSeqID(0xFFFFFFFF); next != 1 {
		t.Errorf("nextSeqID(0xFFFFFFFF) = %d, want 1 (wrap to 1, not 0)", next)
	}
}

// ============================================================================
// RenewLease Tests
// ============================================================================

func TestRenewLease_Success(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	// Create and confirm a client
	result, err := sm.SetClientID("client-renew", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}
	err = sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier)
	if err != nil {
		t.Fatalf("ConfirmClientID: %v", err)
	}

	// Renew should succeed
	err = sm.RenewLease(result.ClientID)
	if err != nil {
		t.Fatalf("RenewLease: %v", err)
	}

	// Verify LastRenewal was updated
	record := sm.GetClient(result.ClientID)
	if record == nil {
		t.Fatal("client record not found")
	}
	if record.LastRenewal.IsZero() {
		t.Error("LastRenewal should be set after RenewLease")
	}
}

func TestRenewLease_UnknownClient(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	err := sm.RenewLease(99999)
	if err == nil {
		t.Fatal("RenewLease should fail for unknown client")
	}
	if err != ErrStaleClientID {
		t.Errorf("expected ErrStaleClientID, got %v", err)
	}
}

func TestRenewLease_UnconfirmedClient(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

	// Create but do NOT confirm
	result, err := sm.SetClientID("client-unconfirmed", verifier, callback, "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("SetClientID: %v", err)
	}

	// Renew should fail for unconfirmed client
	err = sm.RenewLease(result.ClientID)
	if err == nil {
		t.Fatal("RenewLease should fail for unconfirmed client")
	}
	if err != ErrStaleClientID {
		t.Errorf("expected ErrStaleClientID, got %v", err)
	}
}

// ============================================================================
// Integration: Full OPEN -> CONFIRM -> CLOSE lifecycle
// ============================================================================

func TestFullLifecycle_OpenConfirmClose(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	// OPEN (seqid=1)
	openResult, err := sm.OpenFile(0, []byte("owner1"), 1,
		[]byte("file-handle-test"),
		types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE,
		types.CLAIM_NULL,
	)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if openResult.RFlags&types.OPEN4_RESULT_CONFIRM == 0 {
		t.Error("new owner should require confirmation")
	}

	// OPEN_CONFIRM (seqid=2)
	confirmedStateid, err := sm.ConfirmOpen(&openResult.Stateid, 2)
	if err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}

	// Validate the confirmed stateid
	openState, err := sm.ValidateStateid(confirmedStateid, []byte("file-handle-test"))
	if err != nil {
		t.Fatalf("ValidateStateid: %v", err)
	}
	if !openState.Confirmed {
		t.Error("state should be confirmed")
	}
	if openState.ShareAccess != types.OPEN4_SHARE_ACCESS_BOTH {
		t.Errorf("ShareAccess = %d, want BOTH", openState.ShareAccess)
	}

	// CLOSE (seqid=3)
	closedStateid, err := sm.CloseFile(confirmedStateid, 3)
	if err != nil {
		t.Fatalf("CloseFile: %v", err)
	}

	// Verify closed
	if closedStateid.Seqid != 0 {
		t.Errorf("closed seqid = %d, want 0", closedStateid.Seqid)
	}

	// State should be gone
	if sm.GetOpenState(openResult.Stateid.Other) != nil {
		t.Error("open state should be removed after CLOSE")
	}
}

// ============================================================================
// FreeStateid Tests
// ============================================================================

func TestFreeStateid(t *testing.T) {
	t.Run("free_lock_stateid", func(t *testing.T) {
		lm := lock.NewManager()
		sm := NewStateManager(90 * time.Second)
		sm.SetLockManager(lm)
		defer sm.Shutdown()

		// Create open state
		fh := []byte("fh-free-lock")
		openResult, err := sm.OpenFile(0, []byte("owner1"), 1, fh,
			types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}

		// Confirm open
		confirmedStateid, err := sm.ConfirmOpen(&openResult.Stateid, 2)
		if err != nil {
			t.Fatalf("ConfirmOpen: %v", err)
		}

		// Create lock state
		lockResult, err := sm.LockNew(
			0, []byte("lock-owner1"), 1,
			confirmedStateid, 3,
			fh, types.WRITE_LT, 0, 100, false,
		)
		if err != nil {
			t.Fatalf("LockNew: %v", err)
		}

		// Free the lock stateid
		err = sm.FreeStateid(0, &lockResult.Stateid)
		if err != nil {
			t.Fatalf("FreeStateid lock: %v", err)
		}

		// Verify lock state is gone
		sm.mu.RLock()
		_, exists := sm.lockStateByOther[lockResult.Stateid.Other]
		sm.mu.RUnlock()

		if exists {
			t.Error("Lock state should be removed after FreeStateid")
		}
	})

	t.Run("free_open_stateid", func(t *testing.T) {
		sm := NewStateManager(90 * time.Second)
		defer sm.Shutdown()

		fh := []byte("fh-free-open")
		openResult, err := sm.OpenFile(0, []byte("owner1"), 1, fh,
			types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}

		// Confirm open
		_, err = sm.ConfirmOpen(&openResult.Stateid, 2)
		if err != nil {
			t.Fatalf("ConfirmOpen: %v", err)
		}

		// Free the open stateid (no locks held)
		err = sm.FreeStateid(0, &openResult.Stateid)
		if err != nil {
			t.Fatalf("FreeStateid open: %v", err)
		}

		// Verify open state is gone
		if sm.GetOpenState(openResult.Stateid.Other) != nil {
			t.Error("Open state should be removed after FreeStateid")
		}
	})

	t.Run("free_open_with_locks_held", func(t *testing.T) {
		lm := lock.NewManager()
		sm := NewStateManager(90 * time.Second)
		sm.SetLockManager(lm)
		defer sm.Shutdown()

		fh := []byte("fh-free-open-locked")
		openResult, err := sm.OpenFile(0, []byte("owner1"), 1, fh,
			types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}

		confirmedStateid, err := sm.ConfirmOpen(&openResult.Stateid, 2)
		if err != nil {
			t.Fatalf("ConfirmOpen: %v", err)
		}

		// Create a lock
		_, err = sm.LockNew(
			0, []byte("lock-owner1"), 1,
			confirmedStateid, 3,
			fh, types.WRITE_LT, 0, 100, false,
		)
		if err != nil {
			t.Fatalf("LockNew: %v", err)
		}

		// Try to free open stateid -- should fail with NFS4ERR_LOCKS_HELD
		err = sm.FreeStateid(0, &openResult.Stateid)
		if err == nil {
			t.Fatal("Expected NFS4ERR_LOCKS_HELD error")
		}
		stateErr, ok := err.(*NFS4StateError)
		if !ok {
			t.Fatalf("Expected NFS4StateError, got %T", err)
		}
		if stateErr.Status != types.NFS4ERR_LOCKS_HELD {
			t.Errorf("Status = %d, want NFS4ERR_LOCKS_HELD (%d)",
				stateErr.Status, types.NFS4ERR_LOCKS_HELD)
		}
	})

	t.Run("free_delegation_stateid", func(t *testing.T) {
		sm := NewStateManager(90 * time.Second)
		defer sm.Shutdown()

		fh := []byte("fh-free-deleg")
		deleg := sm.GrantDelegation(100, fh, types.OPEN_DELEGATE_WRITE)

		// Free the delegation stateid
		err := sm.FreeStateid(100, &deleg.Stateid)
		if err != nil {
			t.Fatalf("FreeStateid delegation: %v", err)
		}

		// Verify delegation is gone
		sm.mu.RLock()
		_, exists := sm.delegByOther[deleg.Stateid.Other]
		sm.mu.RUnlock()

		if exists {
			t.Error("Delegation should be removed after FreeStateid")
		}
	})

	t.Run("bad_stateid", func(t *testing.T) {
		sm := NewStateManager(90 * time.Second)
		defer sm.Shutdown()

		// Create a stateid with current epoch but not in any map
		other := sm.generateStateidOther(StateTypeOpen)
		stateid := &types.Stateid4{Seqid: 1, Other: other}

		err := sm.FreeStateid(0, stateid)
		if err == nil {
			t.Fatal("Expected NFS4ERR_BAD_STATEID error")
		}
		stateErr, ok := err.(*NFS4StateError)
		if !ok {
			t.Fatalf("Expected NFS4StateError, got %T", err)
		}
		if stateErr.Status != types.NFS4ERR_BAD_STATEID {
			t.Errorf("Status = %d, want NFS4ERR_BAD_STATEID (%d)",
				stateErr.Status, types.NFS4ERR_BAD_STATEID)
		}
	})

	t.Run("special_stateid", func(t *testing.T) {
		sm := NewStateManager(90 * time.Second)
		defer sm.Shutdown()

		// All-zeros (anonymous)
		zeroStateid := &types.Stateid4{Seqid: 0}
		err := sm.FreeStateid(0, zeroStateid)
		if err == nil {
			t.Fatal("Expected NFS4ERR_BAD_STATEID for all-zeros stateid")
		}
		stateErr, ok := err.(*NFS4StateError)
		if !ok {
			t.Fatalf("Expected NFS4StateError, got %T", err)
		}
		if stateErr.Status != types.NFS4ERR_BAD_STATEID {
			t.Errorf("Status = %d, want NFS4ERR_BAD_STATEID", stateErr.Status)
		}

		// All-ones (read bypass)
		onesStateid := &types.Stateid4{Seqid: 0xFFFFFFFF}
		for i := range onesStateid.Other {
			onesStateid.Other[i] = 0xFF
		}
		err = sm.FreeStateid(0, onesStateid)
		if err == nil {
			t.Fatal("Expected NFS4ERR_BAD_STATEID for all-ones stateid")
		}
		stateErr, ok = err.(*NFS4StateError)
		if !ok {
			t.Fatalf("Expected NFS4StateError, got %T", err)
		}
		if stateErr.Status != types.NFS4ERR_BAD_STATEID {
			t.Errorf("Status = %d, want NFS4ERR_BAD_STATEID", stateErr.Status)
		}
	})
}

func TestFreeStateid_Concurrent(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()

	// Create multiple delegations
	const numDelegs = 10
	delegations := make([]*DelegationState, numDelegs)
	for i := 0; i < numDelegs; i++ {
		fh := []byte("fh-concurrent-free-" + string(rune('A'+i)))
		delegations[i] = sm.GrantDelegation(uint64(100+i), fh, types.OPEN_DELEGATE_READ)
	}

	// Concurrently free all delegations
	var wg sync.WaitGroup
	errCh := make(chan error, numDelegs)

	for i := 0; i < numDelegs; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			err := sm.FreeStateid(uint64(100+idx), &delegations[idx].Stateid)
			if err != nil {
				errCh <- err
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("Concurrent FreeStateid error: %v", err)
	}

	// Verify all delegations are gone
	sm.mu.RLock()
	remainingDelegs := len(sm.delegByOther)
	sm.mu.RUnlock()

	if remainingDelegs != 0 {
		t.Errorf("Expected 0 delegations remaining, got %d", remainingDelegs)
	}
}

// ============================================================================
// TestStateids Tests
// ============================================================================

func TestTestStateids(t *testing.T) {
	t.Run("all_valid", func(t *testing.T) {
		sm := NewStateManager(90 * time.Second)
		defer sm.Shutdown()

		// Create several stateids
		fh1 := []byte("fh-test-valid-1")
		result1, err := sm.OpenFile(0, []byte("owner1"), 1, fh1,
			types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
		if err != nil {
			t.Fatalf("OpenFile 1: %v", err)
		}

		fh2 := []byte("fh-test-valid-2")
		deleg := sm.GrantDelegation(100, fh2, types.OPEN_DELEGATE_READ)

		stateids := []types.Stateid4{result1.Stateid, deleg.Stateid}
		results := sm.TestStateids(stateids)

		if len(results) != 2 {
			t.Fatalf("Expected 2 results, got %d", len(results))
		}
		for i, status := range results {
			if status != types.NFS4_OK {
				t.Errorf("Result[%d] = %d, want NFS4_OK", i, status)
			}
		}
	})

	t.Run("mixed_valid_invalid", func(t *testing.T) {
		sm := NewStateManager(90 * time.Second)
		defer sm.Shutdown()

		// Create a valid open state
		fh := []byte("fh-test-mixed")
		openResult, err := sm.OpenFile(0, []byte("owner1"), 1, fh,
			types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}

		// Create an invalid stateid (current epoch but not in maps)
		invalidOther := sm.generateStateidOther(StateTypeOpen)
		invalidStateid := types.Stateid4{Seqid: 1, Other: invalidOther}

		// Create a stale stateid (wrong epoch)
		var staleOther [types.NFS4_OTHER_SIZE]byte
		staleOther[0] = StateTypeOpen
		staleOther[1] = 0xFF
		staleOther[2] = 0xFF
		staleOther[3] = 0xFF
		staleStateid := types.Stateid4{Seqid: 1, Other: staleOther}

		stateids := []types.Stateid4{openResult.Stateid, invalidStateid, staleStateid}
		results := sm.TestStateids(stateids)

		if len(results) != 3 {
			t.Fatalf("Expected 3 results, got %d", len(results))
		}
		if results[0] != types.NFS4_OK {
			t.Errorf("Valid stateid: status = %d, want NFS4_OK", results[0])
		}
		if results[1] != types.NFS4ERR_BAD_STATEID {
			t.Errorf("Invalid stateid: status = %d, want NFS4ERR_BAD_STATEID", results[1])
		}
		if results[2] != types.NFS4ERR_STALE_STATEID {
			t.Errorf("Stale stateid: status = %d, want NFS4ERR_STALE_STATEID", results[2])
		}
	})

	t.Run("empty_list", func(t *testing.T) {
		sm := NewStateManager(90 * time.Second)
		defer sm.Shutdown()

		results := sm.TestStateids([]types.Stateid4{})
		if len(results) != 0 {
			t.Errorf("Expected empty results for empty input, got %d", len(results))
		}
	})

	t.Run("expired_stateid", func(t *testing.T) {
		sm := NewStateManager(50 * time.Millisecond) // Short lease
		defer sm.Shutdown()

		// Create and confirm a v4.0 client with a short lease
		verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
		callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

		clientResult, err := sm.SetClientID("expired-test", verifier, callback, "10.0.0.1:1234")
		if err != nil {
			t.Fatalf("SetClientID: %v", err)
		}
		err = sm.ConfirmClientID(clientResult.ClientID, clientResult.ConfirmVerifier)
		if err != nil {
			t.Fatalf("ConfirmClientID: %v", err)
		}

		// Open a file with this client
		fh := []byte("fh-expired-test")
		openResult, err := sm.OpenFile(clientResult.ClientID, []byte("owner1"), 1, fh,
			types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}

		// Wait for lease to expire
		time.Sleep(100 * time.Millisecond)

		stateids := []types.Stateid4{openResult.Stateid}
		results := sm.TestStateids(stateids)

		if len(results) != 1 {
			t.Fatalf("Expected 1 result, got %d", len(results))
		}
		// Should be expired or bad (depends on whether lease cleanup already ran)
		if results[0] != types.NFS4ERR_EXPIRED && results[0] != types.NFS4ERR_BAD_STATEID {
			t.Errorf("Expired stateid: status = %d, want NFS4ERR_EXPIRED or NFS4ERR_BAD_STATEID", results[0])
		}
	})

	t.Run("no_lease_renewal", func(t *testing.T) {
		sm := NewStateManager(90 * time.Second)
		defer sm.Shutdown()

		// Create and confirm a v4.0 client
		verifier := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
		callback := CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "10.0.0.1.8.1"}

		clientResult, err := sm.SetClientID("no-renew-test", verifier, callback, "10.0.0.1:1234")
		if err != nil {
			t.Fatalf("SetClientID: %v", err)
		}
		err = sm.ConfirmClientID(clientResult.ClientID, clientResult.ConfirmVerifier)
		if err != nil {
			t.Fatalf("ConfirmClientID: %v", err)
		}

		// Open a file with this client
		fh := []byte("fh-no-renew")
		openResult, err := sm.OpenFile(clientResult.ClientID, []byte("owner1"), 1, fh,
			types.OPEN4_SHARE_ACCESS_READ, types.OPEN4_SHARE_DENY_NONE, types.CLAIM_NULL)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}

		// Record last renewal time
		client := sm.GetClient(clientResult.ClientID)
		lastRenew := client.LastRenewal

		// Wait a bit to detect renewal
		time.Sleep(5 * time.Millisecond)

		// TestStateids should NOT renew the lease
		stateids := []types.Stateid4{openResult.Stateid}
		results := sm.TestStateids(stateids)

		if results[0] != types.NFS4_OK {
			t.Fatalf("TestStateids should return NFS4_OK, got %d", results[0])
		}

		// Check that LastRenewal was NOT updated
		client = sm.GetClient(clientResult.ClientID)
		if client.LastRenewal != lastRenew {
			t.Error("TestStateids should NOT renew the lease (read-only operation)")
		}
	})
}
