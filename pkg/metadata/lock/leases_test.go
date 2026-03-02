package lock

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// RequestLease Tests
// ============================================================================

func TestRequestLease_GrantFileLeaseR(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	state, epoch, err := mgr.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead, false)

	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead, state, "should grant Read lease")
	assert.Equal(t, uint16(1), epoch, "new lease should start at epoch 1")
}

func TestRequestLease_GrantFileLeaseRW(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	state, epoch, err := mgr.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite, false)

	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead|LeaseStateWrite, state)
	assert.Equal(t, uint16(1), epoch)
}

func TestRequestLease_GrantFileLeaseRWH(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	state, epoch, err := mgr.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite|LeaseStateHandle, false)

	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead|LeaseStateWrite|LeaseStateHandle, state)
	assert.Equal(t, uint16(1), epoch)
}

func TestRequestLease_GrantDirectoryLeaseR(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	state, epoch, err := mgr.RequestLease(ctx, FileHandle("dir1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead, true)

	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead, state)
	assert.Equal(t, uint16(1), epoch)
}

func TestRequestLease_GrantDirectoryLeaseRH(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	state, epoch, err := mgr.RequestLease(ctx, FileHandle("dir1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateHandle, true)

	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead|LeaseStateHandle, state)
	assert.Equal(t, uint16(1), epoch)
}

func TestRequestLease_RejectInvalidDirectoryState_W(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	state, _, err := mgr.RequestLease(ctx, FileHandle("dir1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite, true)

	require.NoError(t, err)
	assert.Equal(t, LeaseStateNone, state, "should reject Write on directory")
}

func TestRequestLease_RejectInvalidDirectoryState_RWH(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	state, _, err := mgr.RequestLease(ctx, FileHandle("dir1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite|LeaseStateHandle, true)

	require.NoError(t, err)
	assert.Equal(t, LeaseStateNone, state, "should reject RWH on directory")
}

func TestRequestLease_SameKeyUpgrade_R_to_RW(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	// First: grant R
	state, epoch, err := mgr.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead, state)
	assert.Equal(t, uint16(1), epoch)

	// Upgrade to RW
	state, epoch, err = mgr.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite, false)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead|LeaseStateWrite, state)
	assert.Equal(t, uint16(2), epoch, "epoch should increment on upgrade")
}

func TestRequestLease_SameKeyUpgrade_R_to_RH(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	// First: grant R
	_, _, err := mgr.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead, false)
	require.NoError(t, err)

	// Upgrade to RH
	state, epoch, err := mgr.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateHandle, false)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead|LeaseStateHandle, state)
	assert.Equal(t, uint16(2), epoch)
}

func TestRequestLease_SameKeySameState_NoEpochChange(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	// First: grant R
	state, epoch, err := mgr.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead, state)
	assert.Equal(t, uint16(1), epoch)

	// Request same state again
	state, epoch, err = mgr.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead, state)
	assert.Equal(t, uint16(1), epoch, "epoch should not change for same state")
}

func TestRequestLease_SameKeyDowngrade_Rejected(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	// First: grant RWH
	_, _, err := mgr.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite|LeaseStateHandle, false)
	require.NoError(t, err)

	// Attempt downgrade to R
	state, _, err := mgr.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateNone, state, "downgrade should be rejected")
}

func TestRequestLease_CrossKeyConflict(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	key1 := [16]byte{1, 0, 0, 0}
	key2 := [16]byte{2, 0, 0, 0}
	parentKey := [16]byte{}

	// First client gets RW lease
	state, _, err := mgr.RequestLease(ctx, FileHandle("file1"), key1, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite, false)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead|LeaseStateWrite, state)

	// Second client requests R lease on same file - conflicts with Write
	state, _, err = mgr.RequestLease(ctx, FileHandle("file1"), key2, parentKey, "owner2", "client2", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateNone, state, "should return None due to conflict")
}

func TestRequestLease_MultipleReadLeasesNoConflict(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	key1 := [16]byte{1, 0, 0, 0}
	key2 := [16]byte{2, 0, 0, 0}
	parentKey := [16]byte{}

	// First client gets R lease
	state, _, err := mgr.RequestLease(ctx, FileHandle("file1"), key1, parentKey, "owner1", "client1", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead, state)

	// Second client gets R lease on same file - no conflict
	state, _, err = mgr.RequestLease(ctx, FileHandle("file1"), key2, parentKey, "owner2", "client2", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead, state, "Read leases should not conflict")
}

func TestRequestLease_InvalidFileState(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	// Write alone is invalid for files
	state, _, err := mgr.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateWrite, false)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateNone, state, "Write alone should be invalid")
}

// ============================================================================
// AcknowledgeLeaseBreak Tests
// ============================================================================

func TestAcknowledgeLeaseBreak_CompletesBreak(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	key1 := [16]byte{1, 0, 0, 0}
	key2 := [16]byte{2, 0, 0, 0}
	parentKey := [16]byte{}

	// Setup: register a break callback that tracks breaks
	var breakCalled bool
	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lock *UnifiedLock, breakToState uint32) {
			breakCalled = true
			// Simulate: set the lock to breaking state
			lock.Lease.Breaking = true
			lock.Lease.BreakToState = breakToState
			lock.Lease.BreakStarted = time.Now()
		},
	})

	// Grant RW lease to key1
	state, _, err := mgr.RequestLease(ctx, FileHandle("file1"), key1, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite, false)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead|LeaseStateWrite, state)

	// Request from key2 triggers break on key1
	state, _, err = mgr.RequestLease(ctx, FileHandle("file1"), key2, parentKey, "owner2", "client2", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateNone, state, "should return None while break pending")
	assert.True(t, breakCalled, "break callback should have been called")

	// Acknowledge the break - accept None state (break was to None)
	err = mgr.AcknowledgeLeaseBreak(ctx, key1, LeaseStateNone, 0)
	require.NoError(t, err)

	// Lease should be removed after ack to None
	_, _, found := mgr.GetLeaseState(ctx, key1)
	assert.False(t, found, "lease should be removed after ack to None")
}

func TestAcknowledgeLeaseBreak_ToReadState(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	key1 := [16]byte{1, 0, 0, 0}
	parentKey := [16]byte{}

	// Grant RW lease
	_, _, err := mgr.RequestLease(ctx, FileHandle("file1"), key1, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite, false)
	require.NoError(t, err)

	// Manually set the lease to breaking state (simulating break to Read)
	mgr.mu.Lock()
	for _, locks := range mgr.unifiedLocks {
		for _, lock := range locks {
			if lock.Lease != nil && lock.Lease.LeaseKey == key1 {
				lock.Lease.Breaking = true
				lock.Lease.BreakToState = LeaseStateRead
				lock.Lease.BreakStarted = time.Now()
			}
		}
	}
	mgr.mu.Unlock()

	// Acknowledge to Read
	err = mgr.AcknowledgeLeaseBreak(ctx, key1, LeaseStateRead, 0)
	require.NoError(t, err)

	// Verify state was updated
	state, _, found := mgr.GetLeaseState(ctx, key1)
	assert.True(t, found)
	assert.Equal(t, LeaseStateRead, state)
}

func TestAcknowledgeLeaseBreak_NoActiveBreak(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	// Grant a lease (not breaking)
	_, _, err := mgr.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead, false)
	require.NoError(t, err)

	// Try to acknowledge a break that doesn't exist
	err = mgr.AcknowledgeLeaseBreak(ctx, leaseKey, LeaseStateNone, 0)
	assert.Error(t, err, "should error when no break in progress")
}

func TestAcknowledgeLeaseBreak_AckToNone_RemovesLease(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	key1 := [16]byte{1, 0, 0, 0}
	key2 := [16]byte{2, 0, 0, 0}
	parentKey := [16]byte{}

	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lock *UnifiedLock, breakToState uint32) {
			lock.Lease.Breaking = true
			lock.Lease.BreakToState = breakToState
			lock.Lease.BreakStarted = time.Now()
		},
	})

	// Grant RW lease to key1
	_, _, err := mgr.RequestLease(ctx, FileHandle("file1"), key1, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite, false)
	require.NoError(t, err)

	// Trigger break
	_, _, err = mgr.RequestLease(ctx, FileHandle("file1"), key2, parentKey, "owner2", "client2", "/share", LeaseStateRead, false)
	require.NoError(t, err)

	// Acknowledge to None
	err = mgr.AcknowledgeLeaseBreak(ctx, key1, LeaseStateNone, 0)
	require.NoError(t, err)

	// Lease should be removed
	_, _, found := mgr.GetLeaseState(ctx, key1)
	assert.False(t, found, "lease should be removed after ack to None")
}

// ============================================================================
// ReleaseLease Tests
// ============================================================================

func TestReleaseLease_RemovesLease(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	// Grant a lease
	_, _, err := mgr.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite, false)
	require.NoError(t, err)

	// Verify it exists
	_, _, found := mgr.GetLeaseState(ctx, leaseKey)
	assert.True(t, found)

	// Release
	err = mgr.ReleaseLease(ctx, leaseKey)
	require.NoError(t, err)

	// Verify it's gone
	_, _, found = mgr.GetLeaseState(ctx, leaseKey)
	assert.False(t, found)
}

func TestReleaseLease_NonexistentKey(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{99, 99, 99}

	// Release non-existent lease - should not error
	err := mgr.ReleaseLease(ctx, leaseKey)
	assert.NoError(t, err)
}

// ============================================================================
// GetLeaseState Tests
// ============================================================================

func TestGetLeaseState_Found(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	// Grant a lease
	_, _, err := mgr.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateHandle, false)
	require.NoError(t, err)

	// Get state
	state, epoch, found := mgr.GetLeaseState(ctx, leaseKey)
	assert.True(t, found)
	assert.Equal(t, LeaseStateRead|LeaseStateHandle, state)
	assert.Equal(t, uint16(1), epoch)
}

func TestGetLeaseState_NotFound(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{99, 99, 99}

	state, epoch, found := mgr.GetLeaseState(ctx, leaseKey)
	assert.False(t, found)
	assert.Equal(t, uint32(0), state)
	assert.Equal(t, uint16(0), epoch)
}

// ============================================================================
// ReclaimLease Tests
// ============================================================================

func TestReclaimLease_NotInGracePeriod(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}

	// Not in grace period - should fail
	_, err := mgr.ReclaimLease(ctx, leaseKey, LeaseStateRead, false)
	assert.Error(t, err, "should fail when not in grace period")
}

// ============================================================================
// Epoch Increment Tests
// ============================================================================

func TestEpoch_IncrementOnGrant(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	_, epoch, err := mgr.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	assert.Equal(t, uint16(1), epoch, "new lease starts at epoch 1")
}

func TestEpoch_IncrementOnUpgrade(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	// Grant R (epoch=1)
	_, epoch1, err := mgr.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	assert.Equal(t, uint16(1), epoch1)

	// Upgrade to RW (epoch=2)
	_, epoch2, err := mgr.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite, false)
	require.NoError(t, err)
	assert.Equal(t, uint16(2), epoch2)

	// Upgrade to RWH (epoch=3)
	_, epoch3, err := mgr.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite|LeaseStateHandle, false)
	require.NoError(t, err)
	assert.Equal(t, uint16(3), epoch3)
}

// ============================================================================
// testBreakCallbacks helper
// ============================================================================

type testBreakCallbacks struct {
	onOpLockBreak    func(handleKey string, lock *UnifiedLock, breakToState uint32)
	onByteRangeRev   func(handleKey string, lock *UnifiedLock, reason string)
	onAccessConflict func(handleKey string, existingLock *UnifiedLock, requestedMode AccessMode)
}

func (t *testBreakCallbacks) OnOpLockBreak(handleKey string, lock *UnifiedLock, breakToState uint32) {
	if t.onOpLockBreak != nil {
		t.onOpLockBreak(handleKey, lock, breakToState)
	}
}

func (t *testBreakCallbacks) OnByteRangeRevoke(handleKey string, lock *UnifiedLock, reason string) {
	if t.onByteRangeRev != nil {
		t.onByteRangeRev(handleKey, lock, reason)
	}
}

func (t *testBreakCallbacks) OnAccessConflict(handleKey string, existingLock *UnifiedLock, requestedMode AccessMode) {
	if t.onAccessConflict != nil {
		t.onAccessConflict(handleKey, existingLock, requestedMode)
	}
}
