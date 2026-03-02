package lock

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Delegation CRUD Tests
// ============================================================================

func TestManager_GrantDelegation(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	deleg := NewDelegation(DelegTypeRead, "client1", "/export", false)
	err := lm.GrantDelegation("file1", deleg)
	require.NoError(t, err)

	// Verify delegation is listed
	delegations := lm.ListDelegations("file1")
	require.Len(t, delegations, 1)
	assert.Equal(t, deleg.DelegationID, delegations[0].DelegationID)
}

func TestManager_GrantDelegation_ConflictsWithLease(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	// Add a Write lease first
	err := lm.AddUnifiedLock("file1", &UnifiedLock{
		ID:    "lease1",
		Owner: LockOwner{OwnerID: "smb:client1", ClientID: "conn1"},
		Lease: &OpLock{
			LeaseKey:   [16]byte{1, 2, 3},
			LeaseState: LeaseStateRead | LeaseStateWrite,
		},
	})
	require.NoError(t, err)

	// Read delegation should conflict with Write lease
	deleg := NewDelegation(DelegTypeRead, "client2", "/export", false)
	err = lm.GrantDelegation("file1", deleg)
	assert.Error(t, err, "read delegation should conflict with write lease")
}

func TestManager_GrantDelegation_ReadDelegCoexistsWithReadLease(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	// Add a Read-only lease
	err := lm.AddUnifiedLock("file1", &UnifiedLock{
		ID:    "lease1",
		Owner: LockOwner{OwnerID: "smb:client1", ClientID: "conn1"},
		Lease: &OpLock{
			LeaseKey:   [16]byte{1, 2, 3},
			LeaseState: LeaseStateRead,
		},
	})
	require.NoError(t, err)

	// Read delegation should coexist with Read lease
	deleg := NewDelegation(DelegTypeRead, "client2", "/export", false)
	err = lm.GrantDelegation("file1", deleg)
	assert.NoError(t, err, "read delegation should coexist with read lease")
}

func TestManager_RevokeDelegation(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	deleg := NewDelegation(DelegTypeRead, "client1", "/export", false)
	require.NoError(t, lm.GrantDelegation("file1", deleg))

	err := lm.RevokeDelegation("file1", deleg.DelegationID)
	require.NoError(t, err)

	// Delegation should be gone
	delegations := lm.ListDelegations("file1")
	assert.Len(t, delegations, 0)
}

func TestManager_ReturnDelegation(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	deleg := NewDelegation(DelegTypeRead, "client1", "/export", false)
	require.NoError(t, lm.GrantDelegation("file1", deleg))

	err := lm.ReturnDelegation("file1", deleg.DelegationID)
	require.NoError(t, err)

	// Delegation should be gone
	delegations := lm.ListDelegations("file1")
	assert.Len(t, delegations, 0)
}

func TestManager_ReturnDelegation_Idempotent(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	// Return non-existent delegation should not error
	err := lm.ReturnDelegation("file1", "nonexistent")
	assert.NoError(t, err, "ReturnDelegation should be idempotent")
}

func TestManager_GetDelegation(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	deleg := NewDelegation(DelegTypeWrite, "client1", "/export", false)
	require.NoError(t, lm.GrantDelegation("file1", deleg))

	found := lm.GetDelegation("file1", deleg.DelegationID)
	require.NotNil(t, found)
	assert.Equal(t, DelegTypeWrite, found.DelegType)

	// Non-existent delegation
	notFound := lm.GetDelegation("file1", "nonexistent")
	assert.Nil(t, notFound)
}

func TestManager_ListDelegations_Empty(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	delegations := lm.ListDelegations("nonexistent")
	assert.Nil(t, delegations)
}

// ============================================================================
// CheckAndBreakCaching Tests
// ============================================================================

func TestManager_CheckAndBreakCachingForWrite(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	delegRecalls := &delegationRecallTracker{}
	lm.RegisterBreakCallbacks(delegRecalls)

	// Add a Read delegation
	deleg := NewDelegation(DelegTypeRead, "client1", "/export", false)
	require.NoError(t, lm.GrantDelegation("file1", deleg))

	// Add a Read lease
	require.NoError(t, lm.AddUnifiedLock("file1", &UnifiedLock{
		ID:    "lease1",
		Owner: LockOwner{OwnerID: "smb:client2", ClientID: "conn2"},
		Lease: &OpLock{
			LeaseKey:   [16]byte{1},
			LeaseState: LeaseStateRead,
		},
	}))

	// Write should break both
	err := lm.CheckAndBreakCachingForWrite("file1", nil)
	require.NoError(t, err)

	// Delegation should be marked as breaking
	delegations := lm.ListDelegations("file1")
	require.Len(t, delegations, 1)
	assert.True(t, delegations[0].Breaking, "delegation should be breaking")

	// Callback should have been called
	recalls := delegRecalls.getRecalls()
	assert.Len(t, recalls, 1, "OnDelegationRecall should have been called")
}

func TestManager_CheckAndBreakCachingForRead(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	delegRecalls := &delegationRecallTracker{}
	lm.RegisterBreakCallbacks(delegRecalls)

	// Add a Write delegation
	writeDeleg := NewDelegation(DelegTypeWrite, "client1", "/export", false)
	require.NoError(t, lm.GrantDelegation("file1", writeDeleg))

	// Add a Read delegation
	readDeleg := NewDelegation(DelegTypeRead, "client2", "/export", false)
	require.NoError(t, lm.GrantDelegation("file1", readDeleg))

	// Read should only break Write delegation, not Read
	err := lm.CheckAndBreakCachingForRead("file1", nil)
	require.NoError(t, err)

	recalls := delegRecalls.getRecalls()
	assert.Len(t, recalls, 1, "only write delegation should be recalled for read")

	// Read delegation should NOT be breaking
	delegations := lm.ListDelegations("file1")
	for _, d := range delegations {
		if d.DelegType == DelegTypeRead {
			assert.False(t, d.Breaking, "read delegation should not be breaking for read op")
		}
	}
}

func TestManager_CheckAndBreakCachingForDelete(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	delegRecalls := &delegationRecallTracker{}
	lm.RegisterBreakCallbacks(delegRecalls)

	// Add both read and write delegations
	readDeleg := NewDelegation(DelegTypeRead, "client1", "/export", false)
	require.NoError(t, lm.GrantDelegation("file1", readDeleg))

	writeDeleg := NewDelegation(DelegTypeWrite, "client2", "/export", false)
	require.NoError(t, lm.GrantDelegation("file1", writeDeleg))

	// Delete should break ALL delegations
	err := lm.CheckAndBreakCachingForDelete("file1", nil)
	require.NoError(t, err)

	recalls := delegRecalls.getRecalls()
	assert.Len(t, recalls, 2, "both delegations should be recalled for delete")
}

func TestManager_CheckAndBreakOpLocksForWrite_BackwardCompat(t *testing.T) {
	t.Parallel()

	// Verify old method still works
	lm := NewManager()
	cb := &mockBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	require.NoError(t, lm.AddUnifiedLock("file1", &UnifiedLock{
		ID:    "lease1",
		Owner: LockOwner{OwnerID: "smb:client1"},
		Lease: &OpLock{
			LeaseKey:   [16]byte{1},
			LeaseState: LeaseStateRead | LeaseStateWrite,
		},
	}))

	err := lm.CheckAndBreakOpLocksForWrite("file1", nil)
	require.NoError(t, err)

	breaks := cb.getOpLockBreaks()
	assert.Len(t, breaks, 1, "backward compat: old method should still break leases")
}

// ============================================================================
// WaitForBreakCompletion Tests
// ============================================================================

func TestManager_WaitForBreakCompletion_Resolved(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	deleg := NewDelegation(DelegTypeRead, "client1", "/export", false)
	require.NoError(t, lm.GrantDelegation("file1", deleg))

	// Start breaking
	delegRecalls := &delegationRecallTracker{}
	lm.RegisterBreakCallbacks(delegRecalls)
	require.NoError(t, lm.CheckAndBreakCachingForWrite("file1", nil))

	// Return delegation in background to unblock wait
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = lm.ReturnDelegation("file1", deleg.DelegationID)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := lm.WaitForBreakCompletion(ctx, "file1")
	assert.NoError(t, err, "wait should succeed after delegation returned")
}

func TestManager_WaitForBreakCompletion_Timeout(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	deleg := NewDelegation(DelegTypeRead, "client1", "/export", false)
	require.NoError(t, lm.GrantDelegation("file1", deleg))

	// Start breaking but don't return
	delegRecalls := &delegationRecallTracker{}
	lm.RegisterBreakCallbacks(delegRecalls)
	require.NoError(t, lm.CheckAndBreakCachingForWrite("file1", nil))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := lm.WaitForBreakCompletion(ctx, "file1")
	assert.Error(t, err, "wait should fail on timeout")
}

// ============================================================================
// OnDirChange with Delegations Tests
// ============================================================================

func TestManager_OnDirChange_BreaksDelegations(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	delegRecalls := &delegationRecallTracker{}
	lm.RegisterBreakCallbacks(delegRecalls)

	// Add a directory delegation
	dirDeleg := NewDelegation(DelegTypeRead, "client1", "/export", true)
	require.NoError(t, lm.GrantDelegation("dir1", dirDeleg))

	// Trigger directory change from a different client
	lm.OnDirChange("dir1", DirChangeAddEntry, "client2")

	// Delegation should be recalled
	recalls := delegRecalls.getRecalls()
	assert.Len(t, recalls, 1, "directory delegation should be recalled on dir change")
}

func TestManager_OnDirChange_SkipsOriginClient(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	delegRecalls := &delegationRecallTracker{}
	lm.RegisterBreakCallbacks(delegRecalls)

	// Add a directory delegation
	dirDeleg := NewDelegation(DelegTypeRead, "client1", "/export", true)
	require.NoError(t, lm.GrantDelegation("dir1", dirDeleg))

	// Change from same client should not break delegation
	lm.OnDirChange("dir1", DirChangeAddEntry, "client1")

	recalls := delegRecalls.getRecalls()
	assert.Len(t, recalls, 0, "origin client delegation should not be recalled")
}

// ============================================================================
// RequestLease Delegation Coexistence Tests
// ============================================================================

func TestManager_RequestLease_ChecksDelegationConflict(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	// Grant a Write delegation
	writeDeleg := NewDelegation(DelegTypeWrite, "nfs-client1", "/export", false)
	require.NoError(t, lm.GrantDelegation("file1", writeDeleg))

	// Request a Read lease - should conflict with Write delegation
	ctx := context.Background()
	grantedState, _, err := lm.RequestLease(ctx, "file1",
		[16]byte{1, 2, 3}, [16]byte{}, "smb:client2", "conn2", "/export",
		LeaseStateRead, false)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateNone, grantedState,
		"lease should be denied when conflicting delegation exists")
}

// ============================================================================
// Test Helpers
// ============================================================================

type delegationRecallTracker struct {
	mu      sync.Mutex
	recalls []delegationRecallEvent
}

type delegationRecallEvent struct {
	handleKey    string
	delegationID string
}

func (d *delegationRecallTracker) OnOpLockBreak(handleKey string, lock *UnifiedLock, breakToState uint32) {
	// No-op
}

func (d *delegationRecallTracker) OnByteRangeRevoke(handleKey string, lock *UnifiedLock, reason string) {
	// No-op
}

func (d *delegationRecallTracker) OnAccessConflict(handleKey string, existingLock *UnifiedLock, requestedMode AccessMode) {
	// No-op
}

func (d *delegationRecallTracker) OnDelegationRecall(handleKey string, lock *UnifiedLock) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delegID := ""
	if lock.Delegation != nil {
		delegID = lock.Delegation.DelegationID
	}
	d.recalls = append(d.recalls, delegationRecallEvent{
		handleKey:    handleKey,
		delegationID: delegID,
	})
}

func (d *delegationRecallTracker) getRecalls() []delegationRecallEvent {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := make([]delegationRecallEvent, len(d.recalls))
	copy(result, d.recalls)
	return result
}
