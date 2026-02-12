package lock

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Persistence Conversion Tests
// ============================================================================

func TestToPersistedLock_ByteRangeLock(t *testing.T) {
	t.Parallel()

	lock := &EnhancedLock{
		ID: "lock-123",
		Owner: LockOwner{
			OwnerID:   "nlm:client1:pid123",
			ClientID:  "nlm-conn-1",
			ShareName: "/export",
		},
		FileHandle:       FileHandle("file-handle-abc"),
		Offset:           100,
		Length:           500,
		Type:             LockTypeExclusive,
		ShareReservation: ShareReservationDenyWrite,
		AcquiredAt:       time.Date(2026, 2, 5, 12, 0, 0, 0, time.UTC),
		Blocking:         true,
		Reclaim:          true,
		// No Lease field
	}

	pl := ToPersistedLock(lock, 42)

	assert.Equal(t, "lock-123", pl.ID)
	assert.Equal(t, "/export", pl.ShareName)
	assert.Equal(t, "file-handle-abc", pl.FileID)
	assert.Equal(t, "nlm:client1:pid123", pl.OwnerID)
	assert.Equal(t, "nlm-conn-1", pl.ClientID)
	assert.Equal(t, 1, pl.LockType) // LockTypeExclusive = 1
	assert.Equal(t, uint64(100), pl.Offset)
	assert.Equal(t, uint64(500), pl.Length)
	assert.Equal(t, 2, pl.ShareReservation) // ShareReservationDenyWrite = 2
	assert.Equal(t, lock.AcquiredAt, pl.AcquiredAt)
	assert.Equal(t, uint64(42), pl.ServerEpoch)

	// Lease fields should be empty/zero
	assert.Empty(t, pl.LeaseKey)
	assert.Equal(t, uint32(0), pl.LeaseState)
	assert.Equal(t, uint16(0), pl.LeaseEpoch)
	assert.Equal(t, uint32(0), pl.BreakToState)
	assert.False(t, pl.Breaking)

	// Blocking and Reclaim are NOT persisted
	assert.False(t, pl.IsLease())
}

func TestToPersistedLock_Lease(t *testing.T) {
	t.Parallel()

	leaseKey := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	lock := &EnhancedLock{
		ID: "lease-456",
		Owner: LockOwner{
			OwnerID:   "smb:lease:0102030405060708090a0b0c0d0e0f10",
			ClientID:  "smb-session-1",
			ShareName: "/data",
		},
		FileHandle: FileHandle("smb-file-handle"),
		Offset:     0,
		Length:     0, // Whole file
		Type:       LockTypeShared,
		AcquiredAt: time.Date(2026, 2, 5, 12, 0, 0, 0, time.UTC),
		Lease: &LeaseInfo{
			LeaseKey:     leaseKey,
			LeaseState:   LeaseStateRead | LeaseStateWrite | LeaseStateHandle,
			BreakToState: LeaseStateRead,
			Breaking:     true,
			Epoch:        99,
		},
	}

	pl := ToPersistedLock(lock, 7)

	assert.Equal(t, "lease-456", pl.ID)
	assert.Equal(t, "/data", pl.ShareName)
	assert.Equal(t, "smb-file-handle", pl.FileID)
	assert.Equal(t, uint64(7), pl.ServerEpoch)

	// Verify lease fields
	require.Len(t, pl.LeaseKey, 16)
	for i, b := range leaseKey {
		assert.Equal(t, b, pl.LeaseKey[i], "LeaseKey byte %d mismatch", i)
	}
	assert.Equal(t, uint32(0x07), pl.LeaseState) // RWH
	assert.Equal(t, uint16(99), pl.LeaseEpoch)
	assert.Equal(t, uint32(0x01), pl.BreakToState) // R
	assert.True(t, pl.Breaking)
	assert.True(t, pl.IsLease())
}

func TestFromPersistedLock_ByteRangeLock(t *testing.T) {
	t.Parallel()

	pl := &PersistedLock{
		ID:               "lock-789",
		ShareName:        "/export",
		FileID:           "file-id-xyz",
		OwnerID:          "nlm:client2:pid456",
		ClientID:         "nlm-conn-2",
		LockType:         0, // Shared
		Offset:           200,
		Length:           300,
		ShareReservation: 3, // DenyAll
		AcquiredAt:       time.Date(2026, 2, 5, 14, 0, 0, 0, time.UTC),
		ServerEpoch:      10,
		// No lease fields
	}

	lock := FromPersistedLock(pl)

	assert.Equal(t, "lock-789", lock.ID)
	assert.Equal(t, "nlm:client2:pid456", lock.Owner.OwnerID)
	assert.Equal(t, "nlm-conn-2", lock.Owner.ClientID)
	assert.Equal(t, "/export", lock.Owner.ShareName)
	assert.Equal(t, FileHandle("file-id-xyz"), lock.FileHandle)
	assert.Equal(t, uint64(200), lock.Offset)
	assert.Equal(t, uint64(300), lock.Length)
	assert.Equal(t, LockTypeShared, lock.Type)
	assert.Equal(t, ShareReservationDenyAll, lock.ShareReservation)
	assert.Equal(t, pl.AcquiredAt, lock.AcquiredAt)

	// Runtime-only fields should be default
	assert.False(t, lock.Blocking)
	assert.False(t, lock.Reclaim)

	// Lease should be nil
	assert.Nil(t, lock.Lease)
	assert.False(t, lock.IsLease())
}

func TestFromPersistedLock_Lease(t *testing.T) {
	t.Parallel()

	leaseKey := []byte{10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120, 130, 140, 150, 160}
	pl := &PersistedLock{
		ID:           "lease-abc",
		ShareName:    "/shared",
		FileID:       "lease-file-id",
		OwnerID:      "smb:lease:abc123",
		ClientID:     "smb-client-1",
		LockType:     0,
		Offset:       0,
		Length:       0,
		AcquiredAt:   time.Date(2026, 2, 5, 16, 0, 0, 0, time.UTC),
		ServerEpoch:  20,
		LeaseKey:     leaseKey,
		LeaseState:   0x05, // RH
		LeaseEpoch:   55,
		BreakToState: 0x01, // R
		Breaking:     true,
	}

	lock := FromPersistedLock(pl)

	assert.Equal(t, "lease-abc", lock.ID)
	assert.True(t, lock.IsLease())

	require.NotNil(t, lock.Lease)
	expectedKey := [16]byte{10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120, 130, 140, 150, 160}
	assert.Equal(t, expectedKey, lock.Lease.LeaseKey)
	assert.Equal(t, uint32(0x05), lock.Lease.LeaseState)
	assert.Equal(t, uint16(55), lock.Lease.Epoch)
	assert.Equal(t, uint32(0x01), lock.Lease.BreakToState)
	assert.True(t, lock.Lease.Breaking)

	// BreakStarted is runtime-only, should be zero
	assert.True(t, lock.Lease.BreakStarted.IsZero())
}

func TestPersistedLock_RoundTrip_ByteRangeLock(t *testing.T) {
	t.Parallel()

	original := &EnhancedLock{
		ID: "roundtrip-byte-range",
		Owner: LockOwner{
			OwnerID:   "nlm:test:999",
			ClientID:  "test-client",
			ShareName: "/test",
		},
		FileHandle:       FileHandle("test-file"),
		Offset:           1000,
		Length:           2000,
		Type:             LockTypeExclusive,
		ShareReservation: ShareReservationDenyRead,
		AcquiredAt:       time.Now().Truncate(time.Millisecond),
	}

	// Convert to persisted and back
	persisted := ToPersistedLock(original, 100)
	restored := FromPersistedLock(persisted)

	// Verify round-trip
	assert.Equal(t, original.ID, restored.ID)
	assert.Equal(t, original.Owner.OwnerID, restored.Owner.OwnerID)
	assert.Equal(t, original.Owner.ClientID, restored.Owner.ClientID)
	assert.Equal(t, original.Owner.ShareName, restored.Owner.ShareName)
	assert.Equal(t, original.FileHandle, restored.FileHandle)
	assert.Equal(t, original.Offset, restored.Offset)
	assert.Equal(t, original.Length, restored.Length)
	assert.Equal(t, original.Type, restored.Type)
	assert.Equal(t, original.ShareReservation, restored.ShareReservation)
	assert.Equal(t, original.AcquiredAt, restored.AcquiredAt)
	assert.Nil(t, restored.Lease)
}

func TestPersistedLock_RoundTrip_Lease(t *testing.T) {
	t.Parallel()

	leaseKey := [16]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0x00}
	original := &EnhancedLock{
		ID: "roundtrip-lease",
		Owner: LockOwner{
			OwnerID:   "smb:lease:aabbccdd",
			ClientID:  "smb-session",
			ShareName: "/smb-share",
		},
		FileHandle: FileHandle("smb-file"),
		Offset:     0,
		Length:     0,
		Type:       LockTypeShared,
		AcquiredAt: time.Now().Truncate(time.Millisecond),
		Lease: &LeaseInfo{
			LeaseKey:     leaseKey,
			LeaseState:   LeaseStateRead | LeaseStateWrite,
			BreakToState: LeaseStateRead,
			Breaking:     true,
			Epoch:        77,
		},
	}

	// Convert to persisted and back
	persisted := ToPersistedLock(original, 200)
	restored := FromPersistedLock(persisted)

	// Verify round-trip
	assert.Equal(t, original.ID, restored.ID)
	assert.Equal(t, original.Owner.OwnerID, restored.Owner.OwnerID)
	assert.Equal(t, original.Owner.ClientID, restored.Owner.ClientID)
	assert.Equal(t, original.Owner.ShareName, restored.Owner.ShareName)
	assert.Equal(t, original.FileHandle, restored.FileHandle)
	assert.Equal(t, original.AcquiredAt, restored.AcquiredAt)

	require.NotNil(t, restored.Lease)
	assert.Equal(t, original.Lease.LeaseKey, restored.Lease.LeaseKey)
	assert.Equal(t, original.Lease.LeaseState, restored.Lease.LeaseState)
	assert.Equal(t, original.Lease.BreakToState, restored.Lease.BreakToState)
	assert.Equal(t, original.Lease.Breaking, restored.Lease.Breaking)
	assert.Equal(t, original.Lease.Epoch, restored.Lease.Epoch)
}

// ============================================================================
// LockQuery Tests
// ============================================================================

func TestLockQuery_IsEmpty(t *testing.T) {
	t.Parallel()

	empty := LockQuery{}
	assert.True(t, empty.IsEmpty())

	notEmpty1 := LockQuery{FileID: "file"}
	assert.False(t, notEmpty1.IsEmpty())

	notEmpty2 := LockQuery{OwnerID: "owner"}
	assert.False(t, notEmpty2.IsEmpty())

	notEmpty3 := LockQuery{ClientID: "client"}
	assert.False(t, notEmpty3.IsEmpty())

	notEmpty4 := LockQuery{ShareName: "share"}
	assert.False(t, notEmpty4.IsEmpty())

	isLease := true
	notEmpty5 := LockQuery{IsLease: &isLease}
	assert.False(t, notEmpty5.IsEmpty())
}

func TestLockQuery_MatchesLock(t *testing.T) {
	t.Parallel()

	leaseKey := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	lease := &PersistedLock{
		ID:         "test-lease",
		FileID:     "file1",
		OwnerID:    "owner1",
		ClientID:   "client1",
		ShareName:  "share1",
		LeaseKey:   leaseKey,
		LeaseState: 0x07,
	}

	byteRange := &PersistedLock{
		ID:        "test-byterange",
		FileID:    "file2",
		OwnerID:   "owner2",
		ClientID:  "client2",
		ShareName: "share2",
		// No lease fields
	}

	// Empty query matches everything
	emptyQuery := LockQuery{}
	assert.True(t, emptyQuery.MatchesLock(lease))
	assert.True(t, emptyQuery.MatchesLock(byteRange))

	// FileID filter
	assert.True(t, LockQuery{FileID: "file1"}.MatchesLock(lease))
	assert.False(t, LockQuery{FileID: "file1"}.MatchesLock(byteRange))

	// OwnerID filter
	assert.True(t, LockQuery{OwnerID: "owner1"}.MatchesLock(lease))
	assert.False(t, LockQuery{OwnerID: "owner1"}.MatchesLock(byteRange))

	// ClientID filter
	assert.True(t, LockQuery{ClientID: "client1"}.MatchesLock(lease))
	assert.False(t, LockQuery{ClientID: "client1"}.MatchesLock(byteRange))

	// ShareName filter
	assert.True(t, LockQuery{ShareName: "share1"}.MatchesLock(lease))
	assert.False(t, LockQuery{ShareName: "share1"}.MatchesLock(byteRange))

	// IsLease filter
	isLeaseTrue := true
	isLeaseFalse := false
	assert.True(t, LockQuery{IsLease: &isLeaseTrue}.MatchesLock(lease))
	assert.False(t, LockQuery{IsLease: &isLeaseTrue}.MatchesLock(byteRange))
	assert.False(t, LockQuery{IsLease: &isLeaseFalse}.MatchesLock(lease))
	assert.True(t, LockQuery{IsLease: &isLeaseFalse}.MatchesLock(byteRange))

	// Combined filters
	combinedQuery := LockQuery{
		FileID:   "file1",
		ClientID: "client1",
	}
	assert.True(t, combinedQuery.MatchesLock(lease))
	assert.False(t, combinedQuery.MatchesLock(byteRange))
}

func TestPersistedLock_IsLease(t *testing.T) {
	t.Parallel()

	leaseKey := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

	lease := &PersistedLock{LeaseKey: leaseKey}
	assert.True(t, lease.IsLease())

	byteRange := &PersistedLock{}
	assert.False(t, byteRange.IsLease())

	// Short key (invalid) - not a lease
	shortKey := &PersistedLock{LeaseKey: []byte{1, 2, 3}}
	assert.False(t, shortKey.IsLease())

	// Nil key - not a lease
	nilKey := &PersistedLock{LeaseKey: nil}
	assert.False(t, nilKey.IsLease())
}
