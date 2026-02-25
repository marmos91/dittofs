package lock

import (
	"testing"
	"time"
)

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

	// Same session - should succeed
	ok, conflict := lm.TestLock("file1", 100, 50, 50, true)
	if !ok {
		t.Fatal("Expected test lock to succeed for same session")
	}
	if conflict != nil {
		t.Fatal("Expected no conflict")
	}

	// Different session - should fail
	ok, conflict = lm.TestLock("file1", 200, 50, 50, true)
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
// Enhanced Lock Tests
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
