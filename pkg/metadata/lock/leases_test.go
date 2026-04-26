package lock

import (
	"context"
	"sync"
	"sync/atomic"
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

	// RH is now valid for directories (Handle caching lets clients cache
	// directory handles; breaks notify when other clients need access).
	state, epoch, err := mgr.RequestLease(ctx, FileHandle("dir1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateHandle, true)

	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead|LeaseStateHandle, state, "RH should be granted as-is for directories")
	assert.Equal(t, uint16(1), epoch)
}

func TestRequestLease_DirectoryState_RW(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	// Directories do not support Write (W) caching. Requesting RW on a
	// directory should downgrade to R (strip W).
	state, _, err := mgr.RequestLease(ctx, FileHandle("dir1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite, true)

	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead, state, "RW on directory should downgrade to R (W not valid for dirs)")
}

func TestRequestLease_DirectoryState_RWH(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	// Directories do not support Write (W) caching. Requesting RWH on a
	// directory should downgrade to RH (strip W).
	state, _, err := mgr.RequestLease(ctx, FileHandle("dir1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite|LeaseStateHandle, true)

	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead|LeaseStateHandle, state, "RWH on directory should downgrade to RH (W not valid for dirs)")
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

	// Write alone is invalid for files - per MS-SMB2 3.3.5.9.8, must return error
	state, _, err := mgr.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateWrite, false)
	require.ErrorIs(t, err, ErrInvalidLeaseState)
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
	// acknowledges to None asynchronously, eventually removing key1's
	// lease entirely. RequestLease no longer blocks waiting for the ack
	// (see TestRequestLease_CrossKeyConflict_DoesNotBlockOnAck for the
	// rationale), so key2's grant is computed against the BreakToState
	// snapshot (R after stripping W) and key1's removal happens slightly
	// later when the async ack lands.
	state, _, err = mgr.RequestLease(ctx, FileHandle("file1"), key2, parentKey, "owner2", "client2", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead, state, "should grant R lease after break removes existing")
	assert.True(t, breakCalled, "break callback should have been called")

	// After ack-to-None, key1's lease record persists at LeaseState=None
	// (handle-bound lifetime — removed on CLOSE). A duplicate ack on this
	// state-None record must surface ErrLeaseAckNotBreaking →
	// STATUS_UNSUCCESSFUL per smbtorture breaking2/breaking5.
	assert.Eventually(t, func() bool {
		state, _, found := mgr.GetLeaseState(ctx, key1)
		return found && state == LeaseStateNone
	}, 3*time.Second, 10*time.Millisecond, "key1 lease should drop to LeaseState=None after ack")
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
				lock.Lease.BreakingToRequired = LeaseStateRead
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

func TestAcknowledgeLeaseBreak_AckToNone_KeepsRecordAtNone(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	key1 := [16]byte{1, 0, 0, 0}

	// Grant RW lease to key1
	_, _, err := mgr.RequestLease(ctx, FileHandle("file1"), key1, [16]byte{}, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite, false)
	require.NoError(t, err)

	// Manually set the lease to breaking state (simulating a break to None).
	// This avoids triggering RequestLease which waits for break completion.
	mgr.mu.Lock()
	for _, locks := range mgr.unifiedLocks {
		for _, lock := range locks {
			if lock.Lease != nil && lock.Lease.LeaseKey == key1 {
				lock.Lease.Breaking = true
				lock.Lease.BreakToState = LeaseStateNone
				lock.Lease.BreakingToRequired = LeaseStateNone
				lock.Lease.BreakStarted = time.Now()
			}
		}
	}
	mgr.mu.Unlock()

	// Acknowledge to None (fully release)
	err = mgr.AcknowledgeLeaseBreak(ctx, key1, LeaseStateNone, 0)
	require.NoError(t, err)

	// Per MS-SMB2 3.3.5.22.2 + smbtorture breaking2/breaking5: the lease
	// record persists at LeaseState=None until CLOSE so a duplicate ack
	// can be distinguished from CLOSE-beat-ack.
	state, _, found := mgr.GetLeaseState(ctx, key1)
	assert.True(t, found, "lease record should persist after ack-to-None")
	assert.Equal(t, LeaseStateNone, state, "lease state should be None")

	// Duplicate ack on the released record → ErrLeaseAckNotBreaking.
	err = mgr.AcknowledgeLeaseBreak(ctx, key1, LeaseStateNone, 0)
	assert.ErrorIs(t, err, ErrLeaseAckNotBreaking, "duplicate ack must surface ErrLeaseAckNotBreaking")

	// CLOSE removes the record fully.
	err = mgr.ReleaseLeaseForHandle(ctx, "file1", key1)
	require.NoError(t, err)
	_, _, found = mgr.GetLeaseState(ctx, key1)
	assert.False(t, found, "lease should be gone after CLOSE")
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

// TestReleaseLeaseForHandle_ScopedToSingleBucket covers the fix in 249fd668:
// smbtorture reuses fixed LEASE1/LEASE2 keys across tests, so the same
// LeaseKey can live under two distinct handleKey buckets at the same time.
// Releasing one bucket must not erase the other — otherwise stale records
// accumulate on the surviving file and break ACK lookup.
func TestReleaseLeaseForHandle_ScopedToSingleBucket(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}

	_, _, err := mgr.RequestLease(ctx, FileHandle("/share:fileA"), leaseKey, [16]byte{}, "ownerA", "client", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	_, _, err = mgr.RequestLease(ctx, FileHandle("/share:fileB"), leaseKey, [16]byte{}, "ownerB", "client", "/share", LeaseStateRead, false)
	require.NoError(t, err)

	// Release only fileA's bucket.
	require.NoError(t, mgr.ReleaseLeaseForHandle(ctx, "/share:fileA", leaseKey))

	// fileA's bucket should be gone; fileB's lease record must survive.
	mgr.mu.RLock()
	_, aStillThere := mgr.unifiedLocks["/share:fileA"]
	bBucket := mgr.unifiedLocks["/share:fileB"]
	mgr.mu.RUnlock()
	assert.False(t, aStillThere, "fileA bucket should be removed when emptied")
	require.Len(t, bBucket, 1, "fileB bucket must survive intact")
	assert.Equal(t, leaseKey, bBucket[0].Lease.LeaseKey)
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

// epochForLease returns the current Epoch of the lease with the given key.
// Helper for the epoch-accounting tests below.
func epochForLease(t *testing.T, mgr *Manager, key [16]byte) uint16 {
	t.Helper()
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	_, lock, _ := mgr.findLeaseByKey(key)
	if lock == nil || lock.Lease == nil {
		t.Fatalf("no lease found for key %x", key)
	}
	return lock.Lease.Epoch
}

// setBreaking drives the lease into the Breaking state without going through
// RequestLease (which would block on the break). Mirrors
// TestAcknowledgeLeaseBreak_ToReadState's setup and bumps the epoch exactly
// like RequestLease does at the real break-initiation site.
func setBreaking(t *testing.T, mgr *Manager, key [16]byte, breakTo uint32) {
	t.Helper()
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	for _, locks := range mgr.unifiedLocks {
		for _, lock := range locks {
			if lock.Lease != nil && lock.Lease.LeaseKey == key {
				lock.Lease.Breaking = true
				lock.Lease.BreakToState = breakTo
				lock.Lease.BreakingToRequired = breakTo
				lock.Lease.BreakStarted = time.Now()
				advanceEpoch(lock.Lease) // matches RequestLease at leases.go:256
				return
			}
		}
	}
	t.Fatalf("no lease with key %x to mark breaking", key)
}

// TestEpoch_BreakPlusAck_SingleIncrement verifies MS-SMB2 §3.3.4.7: a break
// (notification + subsequent ACK) advances Epoch exactly once — not twice.
// The break-initiation increment is the state change announced on the wire;
// the ACK confirms that change and must not add a second increment. See #417.
func TestEpoch_BreakPlusAck_SingleIncrement(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	key := [16]byte{1, 0, 0, 0}

	// Grant RWH. Epoch = 1.
	_, epochGrant, err := mgr.RequestLease(ctx, FileHandle("file1"), key, [16]byte{}, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite|LeaseStateHandle, false)
	require.NoError(t, err)
	require.Equal(t, uint16(1), epochGrant)

	// Break initiated (RWH → RH). Epoch must advance to 2 — this is what the
	// notification carries as NewEpoch.
	setBreaking(t, mgr, key, LeaseStateRead|LeaseStateHandle)
	require.Equal(t, uint16(2), epochForLease(t, mgr, key),
		"break initiation must advance epoch to grant + 1")

	// ACK. Epoch must stay at 2: the state change was already counted.
	require.NoError(t, mgr.AcknowledgeLeaseBreak(ctx, key, LeaseStateRead|LeaseStateHandle, 2))
	assert.Equal(t, uint16(2), epochForLease(t, mgr, key),
		"ACK must not advance epoch — would drift one past the client (#417)")
}

// TestEpoch_TwoBreakCycles_TwoIncrements verifies the per-break accounting
// across consecutive break/ACK pairs. After grant + two breaks, Epoch must be
// grant + 2 (not grant + 4, which is the pre-fix double-increment behavior).
func TestEpoch_TwoBreakCycles_TwoIncrements(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	key := [16]byte{2, 0, 0, 0}

	_, epochGrant, err := mgr.RequestLease(ctx, FileHandle("file2"), key, [16]byte{}, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite|LeaseStateHandle, false)
	require.NoError(t, err)
	require.Equal(t, uint16(1), epochGrant)

	// Cycle 1: break RWH → RH, ACK.
	setBreaking(t, mgr, key, LeaseStateRead|LeaseStateHandle)
	require.NoError(t, mgr.AcknowledgeLeaseBreak(ctx, key, LeaseStateRead|LeaseStateHandle, 2))
	require.Equal(t, uint16(2), epochForLease(t, mgr, key))

	// Cycle 2: break RH → R, ACK. One more increment expected.
	setBreaking(t, mgr, key, LeaseStateRead)
	require.NoError(t, mgr.AcknowledgeLeaseBreak(ctx, key, LeaseStateRead, 3))
	assert.Equal(t, uint16(3), epochForLease(t, mgr, key),
		"two break/ack cycles must yield exactly two increments total")
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
	// For directory: RWH (invalid), RH (valid, strip W), RW (invalid, strip H), R (valid, strip both)
	assert.Equal(t, []uint32{
		LeaseStateRead | LeaseStateHandle,
		LeaseStateRead,
	}, candidates)
}

func TestDowngradeCandidates_DirectoryRH(t *testing.T) {
	t.Parallel()

	candidates := downgradeCandidates(LeaseStateRead|LeaseStateHandle, true)
	// RH (valid for dir), strip W = RH (dedup), strip H = R (valid), strip both = R (dedup)
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

	// Directory candidates for RWH: [RH, R] (W invalid for dirs, so RWH and RW skipped)
	// RH: existing W conflicts with requested R -> skip
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

// TestRequestLease_CrossKeyConflict_DoesNotBlockOnAck verifies that the
// second opener's RequestLease does NOT block waiting for the first client's
// LEASE_BREAK_ACK. This is the core invariant behind the WPTS
// BVT_DirectoryLeasing_LeaseBreakOnMultiClients scenario: the test
// orchestrates Client1's ack only AFTER Client2's CREATE returns. If
// RequestLease blocks Client2 waiting for an ack that the test will only
// drive after Client2 returns, the call deadlocks until the WPTS client-side
// ~8s timeout fires (System.TimeoutException).
//
// The test uses a file lease (RW) because Write caching is not valid for
// directories after the lease constant swap. The cross-key non-blocking
// guarantee applies to both file and directory leases.
//
// The internal break dispatch is synchronous (the LEASE_BREAK_NOTIFICATION
// is on the wire before this call returns), and OpLocksConflict already
// treats a Breaking lease as having its BreakToState (oplock.go:229-233),
// so bestGrantableState computes the correct downgraded grant without
// needing to wait for the ack. See also internal/adapter/smb/lease/manager.go
// BreakHandleLeasesOnOpenAsync, which documents the same deadlock pattern
// for directory opens: "blocking would deadlock: the other client needs
// this CREATE's response before it processes the break."
func TestRequestLease_CrossKeyConflict_DoesNotBlockOnAck(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	key1 := [16]byte{1, 0, 0, 0}
	key2 := [16]byte{2, 0, 0, 0}
	parentKey := [16]byte{}

	// Register a break callback that records the break but NEVER acks.
	// This simulates a slow/non-cooperating client (or, in the WPTS test
	// case, a client that the test harness has not yet driven to ack).
	var breakCalled atomic.Bool
	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lock *UnifiedLock, breakToState uint32) {
			breakCalled.Store(true)
			// Intentionally do NOT call AcknowledgeLeaseBreak.
		},
	})

	// Client1 takes RW file lease. We use a file (not directory) because
	// RW is no longer valid for directories after the lease constant swap.
	// The test's purpose is verifying the cross-key path doesn't deadlock.
	state, _, err := mgr.RequestLease(ctx, FileHandle("file1"), key1, parentKey,
		"owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite, false)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead|LeaseStateWrite, state)

	// Client2 requests RW on the same file with a different key.
	// This must trigger a cross-key conflict, dispatch a break (which is
	// never acked), and then return promptly with a downgraded grant.
	//
	// The test asserts the call returns within 1s. Without the fix, the
	// 35s WaitForBreakCompletion in leases.go blocks here for the full
	// timeout, exceeding the 1s budget.
	type result struct {
		state uint32
		err   error
	}
	done := make(chan result, 1)
	go func() {
		s, _, e := mgr.RequestLease(ctx, FileHandle("file1"), key2, parentKey,
			"owner2", "client2", "/share", LeaseStateRead|LeaseStateWrite, false)
		done <- result{s, e}
	}()

	select {
	case r := <-done:
		require.NoError(t, r.err)
		// After break-to-R, Client2 should get R (RW conflict resolved).
		assert.Equal(t, LeaseStateRead, r.state,
			"Client2 should get R after Client1's RW is broken-to-R")
		assert.True(t, breakCalled.Load(), "break callback must have fired")
	case <-time.After(3 * time.Second):
		t.Fatalf("RequestLease blocked >3s waiting for ack that never comes — "+
			"this is the WPTS BVT_DirectoryLeasing_LeaseBreakOnMultiClients "+
			"deadlock. breakCalled=%v", breakCalled.Load())
	}
}

// ============================================================================
// Progressive multi-stage lease-break tests (issue #449)
// ============================================================================
//
// These exercise the smbtorture smb2.lease.breaking3 / v2_breaking3 wire
// shape: when a fresh break is in flight and a stricter conflicting open
// arrives, the cumulative final target (BreakingToRequired) is AND-merged
// without dispatching a new notification; on each subsequent ACK the
// next progressive stage is dispatched (RH→R then R→"") until LeaseState
// reaches BreakingToRequired.

// recordBreakNotifications collects break-to states in order, returning a
// callback registration suitable for testBreakCallbacks.
func recordBreakNotifications() (cb *testBreakCallbacks, breaks *[]uint32, mu *sync.Mutex) {
	var muLocal sync.Mutex
	var seen []uint32
	cb = &testBreakCallbacks{
		onOpLockBreak: func(_ string, _ *UnifiedLock, breakToState uint32) {
			muLocal.Lock()
			seen = append(seen, breakToState)
			muLocal.Unlock()
		},
	}
	return cb, &seen, &muLocal
}

func snapshotBreaks(mu *sync.Mutex, breaks *[]uint32) []uint32 {
	mu.Lock()
	defer mu.Unlock()
	out := make([]uint32, len(*breaks))
	copy(out, *breaks)
	return out
}

// TestProgressiveLeaseBreak_RWH_AndMerge_ToNone exercises the breaking3 wire
// shape end-to-end: RWH lease, default opener (strip W), then a destructive
// opener AND-merges the cumulative target down to None. Each ACK drives the
// next progressive stage; req2/req3-equivalent waiters are tracked via
// signalBreakWait and only released when LeaseState reaches BreakingToRequired.
func TestProgressiveLeaseBreak_RWH_AndMerge_ToNone(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	cb, breaks, breakMu := recordBreakNotifications()
	mgr.RegisterBreakCallbacks(cb)

	ctx := context.Background()
	key1 := [16]byte{0xA1}
	handleKey := "file-449"

	// Grant RWH directly to seed the test (skip RequestLease's grant path
	// which itself doesn't trigger breaks on first grant anyway).
	_, _, err := mgr.RequestLease(ctx, FileHandle(handleKey), key1, [16]byte{},
		"owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite|LeaseStateHandle, false)
	require.NoError(t, err)

	// Stage 1: a default opener arrives (strip W). RWH→RH dispatched.
	require.NoError(t, mgr.BreakLeasesOnOpenConflict(handleKey, &LockOwner{
		OwnerID: "owner-default", ClientID: "client-default",
	}, BreakReasonDefault))

	got := snapshotBreaks(breakMu, breaks)
	require.Len(t, got, 1, "stage 1: exactly one notification dispatched")
	assert.Equal(t, LeaseStateRead|LeaseStateHandle, got[0],
		"stage 1: RWH→RH (strip W)")

	// Lease should now be Breaking with BreakToState=RH and BreakingToRequired=RH.
	mgr.mu.Lock()
	_, lock, _ := mgr.findLeaseByKey(key1)
	require.NotNil(t, lock)
	assert.True(t, lock.Lease.Breaking)
	assert.Equal(t, LeaseStateRead|LeaseStateHandle, lock.Lease.BreakToState)
	assert.Equal(t, LeaseStateRead|LeaseStateHandle, lock.Lease.BreakingToRequired)
	mgr.mu.Unlock()

	// Stage 2: a destructive opener arrives mid-stage → AND-merge.
	// BreakingToRequired = RH & 0 = 0. No new notification.
	require.NoError(t, mgr.BreakLeasesOnOpenConflict(handleKey, &LockOwner{
		OwnerID: "owner-destr", ClientID: "client-destr",
	}, BreakReasonDestructive))

	got = snapshotBreaks(breakMu, breaks)
	require.Len(t, got, 1, "stage 2: AND-merge must NOT dispatch a new notification")

	mgr.mu.Lock()
	_, lock, _ = mgr.findLeaseByKey(key1)
	require.NotNil(t, lock)
	assert.Equal(t, uint32(0), lock.Lease.BreakingToRequired,
		"AND-merge must tighten BreakingToRequired to 0 (None)")
	assert.Equal(t, LeaseStateRead|LeaseStateHandle, lock.Lease.BreakToState,
		"BreakToState (in-flight) is unchanged by AND-merge")
	mgr.mu.Unlock()

	// Stage 3: client ACKs RWH→RH. Re-eval finds acked state has H bit ⇒
	// next target = required(0) | R = R. Dispatch RH→R.
	require.NoError(t, mgr.AcknowledgeLeaseBreak(ctx,
		key1, LeaseStateRead|LeaseStateHandle, 0))

	got = snapshotBreaks(breakMu, breaks)
	require.Len(t, got, 2, "stage 3: ACK RWH→RH triggers RH→R notification")
	assert.Equal(t, LeaseStateRead, got[1], "stage 3: target = R")

	mgr.mu.Lock()
	_, lock, _ = mgr.findLeaseByKey(key1)
	require.NotNil(t, lock)
	assert.True(t, lock.Lease.Breaking,
		"lease still Breaking after partial ACK with stricter required")
	assert.Equal(t, LeaseStateRead, lock.Lease.BreakToState)
	assert.Equal(t, uint32(0), lock.Lease.BreakingToRequired)
	mgr.mu.Unlock()

	// Stage 4: client ACKs RH→R. Re-eval finds acked state has neither W nor
	// H ⇒ next target = required(0) = 0. Dispatch R→"" (fire-and-forget,
	// inline downgrade). Record persists at LeaseState=None (handle-bound
	// lifetime — only CLOSE removes it).
	require.NoError(t, mgr.AcknowledgeLeaseBreak(ctx, key1, LeaseStateRead, 0))

	got = snapshotBreaks(breakMu, breaks)
	require.Len(t, got, 3, "stage 4: ACK RH→R triggers R→\"\" notification")
	assert.Equal(t, LeaseStateNone, got[2], "stage 4: target = None")

	state, _, found := mgr.GetLeaseState(ctx, key1)
	assert.True(t, found, "stage 4: record persists after R→None auto-downgrade")
	assert.Equal(t, LeaseStateNone, state, "stage 4: state drained to None")
}

// TestProgressiveLeaseBreak_NoSpuriousAfterReachingRequired confirms that a
// single-shot break (no concurrent AND-merge) does NOT trigger a second
// progressive stage when the client ACKs to the offered state. This is the
// breaking4-style invariant — fresh dispatch stays single-shot.
func TestProgressiveLeaseBreak_NoSpuriousAfterReachingRequired(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	cb, breaks, breakMu := recordBreakNotifications()
	mgr.RegisterBreakCallbacks(cb)

	ctx := context.Background()
	key1 := [16]byte{0xB1}
	handleKey := "file-449-noprog"

	_, _, err := mgr.RequestLease(ctx, FileHandle(handleKey), key1, [16]byte{},
		"owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite|LeaseStateHandle, false)
	require.NoError(t, err)

	require.NoError(t, mgr.BreakLeasesOnOpenConflict(handleKey, &LockOwner{
		OwnerID: "owner-default", ClientID: "client-default",
	}, BreakReasonDefault))
	require.Len(t, snapshotBreaks(breakMu, breaks), 1)

	// Client ACKs to the offered state. No concurrent break has tightened
	// BreakingToRequired ⇒ no second stage dispatched.
	require.NoError(t, mgr.AcknowledgeLeaseBreak(ctx,
		key1, LeaseStateRead|LeaseStateHandle, 0))

	assert.Len(t, snapshotBreaks(breakMu, breaks), 1,
		"single-shot break must not trigger a second progressive stage")

	mgr.mu.Lock()
	_, lock, _ := mgr.findLeaseByKey(key1)
	require.NotNil(t, lock)
	assert.False(t, lock.Lease.Breaking, "Breaking cleared after final ACK")
	assert.Equal(t, LeaseStateRead|LeaseStateHandle, lock.Lease.LeaseState)
	assert.Equal(t, LeaseStateRead|LeaseStateHandle, lock.Lease.BreakingToRequired,
		"BreakingToRequired equals LeaseState when not Breaking (invariant)")
	mgr.mu.Unlock()
}

// TestForceCompleteBreaks_DrainsToBreakingToRequired confirms that a non-acking
// client triggers the timeout path which drains to the cumulative final
// target, not the in-flight intermediate. Otherwise a non-ack would leave
// the lease parked at e.g. RH when a later destructive opener had AND-merged
// the target down to None.
func TestForceCompleteBreaks_DrainsToBreakingToRequired(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	cb, _, _ := recordBreakNotifications()
	mgr.RegisterBreakCallbacks(cb)

	ctx := context.Background()
	key1 := [16]byte{0xC1}
	handleKey := "file-449-force"

	_, _, err := mgr.RequestLease(ctx, FileHandle(handleKey), key1, [16]byte{},
		"owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite|LeaseStateHandle, false)
	require.NoError(t, err)

	// First break: RWH→RH (BreakingToRequired=RH).
	require.NoError(t, mgr.BreakLeasesOnOpenConflict(handleKey, &LockOwner{
		OwnerID: "owner-default", ClientID: "client-default",
	}, BreakReasonDefault))

	// AND-merge tighter target: BreakingToRequired=0.
	require.NoError(t, mgr.BreakLeasesOnOpenConflict(handleKey, &LockOwner{
		OwnerID: "owner-destr", ClientID: "client-destr",
	}, BreakReasonDestructive))

	// Force-complete should drain to BreakingToRequired (= 0), keeping the
	// record at LeaseState=None (handle-bound lifetime). Pre-fix this drained
	// to BreakToState (= RH), leaving the lease at RH which would block the
	// destructive opener.
	mgr.forceCompleteBreaks(handleKey)

	state, _, found := mgr.GetLeaseState(ctx, key1)
	require.True(t, found, "force-complete keeps the record alive until CLOSE")
	assert.Equal(t, LeaseStateNone, state,
		"force-complete must drain to None (BreakingToRequired), not RH (BreakToState)")
}

// TestAnyHolderHasLeaseBits covers the cross-key per-bit query that gates the
// SMB CREATE post-break park decision (#449). Mirrors Samba `delay_for_oplock_fn`:
//   - sharing violation              → mask = HANDLE (park if any holder has H)
//   - non-violation/default/destruct → mask = WRITE  (park if any holder has W)
func TestAnyHolderHasLeaseBits(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	keyRH := [16]byte{0xD1}
	keyRW := [16]byte{0xD2}
	keyR := [16]byte{0xD3}

	// Empty: no holders.
	assert.False(t, mgr.AnyHolderHasLeaseBits("absent", [16]byte{}, LeaseStateWrite))

	// Grant RH (no W). W-mask must report false; H-mask must report true.
	_, _, err := mgr.RequestLease(ctx, FileHandle("h-rh"), keyRH, [16]byte{},
		"o1", "c1", "/share", LeaseStateRead|LeaseStateHandle, false)
	require.NoError(t, err)
	assert.False(t, mgr.AnyHolderHasLeaseBits("h-rh", [16]byte{}, LeaseStateWrite),
		"RH lease has no W ⇒ W-mask reports false (breaking4: no flush, no park)")
	assert.True(t, mgr.AnyHolderHasLeaseBits("h-rh", [16]byte{}, LeaseStateHandle),
		"RH lease has H ⇒ H-mask reports true (sharing-violation: must park)")

	// Grant RW. W-mask reports true.
	_, _, err = mgr.RequestLease(ctx, FileHandle("h-rw"), keyRW, [16]byte{},
		"o2", "c2", "/share", LeaseStateRead|LeaseStateWrite, false)
	require.NoError(t, err)
	assert.True(t, mgr.AnyHolderHasLeaseBits("h-rw", [16]byte{}, LeaseStateWrite),
		"RW lease has W ⇒ W-mask reports true")

	// Exclusion: same key excluded ⇒ false.
	assert.False(t, mgr.AnyHolderHasLeaseBits("h-rw", keyRW, LeaseStateWrite),
		"excluding the only W holder must report false")

	// Empty mask is a no-op short-circuit.
	assert.False(t, mgr.AnyHolderHasLeaseBits("h-rw", [16]byte{}, 0))

	// Multiple R-only holders: neither W nor H mask matches.
	_, _, err = mgr.RequestLease(ctx, FileHandle("h-mixed"), keyR, [16]byte{},
		"o3", "c3", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	keyMix := [16]byte{0xD4}
	_, _, err = mgr.RequestLease(ctx, FileHandle("h-mixed"), keyMix, [16]byte{},
		"o4", "c4", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	assert.False(t, mgr.AnyHolderHasLeaseBits("h-mixed", [16]byte{}, LeaseStateWrite))
	assert.False(t, mgr.AnyHolderHasLeaseBits("h-mixed", [16]byte{}, LeaseStateHandle))
}
