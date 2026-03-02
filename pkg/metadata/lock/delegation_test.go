package lock

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Delegation Creation Tests
// ============================================================================

func TestDelegation_NewDelegation(t *testing.T) {
	t.Parallel()

	deleg := NewDelegation(DelegTypeRead, "client1", "/export", false)
	require.NotNil(t, deleg)
	assert.NotEmpty(t, deleg.DelegationID, "DelegationID should be non-empty UUID")
	assert.Equal(t, DelegTypeRead, deleg.DelegType)
	assert.Equal(t, "client1", deleg.ClientID)
	assert.Equal(t, "/export", deleg.ShareName)
	assert.False(t, deleg.IsDirectory)
	assert.False(t, deleg.Breaking)
	assert.False(t, deleg.Recalled)
	assert.False(t, deleg.Revoked)
}

func TestDelegation_NewDelegation_WriteDirectory(t *testing.T) {
	t.Parallel()

	deleg := NewDelegation(DelegTypeWrite, "client2", "/data", true)
	require.NotNil(t, deleg)
	assert.Equal(t, DelegTypeWrite, deleg.DelegType)
	assert.True(t, deleg.IsDirectory)
}

func TestDelegationType_String(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "read", DelegTypeRead.String())
	assert.Equal(t, "write", DelegTypeWrite.String())
	assert.Equal(t, "unknown", DelegationType(99).String())
}

// ============================================================================
// Delegation Clone Tests
// ============================================================================

func TestDelegation_Clone(t *testing.T) {
	t.Parallel()

	original := NewDelegation(DelegTypeWrite, "client1", "/export", false)
	original.Breaking = true
	original.Recalled = true
	original.NotificationMask = 0xFF

	clone := original.Clone()
	require.NotNil(t, clone)

	// Fields should match
	assert.Equal(t, original.DelegationID, clone.DelegationID)
	assert.Equal(t, original.DelegType, clone.DelegType)
	assert.Equal(t, original.ClientID, clone.ClientID)
	assert.Equal(t, original.ShareName, clone.ShareName)
	assert.Equal(t, original.Breaking, clone.Breaking)
	assert.Equal(t, original.Recalled, clone.Recalled)
	assert.Equal(t, original.NotificationMask, clone.NotificationMask)

	// Modifying clone should not affect original
	clone.Breaking = false
	assert.True(t, original.Breaking, "modifying clone should not affect original")
}

func TestDelegation_Clone_Nil(t *testing.T) {
	t.Parallel()

	var deleg *Delegation
	assert.Nil(t, deleg.Clone())
}

// ============================================================================
// DelegationConflictsWithLease Tests
// ============================================================================

func TestDelegationConflictsWithLease_ReadDelegReadLease(t *testing.T) {
	t.Parallel()

	deleg := NewDelegation(DelegTypeRead, "client1", "/export", false)
	lease := &OpLock{
		LeaseKey:   [16]byte{1, 2, 3},
		LeaseState: LeaseStateRead,
	}

	assert.False(t, DelegationConflictsWithLease(deleg, lease),
		"read delegation + read lease should NOT conflict")
}

func TestDelegationConflictsWithLease_ReadDelegWriteLease(t *testing.T) {
	t.Parallel()

	deleg := NewDelegation(DelegTypeRead, "client1", "/export", false)
	lease := &OpLock{
		LeaseKey:   [16]byte{1, 2, 3},
		LeaseState: LeaseStateRead | LeaseStateWrite,
	}

	assert.True(t, DelegationConflictsWithLease(deleg, lease),
		"read delegation + write lease should conflict")
}

func TestDelegationConflictsWithLease_WriteDelegReadLease(t *testing.T) {
	t.Parallel()

	deleg := NewDelegation(DelegTypeWrite, "client1", "/export", false)
	lease := &OpLock{
		LeaseKey:   [16]byte{1, 2, 3},
		LeaseState: LeaseStateRead,
	}

	assert.True(t, DelegationConflictsWithLease(deleg, lease),
		"write delegation + read lease should conflict")
}

func TestDelegationConflictsWithLease_WriteDelegWriteLease(t *testing.T) {
	t.Parallel()

	deleg := NewDelegation(DelegTypeWrite, "client1", "/export", false)
	lease := &OpLock{
		LeaseKey:   [16]byte{1, 2, 3},
		LeaseState: LeaseStateRead | LeaseStateWrite,
	}

	assert.True(t, DelegationConflictsWithLease(deleg, lease),
		"write delegation + write lease should conflict")
}

func TestDelegationConflictsWithLease_NilInputs(t *testing.T) {
	t.Parallel()

	assert.False(t, DelegationConflictsWithLease(nil, nil),
		"nil inputs should not conflict")

	deleg := NewDelegation(DelegTypeRead, "client1", "/export", false)
	assert.False(t, DelegationConflictsWithLease(deleg, nil),
		"deleg + nil lease should not conflict")
	assert.False(t, DelegationConflictsWithLease(nil, &OpLock{}),
		"nil deleg + lease should not conflict")
}

func TestDelegationConflictsWithLease_ReadDelegHandleLease(t *testing.T) {
	t.Parallel()

	deleg := NewDelegation(DelegTypeRead, "client1", "/export", false)
	lease := &OpLock{
		LeaseKey:   [16]byte{1, 2, 3},
		LeaseState: LeaseStateRead | LeaseStateHandle,
	}

	// Read delegation with Read+Handle lease should NOT conflict
	// (no write involved on either side)
	assert.False(t, DelegationConflictsWithLease(deleg, lease),
		"read delegation + RH lease should NOT conflict")
}

// ============================================================================
// UnifiedLock Delegation Integration Tests
// ============================================================================

func TestUnifiedLock_IsDelegation(t *testing.T) {
	t.Parallel()

	// Regular byte-range lock
	byteLock := NewUnifiedLock(
		LockOwner{OwnerID: "test"},
		"file1", 0, 100, LockTypeExclusive,
	)
	assert.False(t, byteLock.IsDelegation(), "byte-range lock should not be delegation")

	// Lock with delegation
	byteLock.Delegation = NewDelegation(DelegTypeRead, "client1", "/export", false)
	assert.True(t, byteLock.IsDelegation(), "lock with Delegation should be delegation")
}

func TestUnifiedLock_Clone_WithDelegation(t *testing.T) {
	t.Parallel()

	lock := &UnifiedLock{
		ID:    "test-lock",
		Owner: LockOwner{OwnerID: "owner1"},
		Delegation: &Delegation{
			DelegationID:     "deleg-1",
			DelegType:        DelegTypeWrite,
			ClientID:         "client1",
			ShareName:        "/export",
			Breaking:         true,
			NotificationMask: 0xFF,
		},
	}

	clone := lock.Clone()
	require.NotNil(t, clone.Delegation)
	assert.Equal(t, "deleg-1", clone.Delegation.DelegationID)
	assert.True(t, clone.Delegation.Breaking)

	// Ensure deep copy
	clone.Delegation.Breaking = false
	assert.True(t, lock.Delegation.Breaking, "modifying clone delegation should not affect original")
}

// ============================================================================
// PersistedLock Delegation Round-Trip Tests
// ============================================================================

func TestPersistedLock_DelegationRoundTrip(t *testing.T) {
	t.Parallel()

	original := &UnifiedLock{
		ID:         "lock-1",
		Owner:      LockOwner{OwnerID: "nfs4:client1:stateid1", ClientID: "conn1", ShareName: "/export"},
		FileHandle: "file123",
		Offset:     0,
		Length:     0,
		Type:       LockTypeShared,
		Delegation: &Delegation{
			DelegationID:     "deleg-1",
			DelegType:        DelegTypeRead,
			IsDirectory:      false,
			ClientID:         "conn1",
			ShareName:        "/export",
			Breaking:         true,
			Recalled:         true,
			Revoked:          false,
			NotificationMask: 0xAB,
		},
	}

	// Serialize
	persisted := ToPersistedLock(original, 42)
	assert.Equal(t, "deleg-1", persisted.DelegationID)
	assert.Equal(t, int(DelegTypeRead), persisted.DelegType)
	assert.True(t, persisted.DelegBreaking)
	assert.True(t, persisted.DelegRecalled)
	assert.False(t, persisted.DelegRevoked)
	assert.Equal(t, uint32(0xAB), persisted.DelegNotificationMask)

	// Deserialize
	restored := FromPersistedLock(persisted)
	require.NotNil(t, restored.Delegation, "Delegation should be restored")
	assert.Equal(t, "deleg-1", restored.Delegation.DelegationID)
	assert.Equal(t, DelegTypeRead, restored.Delegation.DelegType)
	assert.True(t, restored.Delegation.Breaking)
	assert.True(t, restored.Delegation.Recalled)
	assert.False(t, restored.Delegation.Revoked)
	assert.Equal(t, uint32(0xAB), restored.Delegation.NotificationMask)
}

func TestPersistedLock_NoDelegation(t *testing.T) {
	t.Parallel()

	// Regular byte-range lock without delegation
	original := &UnifiedLock{
		ID:         "lock-2",
		Owner:      LockOwner{OwnerID: "smb:session1", ClientID: "conn2", ShareName: "/data"},
		FileHandle: "file456",
		Offset:     100,
		Length:     200,
		Type:       LockTypeExclusive,
	}

	persisted := ToPersistedLock(original, 1)
	assert.Empty(t, persisted.DelegationID)

	restored := FromPersistedLock(persisted)
	assert.Nil(t, restored.Delegation, "Delegation should be nil for non-delegation lock")
}
