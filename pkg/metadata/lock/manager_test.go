package lock

import (
	"sync"
	"testing"
	"time"
)

// ============================================================================
// LockManager Interface Compliance
// ============================================================================

func TestManager_ImplementsLockManager(t *testing.T) {
	t.Parallel()

	// Compile-time check is in manager.go (var _ LockManager = (*Manager)(nil))
	// Runtime verification:
	var lm LockManager = NewManager()
	if lm == nil {
		t.Fatal("NewManager should return non-nil LockManager")
	}
}

// ============================================================================
// Basic Lock Tests
// ============================================================================

func TestManager_Lock_Success(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	lock := FileLock{
		ID:        1,
		SessionID: 100,
		Offset:    0,
		Length:    100,
		Exclusive: true,
	}

	err := lm.Lock("file1", lock)
	if err != nil {
		t.Fatalf("Lock failed: %v", err)
	}

	locks := lm.ListLocks("file1")
	if len(locks) != 1 {
		t.Fatalf("Expected 1 lock, got %d", len(locks))
	}
}

func TestManager_Lock_Conflict(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	// First lock succeeds
	lock1 := FileLock{
		ID:        1,
		SessionID: 100,
		Offset:    0,
		Length:    100,
		Exclusive: true,
	}
	if err := lm.Lock("file1", lock1); err != nil {
		t.Fatalf("First lock failed: %v", err)
	}

	// Second lock conflicts
	lock2 := FileLock{
		ID:        2,
		SessionID: 200,
		Offset:    50,
		Length:    100,
		Exclusive: true,
	}
	err := lm.Lock("file1", lock2)
	if err == nil {
		t.Fatal("Expected conflict error")
	}
}

func TestManager_Lock_SharedNoConflict(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	// First shared lock
	lock1 := FileLock{
		ID:        1,
		SessionID: 100,
		Offset:    0,
		Length:    100,
		Exclusive: false,
	}
	if err := lm.Lock("file1", lock1); err != nil {
		t.Fatalf("First lock failed: %v", err)
	}

	// Second shared lock on same range - should succeed
	lock2 := FileLock{
		ID:        2,
		SessionID: 200,
		Offset:    0,
		Length:    100,
		Exclusive: false,
	}
	if err := lm.Lock("file1", lock2); err != nil {
		t.Fatalf("Second shared lock failed: %v", err)
	}

	locks := lm.ListLocks("file1")
	if len(locks) != 2 {
		t.Fatalf("Expected 2 locks, got %d", len(locks))
	}
}

func TestManager_Unlock_Success(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	lock := FileLock{
		ID:        1,
		SessionID: 100,
		Offset:    0,
		Length:    100,
		Exclusive: true,
	}
	_ = lm.Lock("file1", lock)

	err := lm.Unlock("file1", 100, 0, 100)
	if err != nil {
		t.Fatalf("Unlock failed: %v", err)
	}

	locks := lm.ListLocks("file1")
	if len(locks) != 0 {
		t.Fatalf("Expected 0 locks, got %d", len(locks))
	}
}

func TestManager_Unlock_NotFound(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	err := lm.Unlock("file1", 100, 0, 100)
	if err == nil {
		t.Fatal("Expected error for unlock of non-existent lock")
	}
}

func TestManager_UnlockAllForSession(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	// Add multiple locks from same session
	for i := 0; i < 5; i++ {
		lock := FileLock{
			ID:        uint64(i),
			SessionID: 100,
			Offset:    uint64(i * 100),
			Length:    100,
			Exclusive: true,
		}
		_ = lm.Lock("file1", lock)
	}

	// Add lock from different session
	otherLock := FileLock{
		ID:        99,
		SessionID: 200,
		Offset:    1000,
		Length:    100,
		Exclusive: true,
	}
	_ = lm.Lock("file1", otherLock)

	// Remove all locks for session 100
	removed := lm.UnlockAllForSession("file1", 100)
	if removed != 5 {
		t.Fatalf("Expected 5 locks removed, got %d", removed)
	}

	// Other session's lock should remain
	locks := lm.ListLocks("file1")
	if len(locks) != 1 {
		t.Fatalf("Expected 1 lock remaining, got %d", len(locks))
	}
}

func TestManager_TestLock(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	lock := FileLock{
		ID:        1,
		SessionID: 100,
		Offset:    0,
		Length:    100,
		Exclusive: true,
	}
	_ = lm.Lock("file1", lock)

	// Same session - should succeed (no conflict)
	testLock := FileLock{SessionID: 100, Offset: 50, Length: 50, Exclusive: true}
	conflict, err := lm.TestLock("file1", testLock)
	if err != nil {
		t.Fatalf("TestLock failed: %v", err)
	}
	if conflict != nil {
		t.Fatal("Expected no conflict for same session")
	}

	// Different session - should fail
	testLock2 := FileLock{SessionID: 200, Offset: 50, Length: 50, Exclusive: true}
	conflict, err = lm.TestLock("file1", testLock2)
	if err != nil {
		t.Fatalf("TestLock failed: %v", err)
	}
	if conflict == nil {
		t.Fatal("Expected conflict details")
	}
}

func TestManager_TestLockByParams(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	lock := FileLock{
		ID:        1,
		SessionID: 100,
		Offset:    0,
		Length:    100,
		Exclusive: true,
	}
	_ = lm.Lock("file1", lock)

	// Same session - should succeed
	ok, conflict := lm.TestLockByParams("file1", 100, 50, 50, true)
	if !ok {
		t.Fatal("Expected test lock to succeed for same session")
	}
	if conflict != nil {
		t.Fatal("Expected no conflict")
	}

	// Different session - should fail
	ok, conflict = lm.TestLockByParams("file1", 200, 50, 50, true)
	if ok {
		t.Fatal("Expected test lock to fail for different session")
	}
	if conflict == nil {
		t.Fatal("Expected conflict details")
	}
}

func TestManager_CheckForIO(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	lock := FileLock{
		ID:        1,
		SessionID: 100,
		Offset:    0,
		Length:    100,
		Exclusive: true,
	}
	_ = lm.Lock("file1", lock)

	// Same session write - allowed
	conflict := lm.CheckForIO("file1", 100, 0, 50, true)
	if conflict != nil {
		t.Fatal("Expected same session write to be allowed")
	}

	// Different session read with exclusive lock - blocked
	conflict = lm.CheckForIO("file1", 200, 0, 50, false)
	if conflict == nil {
		t.Fatal("Expected read to be blocked by exclusive lock")
	}

	// Different session write - blocked
	conflict = lm.CheckForIO("file1", 200, 0, 50, true)
	if conflict == nil {
		t.Fatal("Expected write to be blocked")
	}
}

// ============================================================================
// Range Overlap Tests
// ============================================================================

func TestRangesOverlap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		o1, l1  uint64
		o2, l2  uint64
		overlap bool
	}{
		{"adjacent", 0, 10, 10, 10, false},
		{"overlap", 0, 10, 5, 10, true},
		{"contained", 0, 100, 10, 10, true},
		{"no overlap", 0, 10, 20, 10, false},
		{"unbounded first", 0, 0, 100, 10, true},
		{"unbounded second", 100, 10, 0, 0, true},
		{"both unbounded", 0, 0, 100, 0, true},
		{"same range", 0, 10, 0, 10, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := RangesOverlap(tt.o1, tt.l1, tt.o2, tt.l2)
			if result != tt.overlap {
				t.Errorf("RangesOverlap(%d,%d,%d,%d) = %v, want %v",
					tt.o1, tt.l1, tt.o2, tt.l2, result, tt.overlap)
			}
		})
	}
}

// ============================================================================
// Unified Lock Tests
// ============================================================================

func TestManager_AddUnifiedLock(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	lock := &UnifiedLock{
		ID: "lock1",
		Owner: LockOwner{
			OwnerID:   "owner1",
			ClientID:  "client1",
			ShareName: "share1",
		},
		FileHandle: "file1",
		Offset:     0,
		Length:     100,
		Type:       LockTypeExclusive,
		AcquiredAt: time.Now(),
	}

	err := lm.AddUnifiedLock("file1", lock)
	if err != nil {
		t.Fatalf("AddUnifiedLock failed: %v", err)
	}

	locks := lm.ListUnifiedLocks("file1")
	if len(locks) != 1 {
		t.Fatalf("Expected 1 lock, got %d", len(locks))
	}
}

func TestManager_AddUnifiedLock_ConflictsWith(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	// Add exclusive lock
	lock1 := &UnifiedLock{
		ID:         "lock1",
		Owner:      LockOwner{OwnerID: "owner1"},
		FileHandle: "file1",
		Offset:     0,
		Length:     100,
		Type:       LockTypeExclusive,
	}
	if err := lm.AddUnifiedLock("file1", lock1); err != nil {
		t.Fatalf("First lock failed: %v", err)
	}

	// Conflicting exclusive from different owner
	lock2 := &UnifiedLock{
		ID:         "lock2",
		Owner:      LockOwner{OwnerID: "owner2"},
		FileHandle: "file1",
		Offset:     50,
		Length:     100,
		Type:       LockTypeExclusive,
	}
	err := lm.AddUnifiedLock("file1", lock2)
	if err == nil {
		t.Fatal("Expected conflict error from ConflictsWith")
	}
}

func TestManager_RemoveUnifiedLock(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	lock := &UnifiedLock{
		ID: "lock1",
		Owner: LockOwner{
			OwnerID:   "owner1",
			ClientID:  "client1",
			ShareName: "share1",
		},
		FileHandle: "file1",
		Offset:     0,
		Length:     100,
		Type:       LockTypeExclusive,
	}
	_ = lm.AddUnifiedLock("file1", lock)

	err := lm.RemoveUnifiedLock("file1", lock.Owner, 0, 100)
	if err != nil {
		t.Fatalf("RemoveUnifiedLock failed: %v", err)
	}

	locks := lm.ListUnifiedLocks("file1")
	if len(locks) != 0 {
		t.Fatalf("Expected 0 locks, got %d", len(locks))
	}
}

func TestManager_GetUnifiedLock(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	owner := LockOwner{OwnerID: "owner1", ClientID: "client1"}
	lock := &UnifiedLock{
		ID:         "lock1",
		Owner:      owner,
		FileHandle: "file1",
		Offset:     0,
		Length:     100,
		Type:       LockTypeExclusive,
	}
	_ = lm.AddUnifiedLock("file1", lock)

	// Found
	got, err := lm.GetUnifiedLock("file1", owner, 0, 100)
	if err != nil {
		t.Fatalf("GetUnifiedLock failed: %v", err)
	}
	if got.ID != "lock1" {
		t.Fatalf("Expected lock ID 'lock1', got '%s'", got.ID)
	}

	// Not found (wrong range)
	_, err = lm.GetUnifiedLock("file1", owner, 200, 100)
	if err == nil {
		t.Fatal("Expected error for non-existent lock")
	}
}

func TestManager_UpgradeLock(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	owner := LockOwner{
		OwnerID:   "owner1",
		ClientID:  "client1",
		ShareName: "share1",
	}

	// Add shared lock first
	sharedLock := &UnifiedLock{
		ID:         "lock1",
		Owner:      owner,
		FileHandle: "file1",
		Offset:     0,
		Length:     100,
		Type:       LockTypeShared,
	}
	_ = lm.AddUnifiedLock("file1", sharedLock)

	// Upgrade to exclusive
	upgraded, err := lm.UpgradeLock("file1", owner, 0, 100)
	if err != nil {
		t.Fatalf("UpgradeLock failed: %v", err)
	}

	if upgraded.Type != LockTypeExclusive {
		t.Fatalf("Expected exclusive lock, got %v", upgraded.Type)
	}
}

func TestManager_UpgradeLock_OtherReader(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	owner1 := LockOwner{OwnerID: "owner1"}
	owner2 := LockOwner{OwnerID: "owner2"}

	// Add shared locks from two owners
	_ = lm.AddUnifiedLock("file1", &UnifiedLock{
		ID:         "lock1",
		Owner:      owner1,
		FileHandle: "file1",
		Offset:     0,
		Length:     100,
		Type:       LockTypeShared,
	})
	_ = lm.AddUnifiedLock("file1", &UnifiedLock{
		ID:         "lock2",
		Owner:      owner2,
		FileHandle: "file1",
		Offset:     0,
		Length:     100,
		Type:       LockTypeShared,
	})

	// Upgrade should fail because owner2 has a lock
	_, err := lm.UpgradeLock("file1", owner1, 0, 100)
	if err == nil {
		t.Fatal("Expected upgrade to fail due to other reader")
	}
}

// ============================================================================
// Break Callbacks Tests
// ============================================================================

// mockBreakCallbacks implements BreakCallbacks for testing.
type mockBreakCallbacks struct {
	mu              sync.Mutex
	opLockBreaks    []opLockBreakEvent
	byteRangeBreaks []byteRangeRevokeEvent
	accessConflicts []accessConflictEvent
}

type opLockBreakEvent struct {
	handleKey    string
	ownerID      string
	breakToState uint32
}

type byteRangeRevokeEvent struct {
	handleKey string
	ownerID   string
	reason    string
}

type accessConflictEvent struct {
	handleKey     string
	ownerID       string
	requestedMode AccessMode
}

func (m *mockBreakCallbacks) OnOpLockBreak(handleKey string, lock *UnifiedLock, breakToState uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opLockBreaks = append(m.opLockBreaks, opLockBreakEvent{
		handleKey:    handleKey,
		ownerID:      lock.Owner.OwnerID,
		breakToState: breakToState,
	})
}

func (m *mockBreakCallbacks) OnByteRangeRevoke(handleKey string, lock *UnifiedLock, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byteRangeBreaks = append(m.byteRangeBreaks, byteRangeRevokeEvent{
		handleKey: handleKey,
		ownerID:   lock.Owner.OwnerID,
		reason:    reason,
	})
}

func (m *mockBreakCallbacks) OnAccessConflict(handleKey string, existingLock *UnifiedLock, requestedMode AccessMode) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accessConflicts = append(m.accessConflicts, accessConflictEvent{
		handleKey:     handleKey,
		ownerID:       existingLock.Owner.OwnerID,
		requestedMode: requestedMode,
	})
}

func (m *mockBreakCallbacks) getOpLockBreaks() []opLockBreakEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]opLockBreakEvent, len(m.opLockBreaks))
	copy(result, m.opLockBreaks)
	return result
}

func TestManager_RegisterBreakCallbacks(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	cb := &mockBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	stats := lm.GetStats()
	if stats.BreakCallbackCount != 1 {
		t.Fatalf("Expected 1 callback, got %d", stats.BreakCallbackCount)
	}
}

func TestManager_CheckAndBreakOpLocksForWrite_TriggersCallback(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	cb := &mockBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	// Add an oplock (RW lease)
	lock := &UnifiedLock{
		ID:    "lease1",
		Owner: LockOwner{OwnerID: "smb:client1", ClientID: "client1"},
		Lease: &OpLock{
			LeaseKey:   [16]byte{1},
			LeaseState: LeaseStateRead | LeaseStateWrite,
		},
	}
	_ = lm.AddUnifiedLock("file1", lock)

	// Write break should trigger callback
	err := lm.CheckAndBreakOpLocksForWrite("file1", nil)
	if err != nil {
		t.Fatalf("CheckAndBreakOpLocksForWrite failed: %v", err)
	}

	breaks := cb.getOpLockBreaks()
	if len(breaks) != 1 {
		t.Fatalf("Expected 1 break callback, got %d", len(breaks))
	}
	if breaks[0].breakToState != LeaseStateNone {
		t.Fatalf("Expected break to None, got %d", breaks[0].breakToState)
	}
	if breaks[0].ownerID != "smb:client1" {
		t.Fatalf("Expected owner 'smb:client1', got '%s'", breaks[0].ownerID)
	}
}

func TestManager_CheckAndBreakOpLocksForRead_OnlyBreaksWrite(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	cb := &mockBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	// Add a RW oplock (only one Write lease can exist per file)
	writeLock := &UnifiedLock{
		ID:    "lease1",
		Owner: LockOwner{OwnerID: "smb:writer", ClientID: "writer"},
		Lease: &OpLock{
			LeaseKey:   [16]byte{1},
			LeaseState: LeaseStateRead | LeaseStateWrite,
		},
	}
	if err := lm.AddUnifiedLock("file1", writeLock); err != nil {
		t.Fatalf("AddUnifiedLock failed: %v", err)
	}

	// Read break should break the Write oplock (downgrade to Read)
	_ = lm.CheckAndBreakOpLocksForRead("file1", nil)

	breaks := cb.getOpLockBreaks()
	if len(breaks) != 1 {
		t.Fatalf("Expected 1 break (Write oplock), got %d", len(breaks))
	}
	if breaks[0].ownerID != "smb:writer" {
		t.Fatalf("Expected writer break, got '%s'", breaks[0].ownerID)
	}
	if breaks[0].breakToState != LeaseStateRead {
		t.Fatalf("Expected break to Read, got %d", breaks[0].breakToState)
	}

	// Also verify: a Read-only oplock is NOT broken by a read
	lm2 := NewManager()
	cb2 := &mockBreakCallbacks{}
	lm2.RegisterBreakCallbacks(cb2)

	readLock := &UnifiedLock{
		ID:    "lease2",
		Owner: LockOwner{OwnerID: "smb:reader", ClientID: "reader"},
		Lease: &OpLock{
			LeaseKey:   [16]byte{2},
			LeaseState: LeaseStateRead,
		},
	}
	if err := lm2.AddUnifiedLock("file1", readLock); err != nil {
		t.Fatalf("AddUnifiedLock failed: %v", err)
	}

	_ = lm2.CheckAndBreakOpLocksForRead("file1", nil)

	breaks2 := cb2.getOpLockBreaks()
	if len(breaks2) != 0 {
		t.Fatalf("Expected 0 breaks for Read oplock during read, got %d", len(breaks2))
	}
}

func TestManager_CheckAndBreakOpLocksForDelete_BreaksAll(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	cb := &mockBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	// Add two Read oplocks from different owners (Read leases can coexist)
	if err := lm.AddUnifiedLock("file1", &UnifiedLock{
		ID:    "lease1",
		Owner: LockOwner{OwnerID: "smb:client1"},
		Lease: &OpLock{LeaseKey: [16]byte{1}, LeaseState: LeaseStateRead},
	}); err != nil {
		t.Fatalf("AddUnifiedLock lease1 failed: %v", err)
	}

	if err := lm.AddUnifiedLock("file1", &UnifiedLock{
		ID:    "lease2",
		Owner: LockOwner{OwnerID: "smb:client2"},
		Lease: &OpLock{LeaseKey: [16]byte{2}, LeaseState: LeaseStateRead},
	}); err != nil {
		t.Fatalf("AddUnifiedLock lease2 failed: %v", err)
	}

	// Delete breaks all oplocks
	_ = lm.CheckAndBreakOpLocksForDelete("file1", nil)

	breaks := cb.getOpLockBreaks()
	if len(breaks) != 2 {
		t.Fatalf("Expected 2 breaks (all oplocks), got %d", len(breaks))
	}
	for _, b := range breaks {
		if b.breakToState != LeaseStateNone {
			t.Fatalf("Expected all breaks to None, got %d", b.breakToState)
		}
	}
}

func TestManager_CheckAndBreakOpLocks_ExcludeOwner(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	cb := &mockBreakCallbacks{}
	lm.RegisterBreakCallbacks(cb)

	// Add two Read oplocks from different owners (Read leases can coexist)
	if err := lm.AddUnifiedLock("file1", &UnifiedLock{
		ID:    "lease1",
		Owner: LockOwner{OwnerID: "smb:client1"},
		Lease: &OpLock{LeaseKey: [16]byte{1}, LeaseState: LeaseStateRead},
	}); err != nil {
		t.Fatalf("AddUnifiedLock lease1 failed: %v", err)
	}
	if err := lm.AddUnifiedLock("file1", &UnifiedLock{
		ID:    "lease2",
		Owner: LockOwner{OwnerID: "smb:client2"},
		Lease: &OpLock{LeaseKey: [16]byte{2}, LeaseState: LeaseStateRead},
	}); err != nil {
		t.Fatalf("AddUnifiedLock lease2 failed: %v", err)
	}

	// Delete with exclude: should break client2 only
	excludeOwner := &LockOwner{OwnerID: "smb:client1"}
	_ = lm.CheckAndBreakOpLocksForDelete("file1", excludeOwner)

	breaks := cb.getOpLockBreaks()
	if len(breaks) != 1 {
		t.Fatalf("Expected 1 break (excluding client1), got %d", len(breaks))
	}
	if breaks[0].ownerID != "smb:client2" {
		t.Fatalf("Expected client2 break, got '%s'", breaks[0].ownerID)
	}
}

// ============================================================================
// RemoveAllLocks / RemoveClientLocks / GetStats Tests
// ============================================================================

func TestManager_RemoveAllLocks(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	// Add legacy lock
	_ = lm.Lock("file1", FileLock{SessionID: 100, Offset: 0, Length: 100, Exclusive: true})

	// Add unified lock
	_ = lm.AddUnifiedLock("file1", &UnifiedLock{
		ID:    "lock1",
		Owner: LockOwner{OwnerID: "owner1"},
		Type:  LockTypeExclusive,
	})

	lm.RemoveAllLocks("file1")

	if len(lm.ListLocks("file1")) != 0 {
		t.Fatal("Expected 0 legacy locks after RemoveAllLocks")
	}
	if len(lm.ListUnifiedLocks("file1")) != 0 {
		t.Fatal("Expected 0 unified locks after RemoveAllLocks")
	}
}

func TestManager_RemoveClientLocks(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	// Add shared lock for client1 on two files
	if err := lm.AddUnifiedLock("file1", &UnifiedLock{
		ID:     "lock1",
		Owner:  LockOwner{OwnerID: "owner1", ClientID: "client1"},
		Offset: 0,
		Length: 100,
		Type:   LockTypeShared,
	}); err != nil {
		t.Fatalf("AddUnifiedLock lock1 failed: %v", err)
	}
	if err := lm.AddUnifiedLock("file2", &UnifiedLock{
		ID:     "lock2",
		Owner:  LockOwner{OwnerID: "owner1", ClientID: "client1"},
		Offset: 0,
		Length: 100,
		Type:   LockTypeShared,
	}); err != nil {
		t.Fatalf("AddUnifiedLock lock2 failed: %v", err)
	}

	// Add shared lock for client2 on file1 (shared locks don't conflict)
	if err := lm.AddUnifiedLock("file1", &UnifiedLock{
		ID:     "lock3",
		Owner:  LockOwner{OwnerID: "owner2", ClientID: "client2"},
		Offset: 0,
		Length: 100,
		Type:   LockTypeShared,
	}); err != nil {
		t.Fatalf("AddUnifiedLock lock3 failed: %v", err)
	}

	lm.RemoveClientLocks("client1")

	// client1 locks should be gone
	if len(lm.ListUnifiedLocks("file1")) != 1 {
		t.Fatalf("Expected 1 lock on file1 (client2 only), got %d", len(lm.ListUnifiedLocks("file1")))
	}
	if len(lm.ListUnifiedLocks("file2")) != 0 {
		t.Fatalf("Expected 0 locks on file2, got %d", len(lm.ListUnifiedLocks("file2")))
	}
}

func TestManager_GetStats(t *testing.T) {
	t.Parallel()

	lm := NewManager()

	// Empty stats
	stats := lm.GetStats()
	if stats.TotalLegacyLocks != 0 || stats.TotalUnifiedLocks != 0 || stats.TotalFiles != 0 {
		t.Fatalf("Expected all zeros, got %+v", stats)
	}

	// Add some locks
	_ = lm.Lock("file1", FileLock{SessionID: 100, Offset: 0, Length: 100, Exclusive: true})
	_ = lm.AddUnifiedLock("file2", &UnifiedLock{
		ID:    "lock1",
		Owner: LockOwner{OwnerID: "owner1"},
		Type:  LockTypeExclusive,
	})

	stats = lm.GetStats()
	if stats.TotalLegacyLocks != 1 {
		t.Fatalf("Expected 1 legacy lock, got %d", stats.TotalLegacyLocks)
	}
	if stats.TotalUnifiedLocks != 1 {
		t.Fatalf("Expected 1 unified lock, got %d", stats.TotalUnifiedLocks)
	}
	if stats.TotalFiles != 2 {
		t.Fatalf("Expected 2 files with locks, got %d", stats.TotalFiles)
	}
}

// ============================================================================
// Grace Period Delegation Tests
// ============================================================================

func TestManager_GracePeriod_Delegation(t *testing.T) {
	t.Parallel()

	gpm := NewGracePeriodManager(1*time.Second, nil)
	lm := NewManagerWithGracePeriod(gpm)

	// Initially not in grace period
	if lm.IsInGracePeriod() {
		t.Fatal("Expected not in grace period initially")
	}

	// Enter grace period
	lm.EnterGracePeriod([]string{"client1"})
	if !lm.IsInGracePeriod() {
		t.Fatal("Expected in grace period after EnterGracePeriod")
	}

	// New locks should be denied
	allowed, err := lm.IsOperationAllowed(Operation{IsNew: true})
	if allowed {
		t.Fatal("Expected new lock to be denied during grace period")
	}
	if err == nil {
		t.Fatal("Expected grace period error")
	}

	// Reclaims should be allowed
	allowed, err = lm.IsOperationAllowed(Operation{IsReclaim: true})
	if !allowed {
		t.Fatal("Expected reclaim to be allowed during grace period")
	}
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Mark reclaimed and exit
	lm.MarkReclaimed("client1")
	// After all clients reclaim, grace period exits
	if lm.IsInGracePeriod() {
		t.Fatal("Expected grace period to exit after all clients reclaimed")
	}

	gpm.Close()
}

func TestManager_GracePeriod_NilManager(t *testing.T) {
	t.Parallel()

	lm := NewManager() // No grace period manager

	// All operations should be allowed
	lm.EnterGracePeriod([]string{"client1"}) // no-op
	if lm.IsInGracePeriod() {
		t.Fatal("Expected not in grace period without manager")
	}

	allowed, err := lm.IsOperationAllowed(Operation{IsNew: true})
	if !allowed || err != nil {
		t.Fatal("Expected all operations allowed without grace period manager")
	}

	lm.ExitGracePeriod()   // no-op
	lm.MarkReclaimed("x") // no-op
}

// ============================================================================
// Split Lock Tests
// ============================================================================

func TestSplitLock_ExactMatch(t *testing.T) {
	t.Parallel()

	lock := &UnifiedLock{
		ID:     "lock1",
		Offset: 0,
		Length: 100,
		Type:   LockTypeExclusive,
	}

	result := SplitLock(lock, 0, 100)
	if len(result) != 0 {
		t.Fatalf("Expected 0 locks after exact match unlock, got %d", len(result))
	}
}

func TestSplitLock_UnlockStart(t *testing.T) {
	t.Parallel()

	lock := &UnifiedLock{
		ID:     "lock1",
		Offset: 0,
		Length: 100,
		Type:   LockTypeExclusive,
	}

	result := SplitLock(lock, 0, 50)
	if len(result) != 1 {
		t.Fatalf("Expected 1 lock after unlock at start, got %d", len(result))
	}
	if result[0].Offset != 50 || result[0].Length != 50 {
		t.Fatalf("Expected lock [50-100], got [%d-%d]", result[0].Offset, result[0].Offset+result[0].Length)
	}
}

func TestSplitLock_UnlockEnd(t *testing.T) {
	t.Parallel()

	lock := &UnifiedLock{
		ID:     "lock1",
		Offset: 0,
		Length: 100,
		Type:   LockTypeExclusive,
	}

	result := SplitLock(lock, 50, 50)
	if len(result) != 1 {
		t.Fatalf("Expected 1 lock after unlock at end, got %d", len(result))
	}
	if result[0].Offset != 0 || result[0].Length != 50 {
		t.Fatalf("Expected lock [0-50], got [%d-%d]", result[0].Offset, result[0].Offset+result[0].Length)
	}
}

func TestSplitLock_UnlockMiddle(t *testing.T) {
	t.Parallel()

	lock := &UnifiedLock{
		ID:     "lock1",
		Offset: 0,
		Length: 100,
		Type:   LockTypeExclusive,
	}

	result := SplitLock(lock, 25, 50)
	if len(result) != 2 {
		t.Fatalf("Expected 2 locks after unlock in middle, got %d", len(result))
	}

	// Should have [0-25] and [75-100]
	if result[0].Offset != 0 || result[0].Length != 25 {
		t.Fatalf("Expected first lock [0-25], got [%d-%d]", result[0].Offset, result[0].Offset+result[0].Length)
	}
	if result[1].Offset != 75 || result[1].Length != 25 {
		t.Fatalf("Expected second lock [75-100], got [%d-%d]", result[1].Offset, result[1].Offset+result[1].Length)
	}
}

// ============================================================================
// Merge Lock Tests
// ============================================================================

func TestMergeLocks_Adjacent(t *testing.T) {
	t.Parallel()

	locks := []*UnifiedLock{
		{Owner: LockOwner{OwnerID: "o1"}, FileHandle: "f1", Offset: 0, Length: 50, Type: LockTypeExclusive},
		{Owner: LockOwner{OwnerID: "o1"}, FileHandle: "f1", Offset: 50, Length: 50, Type: LockTypeExclusive},
	}

	result := MergeLocks(locks)
	if len(result) != 1 {
		t.Fatalf("Expected 1 merged lock, got %d", len(result))
	}
	if result[0].Offset != 0 || result[0].Length != 100 {
		t.Fatalf("Expected merged lock [0-100], got [%d-%d]", result[0].Offset, result[0].Offset+result[0].Length)
	}
}

func TestMergeLocks_Overlapping(t *testing.T) {
	t.Parallel()

	locks := []*UnifiedLock{
		{Owner: LockOwner{OwnerID: "o1"}, FileHandle: "f1", Offset: 0, Length: 60, Type: LockTypeExclusive},
		{Owner: LockOwner{OwnerID: "o1"}, FileHandle: "f1", Offset: 40, Length: 60, Type: LockTypeExclusive},
	}

	result := MergeLocks(locks)
	if len(result) != 1 {
		t.Fatalf("Expected 1 merged lock, got %d", len(result))
	}
	if result[0].Offset != 0 || result[0].Length != 100 {
		t.Fatalf("Expected merged lock [0-100], got [%d-%d]", result[0].Offset, result[0].Offset+result[0].Length)
	}
}

func TestMergeLocks_DifferentOwners(t *testing.T) {
	t.Parallel()

	locks := []*UnifiedLock{
		{Owner: LockOwner{OwnerID: "o1"}, FileHandle: "f1", Offset: 0, Length: 50, Type: LockTypeExclusive},
		{Owner: LockOwner{OwnerID: "o2"}, FileHandle: "f1", Offset: 50, Length: 50, Type: LockTypeExclusive},
	}

	result := MergeLocks(locks)
	if len(result) != 2 {
		t.Fatalf("Expected 2 locks (different owners), got %d", len(result))
	}
}
