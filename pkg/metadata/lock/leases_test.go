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

func TestRequestLease_DirectoryState_RW(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	// Per MS-SMB2 3.3.5.9: directories CAN hold Write (W) caching leases.
	// Write caching on a directory means the client can cache directory
	// content changes. Both Windows Server and Samba grant RW on directories.
	state, _, err := mgr.RequestLease(ctx, FileHandle("dir1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite, true)

	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead|LeaseStateWrite, state, "should grant RW as-is for directory")
}

func TestRequestLease_DirectoryState_RWH(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	// Per MS-SMB2 3.3.5.9: directories CAN hold Write (W) caching leases.
	// RWH is granted as-is for directories.
	state, _, err := mgr.RequestLease(ctx, FileHandle("dir1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite|LeaseStateHandle, true)

	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead|LeaseStateWrite|LeaseStateHandle, state, "should grant RWH as-is for directory")
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

	// Register break callback that acknowledges the break immediately.
	// In real SMB, the client would receive the break notification and ack it.
	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lock *UnifiedLock, breakToState uint32) {
			// Snapshot values before goroutine to avoid data race on Epoch.
			key := lock.Lease.LeaseKey
			epoch := lock.Lease.Epoch
			// Simulate client acknowledging break to R (strip W)
			go func() {
				_ = mgr.AcknowledgeLeaseBreak(ctx, key, breakToState, epoch)
			}()
		},
	})

	// First client gets RW lease
	state, _, err := mgr.RequestLease(ctx, FileHandle("file1"), key1, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite, false)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead|LeaseStateWrite, state)

	// Second client requests R lease on same file - triggers break on key1's Write.
	// Per MS-SMB2 3.3.5.9: after the break completes, the server re-evaluates
	// the lease request. Since key1 now has R (Write stripped), key2's R lease
	// should be granted (Read leases can coexist).
	state, _, err = mgr.RequestLease(ctx, FileHandle("file1"), key2, parentKey, "owner2", "client2", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead, state, "should grant R lease after break reduces existing to R")
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

	// Setup: register a break callback that tracks breaks and acknowledges them.
	var breakCalled bool
	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lock *UnifiedLock, breakToState uint32) {
			breakCalled = true
			// Snapshot values before goroutine to avoid data race on Epoch.
			key := lock.Lease.LeaseKey
			epoch := lock.Lease.Epoch
			// Acknowledge break to None (fully relinquish) asynchronously
			go func() {
				_ = mgr.AcknowledgeLeaseBreak(ctx, key, LeaseStateNone, epoch)
			}()
		},
	})

	// Grant RW lease to key1
	state, _, err := mgr.RequestLease(ctx, FileHandle("file1"), key1, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite, false)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead|LeaseStateWrite, state)

	// Request from key2 triggers break on key1. The break callback
	// acknowledges to None, removing key1's lease entirely. After the
	// break completes, key2's R lease should be granted.
	state, _, err = mgr.RequestLease(ctx, FileHandle("file1"), key2, parentKey, "owner2", "client2", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead, state, "should grant R lease after break removes existing")
	assert.True(t, breakCalled, "break callback should have been called")

	// key1's lease should be removed (acknowledged to None)
	_, _, found := mgr.GetLeaseState(ctx, key1)
	assert.False(t, found, "key1 lease should be removed after ack to None")
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

	// Grant RW lease to key1
	_, _, err := mgr.RequestLease(ctx, FileHandle("file1"), key1, [16]byte{}, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite, false)
	require.NoError(t, err)

	// Manually set the lease to breaking state (simulating a break to R).
	// This avoids triggering RequestLease which waits for break completion.
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

	// Acknowledge to None (fully release)
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

func (t *testBreakCallbacks) OnDelegationRecall(handleKey string, lock *UnifiedLock) {
	// No-op for existing lease tests
}

// ============================================================================
// downgradeCandidates Tests
// ============================================================================

func TestDowngradeCandidates_FileRWH(t *testing.T) {
	t.Parallel()

	candidates := downgradeCandidates(LeaseStateRead|LeaseStateWrite|LeaseStateHandle, false)
	// RWH -> try RWH, then RH (strip W), then RW (strip H), then R (strip both), then R (fallback)
	// Deduped: RWH, RH, RW, R
	assert.Equal(t, []uint32{
		LeaseStateRead | LeaseStateWrite | LeaseStateHandle,
		LeaseStateRead | LeaseStateHandle,
		LeaseStateRead | LeaseStateWrite,
		LeaseStateRead,
	}, candidates)
}

func TestDowngradeCandidates_FileRW(t *testing.T) {
	t.Parallel()

	candidates := downgradeCandidates(LeaseStateRead|LeaseStateWrite, false)
	// RW -> try RW, then R (strip W), then RW (strip H = no-op), then R (strip both)
	// Deduped: RW, R
	assert.Equal(t, []uint32{
		LeaseStateRead | LeaseStateWrite,
		LeaseStateRead,
	}, candidates)
}

func TestDowngradeCandidates_FileR(t *testing.T) {
	t.Parallel()

	candidates := downgradeCandidates(LeaseStateRead, false)
	// R -> only R
	assert.Equal(t, []uint32{
		LeaseStateRead,
	}, candidates)
}

func TestDowngradeCandidates_DirectoryRWH(t *testing.T) {
	t.Parallel()

	candidates := downgradeCandidates(LeaseStateRead|LeaseStateWrite|LeaseStateHandle, true)
	// For directory: RWH, RH (strip W), RW (strip H), R (strip both), R (fallback)
	// All are valid directory states. Deduped: RWH, RH, RW, R
	assert.Equal(t, []uint32{
		LeaseStateRead | LeaseStateWrite | LeaseStateHandle,
		LeaseStateRead | LeaseStateHandle,
		LeaseStateRead | LeaseStateWrite,
		LeaseStateRead,
	}, candidates)
}

func TestDowngradeCandidates_DirectoryRH(t *testing.T) {
	t.Parallel()

	candidates := downgradeCandidates(LeaseStateRead|LeaseStateHandle, true)
	// RH -> try RH, then RH (strip W = no-op), then R (strip H), then R (strip both)
	// Deduped: RH, R
	assert.Equal(t, []uint32{
		LeaseStateRead | LeaseStateHandle,
		LeaseStateRead,
	}, candidates)
}

// ============================================================================
// bestGrantableState Tests
// ============================================================================

func TestBestGrantableState_NoConflicts(t *testing.T) {
	t.Parallel()

	// Empty lock set - full request granted
	key := [16]byte{1}
	state := bestGrantableState(nil, key, LeaseStateRead|LeaseStateWrite|LeaseStateHandle, false)
	assert.Equal(t, LeaseStateRead|LeaseStateWrite|LeaseStateHandle, state)
}

func TestBestGrantableState_FileRWH_DowngradesWithExistingR(t *testing.T) {
	t.Parallel()

	// Existing Read lease from different key
	otherKey := [16]byte{2}
	requestKey := [16]byte{1}
	locks := []*UnifiedLock{
		{Lease: &OpLock{LeaseKey: otherKey, LeaseState: LeaseStateRead}},
	}

	// RWH: Write conflicts with existing Read -> skip
	// RH: Handle doesn't conflict with Read -> grant RH
	state := bestGrantableState(locks, requestKey, LeaseStateRead|LeaseStateWrite|LeaseStateHandle, false)
	assert.Equal(t, LeaseStateRead|LeaseStateHandle, state)
}

func TestBestGrantableState_FileRWH_DowngradesToRH(t *testing.T) {
	t.Parallel()

	// Existing RH lease from different key
	otherKey := [16]byte{2}
	requestKey := [16]byte{1}
	locks := []*UnifiedLock{
		{Lease: &OpLock{LeaseKey: otherKey, LeaseState: LeaseStateRead | LeaseStateHandle}},
	}

	// RWH: Write conflicts with existing Read -> skip
	// RH: no conflict -> grant RH
	state := bestGrantableState(locks, requestKey, LeaseStateRead|LeaseStateWrite|LeaseStateHandle, false)
	assert.Equal(t, LeaseStateRead|LeaseStateHandle, state)
}

func TestBestGrantableState_SameKeyIgnored(t *testing.T) {
	t.Parallel()

	// Existing lease from same key should be ignored (not a conflict)
	sameKey := [16]byte{1}
	locks := []*UnifiedLock{
		{Lease: &OpLock{LeaseKey: sameKey, LeaseState: LeaseStateRead | LeaseStateWrite}},
	}

	state := bestGrantableState(locks, sameKey, LeaseStateRead|LeaseStateWrite|LeaseStateHandle, false)
	assert.Equal(t, LeaseStateRead|LeaseStateWrite|LeaseStateHandle, state)
}

func TestBestGrantableState_DirectoryRWH_DowngradeCascade(t *testing.T) {
	t.Parallel()

	// Existing RWH directory lease from different key
	otherKey := [16]byte{2}
	requestKey := [16]byte{1}
	locks := []*UnifiedLock{
		{Lease: &OpLock{LeaseKey: otherKey, LeaseState: LeaseStateRead | LeaseStateWrite | LeaseStateHandle}},
	}

	// RWH: requested W conflicts with existing R/W -> skip
	// RH: requested R conflicts with existing W (existing W conflicts with requested R) -> skip
	// RW: requested W conflicts with existing R/W -> skip
	// R: existing W conflicts with requested R -> skip
	// All candidates conflict -> None
	state := bestGrantableState(locks, requestKey, LeaseStateRead|LeaseStateWrite|LeaseStateHandle, true)
	assert.Equal(t, LeaseStateNone, state)
}

func TestBestGrantableState_AllConflict_ReturnsNone(t *testing.T) {
	t.Parallel()

	// Existing RW lease from other key: existing W conflicts with any requested R or W.
	// All downgrade candidates include R, so all conflict -> None.
	otherKey := [16]byte{2}
	requestKey := [16]byte{1}
	locks := []*UnifiedLock{
		{Lease: &OpLock{LeaseKey: otherKey, LeaseState: LeaseStateRead | LeaseStateWrite}},
	}

	// RW: W conflicts with existing R/W -> skip
	// R: existing W conflicts with requested R -> skip
	// All candidates conflict -> None
	state := bestGrantableState(locks, requestKey, LeaseStateRead|LeaseStateWrite, false)
	assert.Equal(t, LeaseStateNone, state)
}

// ============================================================================
// Same-Key Breaking Lease Tests
// ============================================================================

func TestRequestLease_SameKeyBreaking_ReturnsBreakInProgress(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	key1 := [16]byte{1, 0, 0, 0}
	parentKey := [16]byte{}

	// Grant RWH lease
	_, _, err := mgr.RequestLease(ctx, FileHandle("file1"), key1, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite|LeaseStateHandle, false)
	require.NoError(t, err)

	// Manually set the lease to breaking state
	mgr.mu.Lock()
	for _, locks := range mgr.unifiedLocks {
		for _, lock := range locks {
			if lock.Lease != nil && lock.Lease.LeaseKey == key1 {
				lock.Lease.Breaking = true
				lock.Lease.BreakToState = LeaseStateRead | LeaseStateHandle
				lock.Lease.BreakStarted = time.Now()
				advanceEpoch(lock.Lease) // epoch becomes 2
			}
		}
	}
	mgr.mu.Unlock()

	// Request with same key while breaking
	state, epoch, err := mgr.RequestLease(ctx, FileHandle("file1"), key1, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite|LeaseStateHandle, false)

	// Should return current state with ErrLeaseBreakInProgress
	assert.ErrorIs(t, err, ErrLeaseBreakInProgress)
	assert.Equal(t, LeaseStateRead|LeaseStateWrite|LeaseStateHandle, state, "should return current lease state")
	assert.Equal(t, uint16(2), epoch, "should return current epoch")
}
