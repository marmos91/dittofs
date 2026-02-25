package metadata

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// mockLockStore implements lock.LockStore for testing UnifiedLockView.
type mockLockStore struct {
	locks []*lock.PersistedLock
	epoch uint64
}

func newMockLockStore() *mockLockStore {
	return &mockLockStore{
		locks: make([]*lock.PersistedLock, 0),
		epoch: 1,
	}
}

func (m *mockLockStore) PutLock(_ context.Context, lk *lock.PersistedLock) error {
	// Replace if exists
	for i, existing := range m.locks {
		if existing.ID == lk.ID {
			m.locks[i] = lk
			return nil
		}
	}
	m.locks = append(m.locks, lk)
	return nil
}

func (m *mockLockStore) GetLock(_ context.Context, lockID string) (*lock.PersistedLock, error) {
	for _, lk := range m.locks {
		if lk.ID == lockID {
			return lk, nil
		}
	}
	return nil, nil
}

func (m *mockLockStore) DeleteLock(_ context.Context, lockID string) error {
	for i, lk := range m.locks {
		if lk.ID == lockID {
			m.locks = append(m.locks[:i], m.locks[i+1:]...)
			return nil
		}
	}
	return nil
}

func (m *mockLockStore) ListLocks(_ context.Context, query lock.LockQuery) ([]*lock.PersistedLock, error) {
	var result []*lock.PersistedLock
	for _, lk := range m.locks {
		if query.MatchesLock(lk) {
			result = append(result, lk)
		}
	}
	return result, nil
}

func (m *mockLockStore) DeleteLocksByClient(_ context.Context, clientID string) (int, error) {
	count := 0
	remaining := make([]*lock.PersistedLock, 0)
	for _, lk := range m.locks {
		if lk.ClientID == clientID {
			count++
		} else {
			remaining = append(remaining, lk)
		}
	}
	m.locks = remaining
	return count, nil
}

func (m *mockLockStore) DeleteLocksByFile(_ context.Context, fileID string) (int, error) {
	count := 0
	remaining := make([]*lock.PersistedLock, 0)
	for _, lk := range m.locks {
		if lk.FileID == fileID {
			count++
		} else {
			remaining = append(remaining, lk)
		}
	}
	m.locks = remaining
	return count, nil
}

func (m *mockLockStore) GetServerEpoch(_ context.Context) (uint64, error) {
	return m.epoch, nil
}

func (m *mockLockStore) IncrementServerEpoch(_ context.Context) (uint64, error) {
	m.epoch++
	return m.epoch, nil
}

func (m *mockLockStore) ReclaimLease(_ context.Context, _ lock.FileHandle, _ [16]byte, _ string) (*lock.UnifiedLock, error) {
	// Mock implementation returns not found - reclaim not supported in mock
	return nil, nil
}

// Test helper to create a byte-range lock
func createByteRangeLock(id, fileID, ownerID string, offset, length uint64, exclusive bool) *lock.PersistedLock {
	lockType := 0 // shared
	if exclusive {
		lockType = 1 // exclusive
	}
	return &lock.PersistedLock{
		ID:          id,
		FileID:      fileID,
		OwnerID:     ownerID,
		ClientID:    "client1",
		LockType:    lockType,
		Offset:      offset,
		Length:      length,
		AcquiredAt:  time.Now(),
		ServerEpoch: 1,
	}
}

// Test helper to create a lease
func createLease(id, fileID, ownerID string, leaseState uint32, leaseKey [16]byte) *lock.PersistedLock {
	return &lock.PersistedLock{
		ID:          id,
		FileID:      fileID,
		OwnerID:     ownerID,
		ClientID:    "client1",
		LockType:    1, // exclusive for Write leases
		Offset:      0, // whole file
		Length:      0, // whole file
		AcquiredAt:  time.Now(),
		ServerEpoch: 1,
		LeaseKey:    leaseKey[:],
		LeaseState:  leaseState,
		LeaseEpoch:  1,
	}
}

func TestUnifiedLockView_GetAllLocksOnFile_Empty(t *testing.T) {
	store := newMockLockStore()
	view := NewUnifiedLockView(store)
	ctx := context.Background()

	info, err := view.GetAllLocksOnFile(ctx, "file1")
	if err != nil {
		t.Fatalf("GetAllLocksOnFile failed: %v", err)
	}

	if len(info.ByteRangeLocks) != 0 {
		t.Errorf("Expected 0 byte-range locks, got %d", len(info.ByteRangeLocks))
	}
	if len(info.Leases) != 0 {
		t.Errorf("Expected 0 leases, got %d", len(info.Leases))
	}
	if info.HasAnyLocks() {
		t.Error("HasAnyLocks should return false for empty file")
	}
	if info.TotalCount() != 0 {
		t.Errorf("TotalCount should be 0, got %d", info.TotalCount())
	}
}

func TestUnifiedLockView_GetAllLocksOnFile_ByteRangeLocks(t *testing.T) {
	store := newMockLockStore()
	ctx := context.Background()

	// Add some byte-range locks
	_ = store.PutLock(ctx, createByteRangeLock("lock1", "file1", "nlm:client1:123:abc", 0, 100, false))
	_ = store.PutLock(ctx, createByteRangeLock("lock2", "file1", "nlm:client2:456:def", 200, 100, true))
	_ = store.PutLock(ctx, createByteRangeLock("lock3", "file2", "nlm:client1:123:abc", 0, 100, false)) // different file

	view := NewUnifiedLockView(store)
	info, err := view.GetAllLocksOnFile(ctx, "file1")
	if err != nil {
		t.Fatalf("GetAllLocksOnFile failed: %v", err)
	}

	if len(info.ByteRangeLocks) != 2 {
		t.Errorf("Expected 2 byte-range locks, got %d", len(info.ByteRangeLocks))
	}
	if len(info.Leases) != 0 {
		t.Errorf("Expected 0 leases, got %d", len(info.Leases))
	}
	if info.TotalCount() != 2 {
		t.Errorf("TotalCount should be 2, got %d", info.TotalCount())
	}
}

func TestUnifiedLockView_GetAllLocksOnFile_Leases(t *testing.T) {
	store := newMockLockStore()
	ctx := context.Background()

	// Add some leases
	leaseKey1 := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	leaseKey2 := [16]byte{16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}

	_ = store.PutLock(ctx, createLease("lease1", "file1", "smb:client1", lock.LeaseStateRead|lock.LeaseStateWrite, leaseKey1))
	_ = store.PutLock(ctx, createLease("lease2", "file1", "smb:client2", lock.LeaseStateRead, leaseKey2))

	view := NewUnifiedLockView(store)
	info, err := view.GetAllLocksOnFile(ctx, "file1")
	if err != nil {
		t.Fatalf("GetAllLocksOnFile failed: %v", err)
	}

	if len(info.ByteRangeLocks) != 0 {
		t.Errorf("Expected 0 byte-range locks, got %d", len(info.ByteRangeLocks))
	}
	if len(info.Leases) != 2 {
		t.Errorf("Expected 2 leases, got %d", len(info.Leases))
	}
	if !info.HasAnyLocks() {
		t.Error("HasAnyLocks should return true")
	}
}

func TestUnifiedLockView_GetAllLocksOnFile_Mixed(t *testing.T) {
	store := newMockLockStore()
	ctx := context.Background()

	// Add a byte-range lock and a lease on the same file
	_ = store.PutLock(ctx, createByteRangeLock("lock1", "file1", "nlm:client1:123:abc", 0, 100, false))
	leaseKey := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	_ = store.PutLock(ctx, createLease("lease1", "file1", "smb:client2", lock.LeaseStateRead|lock.LeaseStateWrite, leaseKey))

	view := NewUnifiedLockView(store)
	info, err := view.GetAllLocksOnFile(ctx, "file1")
	if err != nil {
		t.Fatalf("GetAllLocksOnFile failed: %v", err)
	}

	if len(info.ByteRangeLocks) != 1 {
		t.Errorf("Expected 1 byte-range lock, got %d", len(info.ByteRangeLocks))
	}
	if len(info.Leases) != 1 {
		t.Errorf("Expected 1 lease, got %d", len(info.Leases))
	}
	if info.TotalCount() != 2 {
		t.Errorf("TotalCount should be 2, got %d", info.TotalCount())
	}
}

func TestUnifiedLockView_HasConflictingLocks_NoConflict(t *testing.T) {
	store := newMockLockStore()
	ctx := context.Background()

	view := NewUnifiedLockView(store)

	// Empty file should have no conflicts
	hasConflict, conflicts, err := view.HasConflictingLocks(ctx, "file1", lock.LockTypeExclusive)
	if err != nil {
		t.Fatalf("HasConflictingLocks failed: %v", err)
	}
	if hasConflict {
		t.Error("Expected no conflict on empty file")
	}
	if len(conflicts) != 0 {
		t.Errorf("Expected 0 conflicts, got %d", len(conflicts))
	}
}

func TestUnifiedLockView_HasConflictingLocks_ExclusiveVsExisting(t *testing.T) {
	store := newMockLockStore()
	ctx := context.Background()

	// Add an existing shared lock
	_ = store.PutLock(ctx, createByteRangeLock("lock1", "file1", "nlm:client1:123:abc", 0, 100, false))

	view := NewUnifiedLockView(store)

	// Exclusive lock should conflict with shared lock
	hasConflict, conflicts, err := view.HasConflictingLocks(ctx, "file1", lock.LockTypeExclusive)
	if err != nil {
		t.Fatalf("HasConflictingLocks failed: %v", err)
	}
	if !hasConflict {
		t.Error("Expected conflict: exclusive vs shared")
	}
	if len(conflicts) != 1 {
		t.Errorf("Expected 1 conflict, got %d", len(conflicts))
	}
}

func TestUnifiedLockView_HasConflictingLocks_SharedVsShared(t *testing.T) {
	store := newMockLockStore()
	ctx := context.Background()

	// Add an existing shared lock
	_ = store.PutLock(ctx, createByteRangeLock("lock1", "file1", "nlm:client1:123:abc", 0, 100, false))

	view := NewUnifiedLockView(store)

	// Shared lock should NOT conflict with shared lock
	hasConflict, _, err := view.HasConflictingLocks(ctx, "file1", lock.LockTypeShared)
	if err != nil {
		t.Fatalf("HasConflictingLocks failed: %v", err)
	}
	if hasConflict {
		t.Error("Expected no conflict: shared vs shared")
	}
}

func TestUnifiedLockView_HasConflictingLocks_VsLease(t *testing.T) {
	store := newMockLockStore()
	ctx := context.Background()

	// Add a Write lease
	leaseKey := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	_ = store.PutLock(ctx, createLease("lease1", "file1", "smb:client1", lock.LeaseStateRead|lock.LeaseStateWrite, leaseKey))

	view := NewUnifiedLockView(store)

	// Exclusive lock should conflict with Write lease
	hasConflict, conflicts, err := view.HasConflictingLocks(ctx, "file1", lock.LockTypeExclusive)
	if err != nil {
		t.Fatalf("HasConflictingLocks failed: %v", err)
	}
	if !hasConflict {
		t.Error("Expected conflict: exclusive vs Write lease")
	}
	if len(conflicts) != 1 {
		t.Errorf("Expected 1 conflict, got %d", len(conflicts))
	}
	if conflicts[0].Lease == nil {
		t.Error("Expected conflicting lock to be a lease")
	}
}

func TestUnifiedLockView_GetLeaseByKey(t *testing.T) {
	store := newMockLockStore()
	ctx := context.Background()

	leaseKey1 := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	leaseKey2 := [16]byte{16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}

	_ = store.PutLock(ctx, createLease("lease1", "file1", "smb:client1", lock.LeaseStateRead, leaseKey1))
	_ = store.PutLock(ctx, createLease("lease2", "file1", "smb:client2", lock.LeaseStateRead|lock.LeaseStateWrite, leaseKey2))

	view := NewUnifiedLockView(store)

	// Find lease1 by key
	lease, err := view.GetLeaseByKey(ctx, "file1", leaseKey1)
	if err != nil {
		t.Fatalf("GetLeaseByKey failed: %v", err)
	}
	if lease == nil {
		t.Fatal("Expected to find lease1")
	}
	if lease.ID != "lease1" {
		t.Errorf("Expected lease1, got %s", lease.ID)
	}

	// Find non-existent key
	nonExistentKey := [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	lease, err = view.GetLeaseByKey(ctx, "file1", nonExistentKey)
	if err != nil {
		t.Fatalf("GetLeaseByKey failed: %v", err)
	}
	if lease != nil {
		t.Error("Expected nil for non-existent key")
	}
}

func TestUnifiedLockView_GetWriteLeases(t *testing.T) {
	store := newMockLockStore()
	ctx := context.Background()

	leaseKey1 := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	leaseKey2 := [16]byte{16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}
	leaseKey3 := [16]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}

	// Read-only lease
	_ = store.PutLock(ctx, createLease("lease1", "file1", "smb:client1", lock.LeaseStateRead, leaseKey1))
	// Read+Write lease
	_ = store.PutLock(ctx, createLease("lease2", "file1", "smb:client2", lock.LeaseStateRead|lock.LeaseStateWrite, leaseKey2))
	// Full RWH lease
	_ = store.PutLock(ctx, createLease("lease3", "file1", "smb:client3", lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle, leaseKey3))

	view := NewUnifiedLockView(store)

	writeLeases, err := view.GetWriteLeases(ctx, "file1")
	if err != nil {
		t.Fatalf("GetWriteLeases failed: %v", err)
	}

	if len(writeLeases) != 2 {
		t.Errorf("Expected 2 Write leases, got %d", len(writeLeases))
	}

	// Verify only Write leases returned
	for _, lease := range writeLeases {
		if lease.Lease == nil || !lease.Lease.HasWrite() {
			t.Errorf("Unexpected non-Write lease: %s", lease.ID)
		}
	}
}

func TestUnifiedLockView_GetHandleLeases(t *testing.T) {
	store := newMockLockStore()
	ctx := context.Background()

	leaseKey1 := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	leaseKey2 := [16]byte{16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}
	leaseKey3 := [16]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}

	// Read-only lease (no Handle)
	_ = store.PutLock(ctx, createLease("lease1", "file1", "smb:client1", lock.LeaseStateRead, leaseKey1))
	// Read+Handle lease
	_ = store.PutLock(ctx, createLease("lease2", "file1", "smb:client2", lock.LeaseStateRead|lock.LeaseStateHandle, leaseKey2))
	// Full RWH lease
	_ = store.PutLock(ctx, createLease("lease3", "file1", "smb:client3", lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle, leaseKey3))

	view := NewUnifiedLockView(store)

	handleLeases, err := view.GetHandleLeases(ctx, "file1")
	if err != nil {
		t.Fatalf("GetHandleLeases failed: %v", err)
	}

	if len(handleLeases) != 2 {
		t.Errorf("Expected 2 Handle leases, got %d", len(handleLeases))
	}

	// Verify only Handle leases returned
	for _, lease := range handleLeases {
		if lease.Lease == nil || !lease.Lease.HasHandle() {
			t.Errorf("Unexpected non-Handle lease: %s", lease.ID)
		}
	}
}
