package lock

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Lease State Constants Tests
// ============================================================================

func TestLeaseStateConstants(t *testing.T) {
	t.Parallel()

	// Verify MS-SMB2 spec values
	assert.Equal(t, uint32(0x00), LeaseStateNone, "None should be 0x00")
	assert.Equal(t, uint32(0x01), LeaseStateRead, "Read should be 0x01")
	assert.Equal(t, uint32(0x02), LeaseStateWrite, "Write should be 0x02")
	assert.Equal(t, uint32(0x04), LeaseStateHandle, "Handle should be 0x04")
}

func TestLeaseStateCombinations(t *testing.T) {
	t.Parallel()

	// RW = Read | Write = 0x03
	assert.Equal(t, uint32(0x03), LeaseStateRead|LeaseStateWrite)

	// RH = Read | Handle = 0x05
	assert.Equal(t, uint32(0x05), LeaseStateRead|LeaseStateHandle)

	// RWH = Read | Write | Handle = 0x07
	assert.Equal(t, uint32(0x07), LeaseStateRead|LeaseStateWrite|LeaseStateHandle)
}

// ============================================================================
// OpLock Helper Methods Tests
// ============================================================================

func TestOpLock_HasRead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		state    uint32
		expected bool
	}{
		{"None", LeaseStateNone, false},
		{"Read only", LeaseStateRead, true},
		{"Write only", LeaseStateWrite, false},
		{"Handle only", LeaseStateHandle, false},
		{"Read+Write", LeaseStateRead | LeaseStateWrite, true},
		{"Read+Handle", LeaseStateRead | LeaseStateHandle, true},
		{"Read+Write+Handle", LeaseStateRead | LeaseStateWrite | LeaseStateHandle, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lease := &OpLock{LeaseState: tc.state}
			assert.Equal(t, tc.expected, lease.HasRead())
		})
	}
}

func TestOpLock_HasWrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		state    uint32
		expected bool
	}{
		{"None", LeaseStateNone, false},
		{"Read only", LeaseStateRead, false},
		{"Write only", LeaseStateWrite, true},
		{"Handle only", LeaseStateHandle, false},
		{"Read+Write", LeaseStateRead | LeaseStateWrite, true},
		{"Read+Handle", LeaseStateRead | LeaseStateHandle, false},
		{"Read+Write+Handle", LeaseStateRead | LeaseStateWrite | LeaseStateHandle, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lease := &OpLock{LeaseState: tc.state}
			assert.Equal(t, tc.expected, lease.HasWrite())
		})
	}
}

func TestOpLock_HasHandle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		state    uint32
		expected bool
	}{
		{"None", LeaseStateNone, false},
		{"Read only", LeaseStateRead, false},
		{"Write only", LeaseStateWrite, false},
		{"Handle only", LeaseStateHandle, true},
		{"Read+Write", LeaseStateRead | LeaseStateWrite, false},
		{"Read+Handle", LeaseStateRead | LeaseStateHandle, true},
		{"Read+Write+Handle", LeaseStateRead | LeaseStateWrite | LeaseStateHandle, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lease := &OpLock{LeaseState: tc.state}
			assert.Equal(t, tc.expected, lease.HasHandle())
		})
	}
}

func TestOpLock_IsBreaking(t *testing.T) {
	t.Parallel()

	lease := &OpLock{LeaseState: LeaseStateRead | LeaseStateWrite}
	assert.False(t, lease.IsBreaking())

	lease.Breaking = true
	assert.True(t, lease.IsBreaking())
}

func TestOpLock_StateString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state    uint32
		expected string
	}{
		{LeaseStateNone, "None"},
		{LeaseStateRead, "R"},
		{LeaseStateWrite, "W"},
		{LeaseStateHandle, "H"},
		{LeaseStateRead | LeaseStateWrite, "RW"},
		{LeaseStateRead | LeaseStateHandle, "RH"},
		{LeaseStateWrite | LeaseStateHandle, "WH"},
		{LeaseStateRead | LeaseStateWrite | LeaseStateHandle, "RWH"},
	}

	for _, tc := range tests {
		t.Run(tc.expected, func(t *testing.T) {
			lease := &OpLock{LeaseState: tc.state}
			assert.Equal(t, tc.expected, lease.StateString())
		})
	}
}

func TestLeaseStateToString_Unknown(t *testing.T) {
	t.Parallel()

	// Invalid state (bits that shouldn't be set)
	result := LeaseStateToString(0x08)
	assert.Contains(t, result, "Unknown")
}

// ============================================================================
// Lease State Validation Tests
// ============================================================================

func TestIsValidFileLeaseState(t *testing.T) {
	t.Parallel()

	validStates := []struct {
		state uint32
		name  string
	}{
		{LeaseStateNone, "None"},
		{LeaseStateRead, "R"},
		{LeaseStateRead | LeaseStateWrite, "RW"},
		{LeaseStateRead | LeaseStateHandle, "RH"},
		{LeaseStateRead | LeaseStateWrite | LeaseStateHandle, "RWH"},
	}

	for _, tc := range validStates {
		t.Run("valid_"+tc.name, func(t *testing.T) {
			assert.True(t, IsValidFileLeaseState(tc.state), "%s should be valid", tc.name)
		})
	}

	invalidStates := []struct {
		state uint32
		name  string
	}{
		{LeaseStateWrite, "W alone"},
		{LeaseStateHandle, "H alone"},
		{LeaseStateWrite | LeaseStateHandle, "WH without R"},
	}

	for _, tc := range invalidStates {
		t.Run("invalid_"+tc.name, func(t *testing.T) {
			assert.False(t, IsValidFileLeaseState(tc.state), "%s should be invalid", tc.name)
		})
	}
}

func TestIsValidDirectoryLeaseState(t *testing.T) {
	t.Parallel()

	validStates := []struct {
		state uint32
		name  string
	}{
		{LeaseStateNone, "None"},
		{LeaseStateRead, "R"},
		{LeaseStateRead | LeaseStateHandle, "RH"},
	}

	for _, tc := range validStates {
		t.Run("valid_"+tc.name, func(t *testing.T) {
			assert.True(t, IsValidDirectoryLeaseState(tc.state), "%s should be valid for directories", tc.name)
		})
	}

	invalidStates := []struct {
		state uint32
		name  string
	}{
		{LeaseStateWrite, "W"},
		{LeaseStateRead | LeaseStateWrite, "RW"},
		{LeaseStateRead | LeaseStateWrite | LeaseStateHandle, "RWH"},
	}

	for _, tc := range invalidStates {
		t.Run("invalid_"+tc.name, func(t *testing.T) {
			assert.False(t, IsValidDirectoryLeaseState(tc.state), "%s should be invalid for directories", tc.name)
		})
	}
}

// ============================================================================
// OpLock Clone Tests
// ============================================================================

func TestOpLock_Clone(t *testing.T) {
	t.Parallel()

	original := &OpLock{
		LeaseKey:     [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		LeaseState:   LeaseStateRead | LeaseStateWrite,
		BreakToState: LeaseStateRead,
		Breaking:     true,
		Epoch:        42,
		BreakStarted: time.Now(),
	}

	clone := original.Clone()

	require.NotNil(t, clone)
	assert.NotSame(t, original, clone, "Clone should be a different instance")
	assert.Equal(t, original.LeaseKey, clone.LeaseKey)
	assert.Equal(t, original.LeaseState, clone.LeaseState)
	assert.Equal(t, original.BreakToState, clone.BreakToState)
	assert.Equal(t, original.Breaking, clone.Breaking)
	assert.Equal(t, original.Epoch, clone.Epoch)
	assert.Equal(t, original.BreakStarted, clone.BreakStarted)

	// Modify clone and verify original is unchanged
	clone.LeaseState = LeaseStateNone
	clone.Breaking = false
	assert.Equal(t, LeaseStateRead|LeaseStateWrite, original.LeaseState)
	assert.True(t, original.Breaking)
}

func TestOpLock_Clone_Nil(t *testing.T) {
	t.Parallel()

	var lease *OpLock
	clone := lease.Clone()
	assert.Nil(t, clone)
}

// ============================================================================
// Lease Conflict Detection Tests
// ============================================================================

func TestOpLocksConflict_SameKey(t *testing.T) {
	t.Parallel()

	key := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

	lease1 := &OpLock{LeaseKey: key, LeaseState: LeaseStateRead | LeaseStateWrite | LeaseStateHandle}
	lease2 := &OpLock{LeaseKey: key, LeaseState: LeaseStateRead | LeaseStateWrite | LeaseStateHandle}

	// Same key = no conflict, regardless of state
	assert.False(t, OpLocksConflict(lease1, lease2), "Same key should never conflict")
}

func TestOpLocksConflict_DifferentKeys_Write(t *testing.T) {
	t.Parallel()

	key1 := [16]byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	key2 := [16]byte{2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}

	// Existing has Write - conflicts with any Read or Write request
	existing := &OpLock{LeaseKey: key1, LeaseState: LeaseStateRead | LeaseStateWrite}
	requested := &OpLock{LeaseKey: key2, LeaseState: LeaseStateRead}

	assert.True(t, OpLocksConflict(existing, requested), "Write lease should conflict with Read request")

	// Requested wants Write - conflicts with existing Read
	existing2 := &OpLock{LeaseKey: key1, LeaseState: LeaseStateRead}
	requested2 := &OpLock{LeaseKey: key2, LeaseState: LeaseStateRead | LeaseStateWrite}

	assert.True(t, OpLocksConflict(existing2, requested2), "Write request should conflict with Read lease")
}

func TestOpLocksConflict_DifferentKeys_ReadOnly(t *testing.T) {
	t.Parallel()

	key1 := [16]byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	key2 := [16]byte{2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}

	// Multiple Read leases can coexist
	lease1 := &OpLock{LeaseKey: key1, LeaseState: LeaseStateRead}
	lease2 := &OpLock{LeaseKey: key2, LeaseState: LeaseStateRead}

	assert.False(t, OpLocksConflict(lease1, lease2), "Read leases should not conflict")

	// Read+Handle also doesn't conflict with Read
	lease3 := &OpLock{LeaseKey: key1, LeaseState: LeaseStateRead | LeaseStateHandle}
	lease4 := &OpLock{LeaseKey: key2, LeaseState: LeaseStateRead | LeaseStateHandle}

	assert.False(t, OpLocksConflict(lease3, lease4), "RH leases should not conflict")
}

func TestOpLocksConflict_BreakingLease(t *testing.T) {
	t.Parallel()

	key1 := [16]byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	key2 := [16]byte{2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}

	// Breaking lease - should use BreakToState for conflict check
	existing := &OpLock{
		LeaseKey:     key1,
		LeaseState:   LeaseStateRead | LeaseStateWrite, // Currently has RW
		BreakToState: LeaseStateRead,                   // Breaking to R
		Breaking:     true,
	}
	requested := &OpLock{LeaseKey: key2, LeaseState: LeaseStateRead | LeaseStateWrite}

	// After break completes, existing will be R only - no conflict with new RW
	// But during break, we use BreakToState (R) for conflict check
	// R doesn't conflict with RW request's Read component, but RW request has Write
	// which conflicts with any existing read (need exclusive)
	assert.True(t, OpLocksConflict(existing, requested), "Write request conflicts with Read lease")
}

// ============================================================================
// Lease vs Byte-Range Lock Conflict Tests
// ============================================================================

func TestOpLockConflictsWithByteLock_SameOwner(t *testing.T) {
	t.Parallel()

	lease := &OpLock{LeaseState: LeaseStateRead | LeaseStateWrite}
	lock := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "owner1"},
		Type:   LockTypeExclusive,
		Offset: 0,
		Length: 100,
	}

	// Same owner - no conflict
	assert.False(t, opLockConflictsWithByteLock(lease, "owner1", lock))
}

func TestOpLockConflictsWithByteLock_WriteLeaseVsExclusive(t *testing.T) {
	t.Parallel()

	lease := &OpLock{LeaseState: LeaseStateRead | LeaseStateWrite}
	lock := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "owner2"},
		Type:   LockTypeExclusive,
		Offset: 0,
		Length: 100,
	}

	// Write lease conflicts with exclusive byte-range lock
	assert.True(t, opLockConflictsWithByteLock(lease, "owner1", lock))
}

func TestOpLockConflictsWithByteLock_ReadLeaseVsShared(t *testing.T) {
	t.Parallel()

	lease := &OpLock{LeaseState: LeaseStateRead}
	lock := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "owner2"},
		Type:   LockTypeShared,
		Offset: 0,
		Length: 100,
	}

	// Read lease doesn't conflict with shared byte-range lock
	assert.False(t, opLockConflictsWithByteLock(lease, "owner1", lock))
}

func TestOpLockConflictsWithByteLock_ReadLeaseVsExclusive(t *testing.T) {
	t.Parallel()

	lease := &OpLock{LeaseState: LeaseStateRead}
	lock := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "owner2"},
		Type:   LockTypeExclusive,
		Offset: 0,
		Length: 100,
	}

	// Read-only lease doesn't conflict with exclusive lock (no Write to protect)
	assert.False(t, opLockConflictsWithByteLock(lease, "owner1", lock))
}

// ============================================================================
// UnifiedLock with Lease Tests
// ============================================================================

func TestUnifiedLock_IsLease(t *testing.T) {
	t.Parallel()

	// Byte-range lock (no Lease field)
	byteRangeLock := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "owner1"},
		Offset: 0,
		Length: 100,
		Type:   LockTypeExclusive,
	}
	assert.False(t, byteRangeLock.IsLease())

	// Lease (has Lease field)
	lease := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "owner1"},
		Offset: 0,
		Length: 0, // Whole file
		Type:   LockTypeShared,
		Lease: &OpLock{
			LeaseKey:   [16]byte{1, 2, 3},
			LeaseState: LeaseStateRead,
		},
	}
	assert.True(t, lease.IsLease())
}

func TestUnifiedLock_Clone_WithLease(t *testing.T) {
	t.Parallel()

	original := &UnifiedLock{
		ID:         "lock-123",
		Owner:      LockOwner{OwnerID: "owner1", ClientID: "client1", ShareName: "/share"},
		FileHandle: FileHandle("file-handle"),
		Offset:     0,
		Length:     0,
		Type:       LockTypeShared,
		AcquiredAt: time.Now(),
		Lease: &OpLock{
			LeaseKey:   [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
			LeaseState: LeaseStateRead | LeaseStateWrite,
			Epoch:      5,
		},
	}

	clone := original.Clone()

	require.NotNil(t, clone)
	require.NotNil(t, clone.Lease)
	assert.NotSame(t, original.Lease, clone.Lease, "Lease should be deep copied")
	assert.Equal(t, original.Lease.LeaseKey, clone.Lease.LeaseKey)
	assert.Equal(t, original.Lease.LeaseState, clone.Lease.LeaseState)
	assert.Equal(t, original.Lease.Epoch, clone.Lease.Epoch)

	// Modify clone's lease
	clone.Lease.LeaseState = LeaseStateNone
	assert.Equal(t, LeaseStateRead|LeaseStateWrite, original.Lease.LeaseState)
}

func TestIsUnifiedLockConflicting_LeaseVsLease(t *testing.T) {
	t.Parallel()

	key1 := [16]byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	key2 := [16]byte{2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}

	lease1 := &UnifiedLock{
		Owner: LockOwner{OwnerID: "owner1"},
		Lease: &OpLock{LeaseKey: key1, LeaseState: LeaseStateRead | LeaseStateWrite},
	}
	lease2 := &UnifiedLock{
		Owner: LockOwner{OwnerID: "owner2"},
		Lease: &OpLock{LeaseKey: key2, LeaseState: LeaseStateRead},
	}

	// Write lease conflicts with Read lease from different owner
	assert.True(t, IsUnifiedLockConflicting(lease1, lease2))
}

func TestIsUnifiedLockConflicting_LeaseVsByteRange(t *testing.T) {
	t.Parallel()

	lease := &UnifiedLock{
		Owner: LockOwner{OwnerID: "owner1"},
		Lease: &OpLock{LeaseState: LeaseStateRead | LeaseStateWrite},
	}
	byteRange := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "owner2"},
		Offset: 0,
		Length: 100,
		Type:   LockTypeExclusive,
	}

	// Write lease conflicts with exclusive byte-range lock
	assert.True(t, IsUnifiedLockConflicting(lease, byteRange))
	assert.True(t, IsUnifiedLockConflicting(byteRange, lease))
}

func TestIsUnifiedLockConflicting_ByteRangeVsByteRange(t *testing.T) {
	t.Parallel()

	lock1 := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "owner1"},
		Offset: 0,
		Length: 100,
		Type:   LockTypeShared,
	}
	lock2 := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "owner2"},
		Offset: 50,
		Length: 100,
		Type:   LockTypeShared,
	}

	// Shared locks don't conflict
	assert.False(t, IsUnifiedLockConflicting(lock1, lock2))

	// Make lock2 exclusive
	lock2.Type = LockTypeExclusive
	assert.True(t, IsUnifiedLockConflicting(lock1, lock2))
}

func TestIsUnifiedLockConflicting_SameOwner(t *testing.T) {
	t.Parallel()

	// Same owner, different lock types - no conflict
	lock1 := &UnifiedLock{
		Owner: LockOwner{OwnerID: "owner1"},
		Lease: &OpLock{LeaseState: LeaseStateRead | LeaseStateWrite},
	}
	lock2 := &UnifiedLock{
		Owner:  LockOwner{OwnerID: "owner1"},
		Offset: 0,
		Length: 100,
		Type:   LockTypeExclusive,
	}

	// Same owner - never conflicts
	assert.False(t, IsUnifiedLockConflicting(lock1, lock2))
}
