package lock

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// OpLock V2 Fields Tests
// ============================================================================

func TestOpLock_ParentLeaseKey_DefaultZero(t *testing.T) {
	t.Parallel()

	lease := &OpLock{
		LeaseKey:   [16]byte{1, 2, 3},
		LeaseState: LeaseStateRead,
	}

	// ParentLeaseKey should default to zero value
	assert.Equal(t, [16]byte{}, lease.ParentLeaseKey)
}

func TestOpLock_IsDirectory_DefaultFalse(t *testing.T) {
	t.Parallel()

	lease := &OpLock{
		LeaseKey:   [16]byte{1, 2, 3},
		LeaseState: LeaseStateRead,
	}

	// IsDirectory should default to false
	assert.False(t, lease.IsDirectory)
}

func TestOpLock_Clone_CopiesV2Fields(t *testing.T) {
	t.Parallel()

	parentKey := [16]byte{10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120, 130, 140, 150, 160}
	original := &OpLock{
		LeaseKey:       [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		LeaseState:     LeaseStateRead | LeaseStateHandle,
		ParentLeaseKey: parentKey,
		IsDirectory:    true,
		Epoch:          5,
	}

	clone := original.Clone()

	require.NotNil(t, clone)
	assert.Equal(t, parentKey, clone.ParentLeaseKey, "ParentLeaseKey should be copied")
	assert.True(t, clone.IsDirectory, "IsDirectory should be copied")

	// Modify clone and verify original unchanged
	clone.ParentLeaseKey[0] = 99
	clone.IsDirectory = false
	assert.Equal(t, byte(10), original.ParentLeaseKey[0])
	assert.True(t, original.IsDirectory)
}

// ============================================================================
// PersistedLock V2 Fields Tests
// ============================================================================

func TestPersistedLock_V2Fields_Exist(t *testing.T) {
	t.Parallel()

	pl := &PersistedLock{
		ID:             "test-id",
		ParentLeaseKey: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		IsDirectory:    true,
	}

	assert.NotNil(t, pl.ParentLeaseKey)
	assert.True(t, pl.IsDirectory)
}

func TestToPersistedLock_CopiesV2Fields(t *testing.T) {
	t.Parallel()

	parentKey := [16]byte{10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120, 130, 140, 150, 160}
	lock := &UnifiedLock{
		ID:         "lock-1",
		Owner:      LockOwner{OwnerID: "owner1", ClientID: "client1", ShareName: "/share"},
		FileHandle: FileHandle("file1"),
		Lease: &OpLock{
			LeaseKey:       [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
			LeaseState:     LeaseStateRead | LeaseStateHandle,
			ParentLeaseKey: parentKey,
			IsDirectory:    true,
			Epoch:          3,
		},
	}

	pl := ToPersistedLock(lock, 1)

	assert.Equal(t, parentKey[:], pl.ParentLeaseKey, "ParentLeaseKey should be persisted")
	assert.True(t, pl.IsDirectory, "IsDirectory should be persisted")
}

func TestFromPersistedLock_RestoresV2Fields(t *testing.T) {
	t.Parallel()

	parentKey := []byte{10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120, 130, 140, 150, 160}
	pl := &PersistedLock{
		ID:             "lock-1",
		ShareName:      "/share",
		FileID:         "file1",
		OwnerID:        "owner1",
		ClientID:       "client1",
		LeaseKey:       []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		LeaseState:     LeaseStateRead | LeaseStateHandle,
		LeaseEpoch:     3,
		ParentLeaseKey: parentKey,
		IsDirectory:    true,
	}

	lock := FromPersistedLock(pl)

	require.NotNil(t, lock.Lease)
	var expectedParentKey [16]byte
	copy(expectedParentKey[:], parentKey)
	assert.Equal(t, expectedParentKey, lock.Lease.ParentLeaseKey, "ParentLeaseKey should be restored")
	assert.True(t, lock.Lease.IsDirectory, "IsDirectory should be restored")
}

func TestPersistedLock_V2Fields_RoundTrip(t *testing.T) {
	t.Parallel()

	parentKey := [16]byte{10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120, 130, 140, 150, 160}
	original := &UnifiedLock{
		ID:         "lock-1",
		Owner:      LockOwner{OwnerID: "owner1", ClientID: "client1", ShareName: "/share"},
		FileHandle: FileHandle("file1"),
		Lease: &OpLock{
			LeaseKey:       [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
			LeaseState:     LeaseStateRead | LeaseStateHandle,
			ParentLeaseKey: parentKey,
			IsDirectory:    true,
			Epoch:          7,
		},
	}

	// Round-trip: UnifiedLock -> PersistedLock -> UnifiedLock
	pl := ToPersistedLock(original, 1)
	restored := FromPersistedLock(pl)

	require.NotNil(t, restored.Lease)
	assert.Equal(t, parentKey, restored.Lease.ParentLeaseKey, "ParentLeaseKey should survive round-trip")
	assert.True(t, restored.Lease.IsDirectory, "IsDirectory should survive round-trip")
}

// ============================================================================
// LockManager Lease Interface Tests
// ============================================================================

func TestLockManager_HasLeaseOperations(t *testing.T) {
	t.Parallel()

	// Verify Manager satisfies LockManager (compile-time check in manager.go)
	// but also verify the new methods exist at runtime
	var lm LockManager = NewManager()

	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{10, 20, 30}

	// RequestLease should exist and be callable
	_, _, err := lm.RequestLease(ctx, FileHandle("file1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead, false)
	// It may panic or return an error for stub -- just verifying it compiles and is callable
	_ = err

	// AcknowledgeLeaseBreak should exist
	err = lm.AcknowledgeLeaseBreak(ctx, leaseKey, LeaseStateNone, 0)
	_ = err

	// ReleaseLease should exist
	err = lm.ReleaseLease(ctx, leaseKey)
	_ = err

	// ReclaimLease should exist
	_, err = lm.ReclaimLease(ctx, leaseKey, LeaseStateRead, false)
	_ = err

	// GetLeaseState should exist
	state, epoch, found := lm.GetLeaseState(ctx, leaseKey)
	_ = state
	_ = epoch
	_ = found
}

// ============================================================================
// CheckNLMLocksForLeaseConflict Tests
// ============================================================================

func TestCheckNLMLocksForLeaseConflict_NilLockStore(t *testing.T) {
	t.Parallel()

	// Should return false (no conflict) when lockStore is nil
	conflict := CheckNLMLocksForLeaseConflict(nil, context.Background(), "file1", LeaseStateRead|LeaseStateWrite, "client1")
	assert.False(t, conflict)
}

// ============================================================================
// HandleChecker Interface Tests
// ============================================================================

func TestHandleChecker_Interface(t *testing.T) {
	t.Parallel()

	// Verify HandleChecker interface exists with HandleExists method
	var checker HandleChecker = &mockHandleChecker{exists: true}
	assert.True(t, checker.HandleExists(FileHandle("test")))
}

func TestManager_SetHandleChecker(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	checker := &mockHandleChecker{exists: true}
	mgr.SetHandleChecker(checker)
	// No panic = success
}

// mockHandleChecker is a test helper
type mockHandleChecker struct {
	exists bool
}

func (m *mockHandleChecker) HandleExists(handle FileHandle) bool {
	return m.exists
}
