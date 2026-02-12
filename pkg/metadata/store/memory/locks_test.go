package memory

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

func TestMemoryLockStore_PutAndGetLock(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	lock := &metadata.PersistedLock{
		ID:               "test-lock-1",
		ShareName:        "/export",
		FileID:           "/export:file1",
		OwnerID:          "nlm:client1:pid123",
		ClientID:         "client-conn-1",
		LockType:         1, // Exclusive
		Offset:           100,
		Length:           500,
		ShareReservation: 0,
		AcquiredAt:       time.Now().Truncate(time.Millisecond),
		ServerEpoch:      1,
	}

	// Put lock
	err := store.PutLock(ctx, lock)
	if err != nil {
		t.Fatalf("PutLock failed: %v", err)
	}

	// Get lock
	retrieved, err := store.GetLock(ctx, lock.ID)
	if err != nil {
		t.Fatalf("GetLock failed: %v", err)
	}

	// Verify fields
	if retrieved.ID != lock.ID {
		t.Errorf("ID mismatch: got %s, want %s", retrieved.ID, lock.ID)
	}
	if retrieved.ShareName != lock.ShareName {
		t.Errorf("ShareName mismatch: got %s, want %s", retrieved.ShareName, lock.ShareName)
	}
	if retrieved.FileID != lock.FileID {
		t.Errorf("FileID mismatch: got %s, want %s", retrieved.FileID, lock.FileID)
	}
	if retrieved.OwnerID != lock.OwnerID {
		t.Errorf("OwnerID mismatch: got %s, want %s", retrieved.OwnerID, lock.OwnerID)
	}
	if retrieved.ClientID != lock.ClientID {
		t.Errorf("ClientID mismatch: got %s, want %s", retrieved.ClientID, lock.ClientID)
	}
	if retrieved.LockType != lock.LockType {
		t.Errorf("LockType mismatch: got %d, want %d", retrieved.LockType, lock.LockType)
	}
	if retrieved.Offset != lock.Offset {
		t.Errorf("Offset mismatch: got %d, want %d", retrieved.Offset, lock.Offset)
	}
	if retrieved.Length != lock.Length {
		t.Errorf("Length mismatch: got %d, want %d", retrieved.Length, lock.Length)
	}
	if !retrieved.AcquiredAt.Equal(lock.AcquiredAt) {
		t.Errorf("AcquiredAt mismatch: got %v, want %v", retrieved.AcquiredAt, lock.AcquiredAt)
	}
	if retrieved.ServerEpoch != lock.ServerEpoch {
		t.Errorf("ServerEpoch mismatch: got %d, want %d", retrieved.ServerEpoch, lock.ServerEpoch)
	}
}

func TestMemoryLockStore_GetLockNotFound(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	_, err := store.GetLock(ctx, "non-existent")
	if err == nil {
		t.Fatal("expected error for non-existent lock")
	}

	storeErr, ok := err.(*metadata.StoreError)
	if !ok {
		t.Fatalf("expected StoreError, got %T", err)
	}
	if storeErr.Code != metadata.ErrLockNotFound {
		t.Errorf("expected ErrLockNotFound, got %v", storeErr.Code)
	}
}

func TestMemoryLockStore_DeleteLock(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	lock := &metadata.PersistedLock{
		ID:          "delete-test-lock",
		ShareName:   "/export",
		FileID:      "/export:file1",
		OwnerID:     "owner1",
		ClientID:    "client1",
		AcquiredAt:  time.Now(),
		ServerEpoch: 1,
	}

	// Put lock
	err := store.PutLock(ctx, lock)
	if err != nil {
		t.Fatalf("PutLock failed: %v", err)
	}

	// Verify it exists
	_, err = store.GetLock(ctx, lock.ID)
	if err != nil {
		t.Fatalf("GetLock failed: %v", err)
	}

	// Delete lock
	err = store.DeleteLock(ctx, lock.ID)
	if err != nil {
		t.Fatalf("DeleteLock failed: %v", err)
	}

	// Verify it's gone
	_, err = store.GetLock(ctx, lock.ID)
	if err == nil {
		t.Fatal("expected error for deleted lock")
	}

	// Try to delete again (should fail)
	err = store.DeleteLock(ctx, lock.ID)
	if err == nil {
		t.Fatal("expected error for already deleted lock")
	}
}

func TestMemoryLockStore_ListLocks(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	// Create test locks
	locks := []*metadata.PersistedLock{
		{
			ID:          "lock1",
			ShareName:   "/export",
			FileID:      "/export:file1",
			OwnerID:     "owner1",
			ClientID:    "client1",
			AcquiredAt:  time.Now(),
			ServerEpoch: 1,
		},
		{
			ID:          "lock2",
			ShareName:   "/export",
			FileID:      "/export:file2",
			OwnerID:     "owner2",
			ClientID:    "client1",
			AcquiredAt:  time.Now(),
			ServerEpoch: 1,
		},
		{
			ID:          "lock3",
			ShareName:   "/data",
			FileID:      "/data:file1",
			OwnerID:     "owner1",
			ClientID:    "client2",
			AcquiredAt:  time.Now(),
			ServerEpoch: 1,
		},
	}

	for _, lock := range locks {
		if err := store.PutLock(ctx, lock); err != nil {
			t.Fatalf("PutLock failed: %v", err)
		}
	}

	// List all locks
	all, err := store.ListLocks(ctx, metadata.LockQuery{})
	if err != nil {
		t.Fatalf("ListLocks failed: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 locks, got %d", len(all))
	}

	// Query by FileID
	byFile, err := store.ListLocks(ctx, metadata.LockQuery{FileID: "/export:file1"})
	if err != nil {
		t.Fatalf("ListLocks by file failed: %v", err)
	}
	if len(byFile) != 1 {
		t.Errorf("expected 1 lock for file1, got %d", len(byFile))
	}

	// Query by OwnerID
	byOwner, err := store.ListLocks(ctx, metadata.LockQuery{OwnerID: "owner1"})
	if err != nil {
		t.Fatalf("ListLocks by owner failed: %v", err)
	}
	if len(byOwner) != 2 {
		t.Errorf("expected 2 locks for owner1, got %d", len(byOwner))
	}

	// Query by ClientID
	byClient, err := store.ListLocks(ctx, metadata.LockQuery{ClientID: "client1"})
	if err != nil {
		t.Fatalf("ListLocks by client failed: %v", err)
	}
	if len(byClient) != 2 {
		t.Errorf("expected 2 locks for client1, got %d", len(byClient))
	}

	// Query by ShareName
	byShare, err := store.ListLocks(ctx, metadata.LockQuery{ShareName: "/data"})
	if err != nil {
		t.Fatalf("ListLocks by share failed: %v", err)
	}
	if len(byShare) != 1 {
		t.Errorf("expected 1 lock for /data, got %d", len(byShare))
	}

	// Combined query
	combined, err := store.ListLocks(ctx, metadata.LockQuery{ShareName: "/export", ClientID: "client1"})
	if err != nil {
		t.Fatalf("ListLocks combined failed: %v", err)
	}
	if len(combined) != 2 {
		t.Errorf("expected 2 locks for combined query, got %d", len(combined))
	}
}

func TestMemoryLockStore_DeleteLocksByClient(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	// Create test locks
	locks := []*metadata.PersistedLock{
		{ID: "lock1", ShareName: "/export", FileID: "f1", OwnerID: "o1", ClientID: "client1", AcquiredAt: time.Now(), ServerEpoch: 1},
		{ID: "lock2", ShareName: "/export", FileID: "f2", OwnerID: "o2", ClientID: "client1", AcquiredAt: time.Now(), ServerEpoch: 1},
		{ID: "lock3", ShareName: "/export", FileID: "f3", OwnerID: "o3", ClientID: "client2", AcquiredAt: time.Now(), ServerEpoch: 1},
	}

	for _, lock := range locks {
		if err := store.PutLock(ctx, lock); err != nil {
			t.Fatalf("PutLock failed: %v", err)
		}
	}

	// Delete client1's locks
	count, err := store.DeleteLocksByClient(ctx, "client1")
	if err != nil {
		t.Fatalf("DeleteLocksByClient failed: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 locks deleted, got %d", count)
	}

	// Verify client1's locks are gone
	remaining, _ := store.ListLocks(ctx, metadata.LockQuery{ClientID: "client1"})
	if len(remaining) != 0 {
		t.Errorf("expected 0 locks for client1, got %d", len(remaining))
	}

	// Verify client2's lock still exists
	client2Locks, _ := store.ListLocks(ctx, metadata.LockQuery{ClientID: "client2"})
	if len(client2Locks) != 1 {
		t.Errorf("expected 1 lock for client2, got %d", len(client2Locks))
	}
}

func TestMemoryLockStore_DeleteLocksByFile(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	// Create test locks - multiple locks on same file
	locks := []*metadata.PersistedLock{
		{ID: "lock1", ShareName: "/export", FileID: "file1", OwnerID: "o1", ClientID: "c1", LockType: 0, Offset: 0, Length: 100, AcquiredAt: time.Now(), ServerEpoch: 1},
		{ID: "lock2", ShareName: "/export", FileID: "file1", OwnerID: "o2", ClientID: "c2", LockType: 0, Offset: 100, Length: 100, AcquiredAt: time.Now(), ServerEpoch: 1},
		{ID: "lock3", ShareName: "/export", FileID: "file2", OwnerID: "o3", ClientID: "c3", LockType: 1, Offset: 0, Length: 0, AcquiredAt: time.Now(), ServerEpoch: 1},
	}

	for _, lock := range locks {
		if err := store.PutLock(ctx, lock); err != nil {
			t.Fatalf("PutLock failed: %v", err)
		}
	}

	// Delete file1's locks
	count, err := store.DeleteLocksByFile(ctx, "file1")
	if err != nil {
		t.Fatalf("DeleteLocksByFile failed: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 locks deleted, got %d", count)
	}

	// Verify file1's locks are gone
	remaining, _ := store.ListLocks(ctx, metadata.LockQuery{FileID: "file1"})
	if len(remaining) != 0 {
		t.Errorf("expected 0 locks for file1, got %d", len(remaining))
	}

	// Verify file2's lock still exists
	file2Locks, _ := store.ListLocks(ctx, metadata.LockQuery{FileID: "file2"})
	if len(file2Locks) != 1 {
		t.Errorf("expected 1 lock for file2, got %d", len(file2Locks))
	}
}

func TestMemoryLockStore_ServerEpoch(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	// Initial epoch should be 0
	epoch, err := store.GetServerEpoch(ctx)
	if err != nil {
		t.Fatalf("GetServerEpoch failed: %v", err)
	}
	if epoch != 0 {
		t.Errorf("expected initial epoch 0, got %d", epoch)
	}

	// Increment epoch
	newEpoch, err := store.IncrementServerEpoch(ctx)
	if err != nil {
		t.Fatalf("IncrementServerEpoch failed: %v", err)
	}
	if newEpoch != 1 {
		t.Errorf("expected epoch 1 after increment, got %d", newEpoch)
	}

	// Verify GetServerEpoch returns new value
	epoch, err = store.GetServerEpoch(ctx)
	if err != nil {
		t.Fatalf("GetServerEpoch failed: %v", err)
	}
	if epoch != 1 {
		t.Errorf("expected epoch 1, got %d", epoch)
	}

	// Increment again
	newEpoch, err = store.IncrementServerEpoch(ctx)
	if err != nil {
		t.Fatalf("IncrementServerEpoch failed: %v", err)
	}
	if newEpoch != 2 {
		t.Errorf("expected epoch 2, got %d", newEpoch)
	}
}

func TestMemoryLockStore_PutOverwrites(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	lock := &metadata.PersistedLock{
		ID:          "overwrite-lock",
		ShareName:   "/export",
		FileID:      "file1",
		OwnerID:     "owner1",
		ClientID:    "client1",
		LockType:    0, // Shared
		Offset:      0,
		Length:      100,
		AcquiredAt:  time.Now(),
		ServerEpoch: 1,
	}

	// Put initial lock
	if err := store.PutLock(ctx, lock); err != nil {
		t.Fatalf("PutLock failed: %v", err)
	}

	// Update lock (same ID, different properties)
	lock.LockType = 1 // Change to exclusive
	lock.Length = 200
	lock.ServerEpoch = 2

	// Put updated lock (should overwrite)
	if err := store.PutLock(ctx, lock); err != nil {
		t.Fatalf("PutLock update failed: %v", err)
	}

	// Verify updated values
	retrieved, err := store.GetLock(ctx, lock.ID)
	if err != nil {
		t.Fatalf("GetLock failed: %v", err)
	}
	if retrieved.LockType != 1 {
		t.Errorf("expected LockType 1, got %d", retrieved.LockType)
	}
	if retrieved.Length != 200 {
		t.Errorf("expected Length 200, got %d", retrieved.Length)
	}
	if retrieved.ServerEpoch != 2 {
		t.Errorf("expected ServerEpoch 2, got %d", retrieved.ServerEpoch)
	}
}

func TestMemoryLockStore_Transaction(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	// Test lock operations within a transaction
	err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		lock := &metadata.PersistedLock{
			ID:          "tx-lock",
			ShareName:   "/export",
			FileID:      "tx-file",
			OwnerID:     "tx-owner",
			ClientID:    "tx-client",
			AcquiredAt:  time.Now(),
			ServerEpoch: 1,
		}

		// Put lock in transaction
		if err := tx.PutLock(ctx, lock); err != nil {
			return err
		}

		// Get lock in transaction
		retrieved, err := tx.GetLock(ctx, lock.ID)
		if err != nil {
			return err
		}
		if retrieved.ID != lock.ID {
			t.Errorf("ID mismatch in transaction")
		}

		// List locks in transaction
		locks, err := tx.ListLocks(ctx, metadata.LockQuery{})
		if err != nil {
			return err
		}
		if len(locks) != 1 {
			t.Errorf("expected 1 lock in transaction, got %d", len(locks))
		}

		return nil
	})

	if err != nil {
		t.Fatalf("Transaction failed: %v", err)
	}

	// Verify lock exists outside transaction
	lock, err := store.GetLock(ctx, "tx-lock")
	if err != nil {
		t.Fatalf("GetLock after transaction failed: %v", err)
	}
	if lock.ID != "tx-lock" {
		t.Errorf("Lock not persisted after transaction")
	}
}

func TestMemoryLockStore_ContextCancellation(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	lock := &metadata.PersistedLock{
		ID:         "cancel-lock",
		AcquiredAt: time.Now(),
	}

	// All operations should respect cancelled context
	if err := store.PutLock(ctx, lock); err == nil {
		t.Error("PutLock should fail with cancelled context")
	}

	// GetLock with cancelled context
	if _, err := store.GetLock(ctx, "any"); err == nil {
		t.Error("GetLock should fail with cancelled context")
	}

	// DeleteLock with cancelled context
	if err := store.DeleteLock(ctx, "any"); err == nil {
		t.Error("DeleteLock should fail with cancelled context")
	}

	// ListLocks with cancelled context
	if _, err := store.ListLocks(ctx, metadata.LockQuery{}); err == nil {
		t.Error("ListLocks should fail with cancelled context")
	}

	// DeleteLocksByClient with cancelled context
	if _, err := store.DeleteLocksByClient(ctx, "any"); err == nil {
		t.Error("DeleteLocksByClient should fail with cancelled context")
	}

	// DeleteLocksByFile with cancelled context
	if _, err := store.DeleteLocksByFile(ctx, "any"); err == nil {
		t.Error("DeleteLocksByFile should fail with cancelled context")
	}

	// GetServerEpoch with cancelled context
	if _, err := store.GetServerEpoch(ctx); err == nil {
		t.Error("GetServerEpoch should fail with cancelled context")
	}

	// IncrementServerEpoch with cancelled context
	if _, err := store.IncrementServerEpoch(ctx); err == nil {
		t.Error("IncrementServerEpoch should fail with cancelled context")
	}
}

// ============================================================================
// Lease Persistence Tests
// ============================================================================

func TestMemoryLockStore_LeasePersistence(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	// Create a lease with all fields populated
	leaseKey := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	lease := &metadata.PersistedLock{
		ID:           "lease-1",
		ShareName:    "/export",
		FileID:       "/export:file1",
		OwnerID:      "smb:lease:0102030405060708090a0b0c0d0e0f10",
		ClientID:     "smb-session-1",
		LockType:     0, // Shared (Read lease)
		Offset:       0,
		Length:       0, // Whole file
		AcquiredAt:   time.Now().Truncate(time.Millisecond),
		ServerEpoch:  1,
		LeaseKey:     leaseKey,
		LeaseState:   0x03, // R + W
		LeaseEpoch:   42,
		BreakToState: 0x01, // Breaking to R
		Breaking:     true,
	}

	// Put lease
	err := store.PutLock(ctx, lease)
	if err != nil {
		t.Fatalf("PutLock (lease) failed: %v", err)
	}

	// Get lease
	retrieved, err := store.GetLock(ctx, lease.ID)
	if err != nil {
		t.Fatalf("GetLock (lease) failed: %v", err)
	}

	// Verify all lease fields
	if len(retrieved.LeaseKey) != 16 {
		t.Errorf("LeaseKey length mismatch: got %d, want 16", len(retrieved.LeaseKey))
	}
	for i, b := range leaseKey {
		if retrieved.LeaseKey[i] != b {
			t.Errorf("LeaseKey byte %d mismatch: got %d, want %d", i, retrieved.LeaseKey[i], b)
		}
	}
	if retrieved.LeaseState != lease.LeaseState {
		t.Errorf("LeaseState mismatch: got %d, want %d", retrieved.LeaseState, lease.LeaseState)
	}
	if retrieved.LeaseEpoch != lease.LeaseEpoch {
		t.Errorf("LeaseEpoch mismatch: got %d, want %d", retrieved.LeaseEpoch, lease.LeaseEpoch)
	}
	if retrieved.BreakToState != lease.BreakToState {
		t.Errorf("BreakToState mismatch: got %d, want %d", retrieved.BreakToState, lease.BreakToState)
	}
	if retrieved.Breaking != lease.Breaking {
		t.Errorf("Breaking mismatch: got %v, want %v", retrieved.Breaking, lease.Breaking)
	}

	// Verify IsLease() works
	if !retrieved.IsLease() {
		t.Error("IsLease() should return true for lease")
	}
}

func TestMemoryLockStore_ByteRangeLockNoLeaseFields(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	// Create a byte-range lock (no lease fields)
	byteRangeLock := &metadata.PersistedLock{
		ID:          "byterange-1",
		ShareName:   "/export",
		FileID:      "/export:file1",
		OwnerID:     "nlm:client1:pid123",
		ClientID:    "nlm-client-1",
		LockType:    1, // Exclusive
		Offset:      100,
		Length:      500,
		AcquiredAt:  time.Now().Truncate(time.Millisecond),
		ServerEpoch: 1,
		// No lease fields set
	}

	// Put lock
	err := store.PutLock(ctx, byteRangeLock)
	if err != nil {
		t.Fatalf("PutLock failed: %v", err)
	}

	// Get lock
	retrieved, err := store.GetLock(ctx, byteRangeLock.ID)
	if err != nil {
		t.Fatalf("GetLock failed: %v", err)
	}

	// Verify lease fields are empty/zero
	if len(retrieved.LeaseKey) != 0 {
		t.Errorf("LeaseKey should be empty for byte-range lock, got %d bytes", len(retrieved.LeaseKey))
	}
	if retrieved.LeaseState != 0 {
		t.Errorf("LeaseState should be 0 for byte-range lock, got %d", retrieved.LeaseState)
	}
	if retrieved.LeaseEpoch != 0 {
		t.Errorf("LeaseEpoch should be 0 for byte-range lock, got %d", retrieved.LeaseEpoch)
	}
	if retrieved.BreakToState != 0 {
		t.Errorf("BreakToState should be 0 for byte-range lock, got %d", retrieved.BreakToState)
	}
	if retrieved.Breaking {
		t.Error("Breaking should be false for byte-range lock")
	}

	// Verify IsLease() works
	if retrieved.IsLease() {
		t.Error("IsLease() should return false for byte-range lock")
	}
}

func TestMemoryLockStore_ListLocksIsLeaseFilter(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	// Create mixed locks
	leaseKey := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	locks := []*metadata.PersistedLock{
		{
			ID:         "lease-1",
			ShareName:  "/export",
			FileID:     "file1",
			OwnerID:    "owner1",
			ClientID:   "client1",
			AcquiredAt: time.Now(),
			LeaseKey:   leaseKey,
			LeaseState: 0x01,
		},
		{
			ID:         "lease-2",
			ShareName:  "/export",
			FileID:     "file2",
			OwnerID:    "owner2",
			ClientID:   "client2",
			AcquiredAt: time.Now(),
			LeaseKey:   leaseKey,
			LeaseState: 0x07,
		},
		{
			ID:         "byterange-1",
			ShareName:  "/export",
			FileID:     "file1",
			OwnerID:    "owner3",
			ClientID:   "client3",
			Offset:     0,
			Length:     100,
			AcquiredAt: time.Now(),
			// No lease fields
		},
	}

	for _, lock := range locks {
		if err := store.PutLock(ctx, lock); err != nil {
			t.Fatalf("PutLock failed: %v", err)
		}
	}

	// List all locks
	all, err := store.ListLocks(ctx, metadata.LockQuery{})
	if err != nil {
		t.Fatalf("ListLocks failed: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 locks, got %d", len(all))
	}

	// List leases only
	isLeaseTrue := true
	leases, err := store.ListLocks(ctx, metadata.LockQuery{IsLease: &isLeaseTrue})
	if err != nil {
		t.Fatalf("ListLocks (leases only) failed: %v", err)
	}
	if len(leases) != 2 {
		t.Errorf("expected 2 leases, got %d", len(leases))
	}
	for _, l := range leases {
		if !l.IsLease() {
			t.Errorf("expected lease, got byte-range lock: %s", l.ID)
		}
	}

	// List byte-range locks only
	isLeaseFalse := false
	byteRangeLocks, err := store.ListLocks(ctx, metadata.LockQuery{IsLease: &isLeaseFalse})
	if err != nil {
		t.Fatalf("ListLocks (byte-range only) failed: %v", err)
	}
	if len(byteRangeLocks) != 1 {
		t.Errorf("expected 1 byte-range lock, got %d", len(byteRangeLocks))
	}
	for _, l := range byteRangeLocks {
		if l.IsLease() {
			t.Errorf("expected byte-range lock, got lease: %s", l.ID)
		}
	}
}

func TestMemoryLockStore_LeaseKeyDeepCopy(t *testing.T) {
	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	// Create a lease
	leaseKey := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	lease := &metadata.PersistedLock{
		ID:         "lease-copy-test",
		ShareName:  "/export",
		FileID:     "file1",
		OwnerID:    "owner1",
		ClientID:   "client1",
		AcquiredAt: time.Now(),
		LeaseKey:   leaseKey,
		LeaseState: 0x07,
	}

	// Put lease
	err := store.PutLock(ctx, lease)
	if err != nil {
		t.Fatalf("PutLock failed: %v", err)
	}

	// Modify original lease key (should not affect stored copy)
	leaseKey[0] = 0xFF

	// Get lease and verify the stored key is unchanged
	retrieved, err := store.GetLock(ctx, lease.ID)
	if err != nil {
		t.Fatalf("GetLock failed: %v", err)
	}

	if retrieved.LeaseKey[0] != 1 {
		t.Errorf("LeaseKey was not deep copied: got first byte %d, want 1", retrieved.LeaseKey[0])
	}

	// Modify retrieved lease key (should not affect stored copy)
	retrieved.LeaseKey[0] = 0xAA

	// Get lease again and verify still unchanged
	retrieved2, err := store.GetLock(ctx, lease.ID)
	if err != nil {
		t.Fatalf("GetLock failed: %v", err)
	}

	if retrieved2.LeaseKey[0] != 1 {
		t.Errorf("LeaseKey was modified by retrieval: got first byte %d, want 1", retrieved2.LeaseKey[0])
	}
}
