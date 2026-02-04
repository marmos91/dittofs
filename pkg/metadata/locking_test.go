package metadata

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// RangesOverlap Tests
// ============================================================================

func TestRangesOverlap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		offset1, length1 uint64
		offset2, length2 uint64
		want             bool
	}{
		// Non-overlapping cases
		{"disjoint ranges", 0, 10, 20, 10, false},
		{"adjacent ranges no overlap", 0, 10, 10, 10, false},
		{"reversed adjacent", 10, 10, 0, 10, false},

		// Overlapping cases
		{"partial overlap", 0, 10, 5, 10, true},
		{"one contains other", 0, 20, 5, 5, true},
		{"same range", 0, 10, 0, 10, true},
		{"overlap at boundary", 0, 11, 10, 10, true},

		// Unbounded range cases (length=0 means "to EOF")
		{"both unbounded", 0, 0, 100, 0, true},
		{"first unbounded overlaps bounded", 0, 0, 100, 10, true},
		{"first unbounded after bounded start", 50, 0, 0, 10, false},
		{"second unbounded overlaps bounded", 0, 10, 5, 0, true},
		{"second unbounded after bounded", 0, 10, 100, 0, false},
		{"unbounded from zero overlaps all", 0, 0, 1000000, 1, true},

		// Edge cases
		{"zero length bounded at same offset", 10, 5, 10, 5, true},
		{"single byte ranges overlap", 5, 1, 5, 1, true},
		{"single byte ranges adjacent", 5, 1, 6, 1, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := RangesOverlap(tt.offset1, tt.length1, tt.offset2, tt.length2)
			assert.Equal(t, tt.want, got)

			// Test symmetry: overlap(a,b) == overlap(b,a)
			gotReverse := RangesOverlap(tt.offset2, tt.length2, tt.offset1, tt.length1)
			assert.Equal(t, tt.want, gotReverse, "overlap should be symmetric")
		})
	}
}

// ============================================================================
// IsLockConflicting Tests
// ============================================================================

func TestIsLockConflicting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		existing *FileLock
		request  *FileLock
		want     bool
	}{
		{
			name:     "same session no conflict",
			existing: &FileLock{SessionID: 1, Offset: 0, Length: 10, Exclusive: true},
			request:  &FileLock{SessionID: 1, Offset: 0, Length: 10, Exclusive: true},
			want:     false,
		},
		{
			name:     "different session non-overlapping no conflict",
			existing: &FileLock{SessionID: 1, Offset: 0, Length: 10, Exclusive: true},
			request:  &FileLock{SessionID: 2, Offset: 20, Length: 10, Exclusive: true},
			want:     false,
		},
		{
			name:     "both shared no conflict",
			existing: &FileLock{SessionID: 1, Offset: 0, Length: 10, Exclusive: false},
			request:  &FileLock{SessionID: 2, Offset: 0, Length: 10, Exclusive: false},
			want:     false,
		},
		{
			name:     "existing exclusive request shared conflict",
			existing: &FileLock{SessionID: 1, Offset: 0, Length: 10, Exclusive: true},
			request:  &FileLock{SessionID: 2, Offset: 5, Length: 10, Exclusive: false},
			want:     true,
		},
		{
			name:     "existing shared request exclusive conflict",
			existing: &FileLock{SessionID: 1, Offset: 0, Length: 10, Exclusive: false},
			request:  &FileLock{SessionID: 2, Offset: 5, Length: 10, Exclusive: true},
			want:     true,
		},
		{
			name:     "both exclusive overlapping conflict",
			existing: &FileLock{SessionID: 1, Offset: 0, Length: 10, Exclusive: true},
			request:  &FileLock{SessionID: 2, Offset: 5, Length: 10, Exclusive: true},
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := IsLockConflicting(tt.existing, tt.request)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ============================================================================
// CheckIOConflict Tests
// ============================================================================

func TestCheckIOConflict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		lock      *FileLock
		sessionID uint64
		offset    uint64
		length    uint64
		isWrite   bool
		want      bool
	}{
		{
			name:      "same session no conflict for write",
			lock:      &FileLock{SessionID: 1, Offset: 0, Length: 10, Exclusive: true},
			sessionID: 1,
			offset:    0, length: 10, isWrite: true,
			want: false,
		},
		{
			name:      "same session no conflict for read",
			lock:      &FileLock{SessionID: 1, Offset: 0, Length: 10, Exclusive: true},
			sessionID: 1,
			offset:    0, length: 10, isWrite: false,
			want: false,
		},
		{
			name:      "non-overlapping no conflict",
			lock:      &FileLock{SessionID: 1, Offset: 0, Length: 10, Exclusive: true},
			sessionID: 2,
			offset:    20, length: 10, isWrite: true,
			want: false,
		},
		{
			name:      "read vs shared lock no conflict",
			lock:      &FileLock{SessionID: 1, Offset: 0, Length: 10, Exclusive: false},
			sessionID: 2,
			offset:    0, length: 10, isWrite: false,
			want: false,
		},
		{
			name:      "read vs exclusive lock conflict",
			lock:      &FileLock{SessionID: 1, Offset: 0, Length: 10, Exclusive: true},
			sessionID: 2,
			offset:    0, length: 10, isWrite: false,
			want: true,
		},
		{
			name:      "write vs shared lock conflict",
			lock:      &FileLock{SessionID: 1, Offset: 0, Length: 10, Exclusive: false},
			sessionID: 2,
			offset:    5, length: 10, isWrite: true,
			want: true,
		},
		{
			name:      "write vs exclusive lock conflict",
			lock:      &FileLock{SessionID: 1, Offset: 0, Length: 10, Exclusive: true},
			sessionID: 2,
			offset:    5, length: 10, isWrite: true,
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := CheckIOConflict(tt.lock, tt.sessionID, tt.offset, tt.length, tt.isWrite)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ============================================================================
// LockManager Tests
// ============================================================================

func TestNewLockManager(t *testing.T) {
	t.Parallel()

	lm := NewLockManager()

	require.NotNil(t, lm)
	require.NotNil(t, lm.locks)
}

func TestLockManager_Lock(t *testing.T) {
	t.Parallel()

	t.Run("acquires lock successfully", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()

		err := lm.Lock("file1", FileLock{
			SessionID: 1,
			Offset:    0,
			Length:    100,
			Exclusive: true,
		})

		assert.NoError(t, err)

		// Verify lock was added
		locks := lm.ListLocks("file1")
		require.Len(t, locks, 1)
		assert.Equal(t, uint64(1), locks[0].SessionID)
	})

	t.Run("sets AcquiredAt timestamp", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()
		before := time.Now()

		err := lm.Lock("file1", FileLock{
			SessionID: 1,
			Offset:    0,
			Length:    100,
			Exclusive: true,
		})

		require.NoError(t, err)
		locks := lm.ListLocks("file1")
		require.Len(t, locks, 1)
		assert.True(t, locks[0].AcquiredAt.After(before) || locks[0].AcquiredAt.Equal(before))
	})

	t.Run("returns error on conflict", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()

		// First lock
		err := lm.Lock("file1", FileLock{
			SessionID: 1,
			Offset:    0,
			Length:    100,
			Exclusive: true,
		})
		require.NoError(t, err)

		// Conflicting lock from different session
		err = lm.Lock("file1", FileLock{
			SessionID: 2,
			Offset:    50,
			Length:    100,
			Exclusive: true,
		})

		assert.Error(t, err)
		var storeErr *StoreError
		require.ErrorAs(t, err, &storeErr)
		assert.Equal(t, ErrLocked, storeErr.Code)
	})

	t.Run("same session can update existing lock", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()

		// First lock (shared)
		err := lm.Lock("file1", FileLock{
			SessionID: 1,
			Offset:    0,
			Length:    100,
			Exclusive: false,
		})
		require.NoError(t, err)

		// Update to exclusive (same session, same range)
		err = lm.Lock("file1", FileLock{
			SessionID: 1,
			Offset:    0,
			Length:    100,
			Exclusive: true,
		})
		require.NoError(t, err)

		// Verify only one lock exists and it's exclusive
		locks := lm.ListLocks("file1")
		require.Len(t, locks, 1)
		assert.True(t, locks[0].Exclusive)
	})

	t.Run("multiple non-conflicting locks", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()

		// Shared lock 1
		err := lm.Lock("file1", FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: false})
		require.NoError(t, err)

		// Shared lock 2 (shared locks don't conflict)
		err = lm.Lock("file1", FileLock{SessionID: 2, Offset: 0, Length: 100, Exclusive: false})
		require.NoError(t, err)

		locks := lm.ListLocks("file1")
		assert.Len(t, locks, 2)
	})
}

func TestLockManager_Unlock(t *testing.T) {
	t.Parallel()

	t.Run("unlocks existing lock", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()

		// Add lock
		err := lm.Lock("file1", FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true})
		require.NoError(t, err)

		// Unlock
		err = lm.Unlock("file1", 1, 0, 100)
		assert.NoError(t, err)

		// Verify removed
		locks := lm.ListLocks("file1")
		assert.Nil(t, locks)
	})

	t.Run("returns error for non-existent lock", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()

		err := lm.Unlock("file1", 1, 0, 100)

		assert.Error(t, err)
		var storeErr *StoreError
		require.ErrorAs(t, err, &storeErr)
		assert.Equal(t, ErrLockNotFound, storeErr.Code)
	})

	t.Run("returns error for wrong session", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()

		// Add lock for session 1
		err := lm.Lock("file1", FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true})
		require.NoError(t, err)

		// Try to unlock with session 2
		err = lm.Unlock("file1", 2, 0, 100)

		assert.Error(t, err)
		var storeErr *StoreError
		require.ErrorAs(t, err, &storeErr)
		assert.Equal(t, ErrLockNotFound, storeErr.Code)

		// Original lock should still exist
		locks := lm.ListLocks("file1")
		assert.Len(t, locks, 1)
	})

	t.Run("cleans up empty entries", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()

		// Add and remove lock
		err := lm.Lock("file1", FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true})
		require.NoError(t, err)

		err = lm.Unlock("file1", 1, 0, 100)
		require.NoError(t, err)

		// Verify internal map is cleaned up
		lm.mu.RLock()
		_, exists := lm.locks["file1"]
		lm.mu.RUnlock()
		assert.False(t, exists)
	})
}

func TestLockManager_UnlockAllForSession(t *testing.T) {
	t.Parallel()

	t.Run("removes all session locks", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()

		// Add multiple locks for session 1
		_ = lm.Lock("file1", FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true})
		_ = lm.Lock("file1", FileLock{SessionID: 1, Offset: 200, Length: 100, Exclusive: true})

		count := lm.UnlockAllForSession("file1", 1)

		assert.Equal(t, 2, count)
		locks := lm.ListLocks("file1")
		assert.Nil(t, locks)
	})

	t.Run("leaves other sessions locks", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()

		// Add locks for different sessions (non-overlapping)
		_ = lm.Lock("file1", FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true})
		_ = lm.Lock("file1", FileLock{SessionID: 2, Offset: 200, Length: 100, Exclusive: true})

		count := lm.UnlockAllForSession("file1", 1)

		assert.Equal(t, 1, count)
		locks := lm.ListLocks("file1")
		require.Len(t, locks, 1)
		assert.Equal(t, uint64(2), locks[0].SessionID)
	})

	t.Run("returns zero for non-existent file", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()

		count := lm.UnlockAllForSession("nonexistent", 1)

		assert.Equal(t, 0, count)
	})

	t.Run("returns zero for session with no locks", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()

		_ = lm.Lock("file1", FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true})

		count := lm.UnlockAllForSession("file1", 999)

		assert.Equal(t, 0, count)
	})
}

func TestLockManager_TestLock(t *testing.T) {
	t.Parallel()

	t.Run("returns true when no conflict", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()

		ok, conflict := lm.TestLock("file1", 1, 0, 100, true)

		assert.True(t, ok)
		assert.Nil(t, conflict)
	})

	t.Run("returns true when same session", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()
		_ = lm.Lock("file1", FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true})

		ok, conflict := lm.TestLock("file1", 1, 0, 100, true)

		assert.True(t, ok)
		assert.Nil(t, conflict)
	})

	t.Run("returns conflict details on conflict", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()
		_ = lm.Lock("file1", FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true})

		ok, conflict := lm.TestLock("file1", 2, 50, 100, true)

		assert.False(t, ok)
		require.NotNil(t, conflict)
		assert.Equal(t, uint64(0), conflict.Offset)
		assert.Equal(t, uint64(100), conflict.Length)
		assert.True(t, conflict.Exclusive)
		assert.Equal(t, uint64(1), conflict.OwnerSessionID)
	})
}

func TestLockManager_CheckForIO(t *testing.T) {
	t.Parallel()

	t.Run("returns nil when no conflict", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()

		conflict := lm.CheckForIO("file1", 1, 0, 100, true)

		assert.Nil(t, conflict)
	})

	t.Run("returns nil for same session", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()
		_ = lm.Lock("file1", FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true})

		conflict := lm.CheckForIO("file1", 1, 0, 100, true)

		assert.Nil(t, conflict)
	})

	t.Run("returns conflict for blocked write", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()
		_ = lm.Lock("file1", FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: false})

		conflict := lm.CheckForIO("file1", 2, 50, 50, true)

		require.NotNil(t, conflict)
		assert.Equal(t, uint64(1), conflict.OwnerSessionID)
	})

	t.Run("returns conflict for blocked read", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()
		_ = lm.Lock("file1", FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true})

		conflict := lm.CheckForIO("file1", 2, 50, 50, false)

		require.NotNil(t, conflict)
		assert.Equal(t, uint64(1), conflict.OwnerSessionID)
	})
}

func TestLockManager_ListLocks(t *testing.T) {
	t.Parallel()

	t.Run("returns nil for file with no locks", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()

		locks := lm.ListLocks("nonexistent")

		assert.Nil(t, locks)
	})

	t.Run("returns copy of locks", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()
		_ = lm.Lock("file1", FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true})

		locks := lm.ListLocks("file1")

		require.Len(t, locks, 1)

		// Modify the returned slice
		locks[0].SessionID = 999

		// Original should be unchanged
		original := lm.ListLocks("file1")
		assert.Equal(t, uint64(1), original[0].SessionID)
	})
}

func TestLockManager_RemoveFileLocks(t *testing.T) {
	t.Parallel()

	t.Run("removes all locks for file", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()
		_ = lm.Lock("file1", FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true})
		_ = lm.Lock("file1", FileLock{SessionID: 2, Offset: 200, Length: 100, Exclusive: true})

		lm.RemoveFileLocks("file1")

		locks := lm.ListLocks("file1")
		assert.Nil(t, locks)
	})

	t.Run("no-op for non-existent file", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()

		// Should not panic
		lm.RemoveFileLocks("nonexistent")
	})

	t.Run("leaves other files' locks", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()
		_ = lm.Lock("file1", FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true})
		_ = lm.Lock("file2", FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true})

		lm.RemoveFileLocks("file1")

		assert.Nil(t, lm.ListLocks("file1"))
		assert.Len(t, lm.ListLocks("file2"), 1)
	})
}

// ============================================================================
// Enhanced Lock Type Tests
// ============================================================================

func TestEnhancedLock_Basics(t *testing.T) {
	t.Parallel()

	t.Run("NewEnhancedLock creates lock with UUID", func(t *testing.T) {
		t.Parallel()
		owner := LockOwner{OwnerID: "nlm:client1:pid123", ClientID: "client1", ShareName: "/export"}
		lock := NewEnhancedLock(owner, FileHandle("file1"), 0, 100, LockTypeExclusive)

		require.NotEmpty(t, lock.ID)
		assert.Equal(t, owner.OwnerID, lock.Owner.OwnerID)
		assert.Equal(t, FileHandle("file1"), lock.FileHandle)
		assert.Equal(t, uint64(0), lock.Offset)
		assert.Equal(t, uint64(100), lock.Length)
		assert.Equal(t, LockTypeExclusive, lock.Type)
		assert.True(t, lock.IsExclusive())
		assert.False(t, lock.IsShared())
		assert.False(t, lock.AcquiredAt.IsZero())
	})

	t.Run("IsShared and IsExclusive", func(t *testing.T) {
		t.Parallel()
		owner := LockOwner{OwnerID: "test"}
		shared := NewEnhancedLock(owner, FileHandle("file"), 0, 100, LockTypeShared)
		exclusive := NewEnhancedLock(owner, FileHandle("file"), 0, 100, LockTypeExclusive)

		assert.True(t, shared.IsShared())
		assert.False(t, shared.IsExclusive())
		assert.True(t, exclusive.IsExclusive())
		assert.False(t, exclusive.IsShared())
	})

	t.Run("End returns correct value", func(t *testing.T) {
		t.Parallel()
		owner := LockOwner{OwnerID: "test"}

		bounded := NewEnhancedLock(owner, FileHandle("file"), 10, 90, LockTypeShared)
		assert.Equal(t, uint64(100), bounded.End())

		unbounded := NewEnhancedLock(owner, FileHandle("file"), 10, 0, LockTypeShared)
		assert.Equal(t, uint64(0), unbounded.End())
	})

	t.Run("Contains checks range containment", func(t *testing.T) {
		t.Parallel()
		owner := LockOwner{OwnerID: "test"}
		lock := NewEnhancedLock(owner, FileHandle("file"), 10, 90, LockTypeShared) // bytes 10-99

		assert.True(t, lock.Contains(10, 90))   // Exact match
		assert.True(t, lock.Contains(20, 50))   // Fully inside
		assert.True(t, lock.Contains(10, 1))    // At start
		assert.True(t, lock.Contains(99, 1))    // At end
		assert.False(t, lock.Contains(0, 10))   // Before
		assert.False(t, lock.Contains(100, 10)) // After
		assert.False(t, lock.Contains(5, 100))  // Overlaps but not contained
	})

	t.Run("Overlaps checks range overlap", func(t *testing.T) {
		t.Parallel()
		owner := LockOwner{OwnerID: "test"}
		lock := NewEnhancedLock(owner, FileHandle("file"), 10, 90, LockTypeShared) // bytes 10-99

		assert.True(t, lock.Overlaps(10, 90))   // Exact match
		assert.True(t, lock.Overlaps(0, 20))    // Overlaps at start
		assert.True(t, lock.Overlaps(90, 20))   // Overlaps at end
		assert.True(t, lock.Overlaps(0, 200))   // Contains lock
		assert.False(t, lock.Overlaps(0, 10))   // Adjacent before
		assert.False(t, lock.Overlaps(100, 10)) // Adjacent after
	})

	t.Run("Clone creates independent copy", func(t *testing.T) {
		t.Parallel()
		owner := LockOwner{OwnerID: "test", ClientID: "client1", ShareName: "/export"}
		original := NewEnhancedLock(owner, FileHandle("file"), 10, 90, LockTypeShared)
		original.Blocking = true
		original.Reclaim = true

		clone := original.Clone()

		// Verify all fields copied
		assert.Equal(t, original.ID, clone.ID)
		assert.Equal(t, original.Owner.OwnerID, clone.Owner.OwnerID)
		assert.Equal(t, original.FileHandle, clone.FileHandle)
		assert.Equal(t, original.Offset, clone.Offset)
		assert.Equal(t, original.Length, clone.Length)
		assert.Equal(t, original.Type, clone.Type)
		assert.Equal(t, original.Blocking, clone.Blocking)
		assert.Equal(t, original.Reclaim, clone.Reclaim)

		// Verify independence
		clone.Offset = 999
		assert.NotEqual(t, original.Offset, clone.Offset)
	})
}

func TestLockType_String(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "shared", LockTypeShared.String())
	assert.Equal(t, "exclusive", LockTypeExclusive.String())
	assert.Equal(t, "unknown", LockType(99).String())
}

func TestShareReservation_String(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "none", ShareReservationNone.String())
	assert.Equal(t, "deny-read", ShareReservationDenyRead.String())
	assert.Equal(t, "deny-write", ShareReservationDenyWrite.String())
	assert.Equal(t, "deny-all", ShareReservationDenyAll.String())
	assert.Equal(t, "unknown", ShareReservation(99).String())
}

// ============================================================================
// POSIX Lock Splitting Tests
// ============================================================================

func TestSplitLock(t *testing.T) {
	t.Parallel()

	owner := LockOwner{OwnerID: "test", ClientID: "client1", ShareName: "/export"}

	t.Run("unlock exact match removes lock", func(t *testing.T) {
		t.Parallel()
		lock := NewEnhancedLock(owner, FileHandle("file"), 0, 100, LockTypeExclusive)

		result := SplitLock(lock, 0, 100)

		assert.Len(t, result, 0)
	})

	t.Run("unlock at start creates one lock", func(t *testing.T) {
		t.Parallel()
		lock := NewEnhancedLock(owner, FileHandle("file"), 0, 100, LockTypeExclusive)

		result := SplitLock(lock, 0, 50)

		require.Len(t, result, 1)
		assert.Equal(t, uint64(50), result[0].Offset)
		assert.Equal(t, uint64(50), result[0].Length)
	})

	t.Run("unlock at end creates one lock", func(t *testing.T) {
		t.Parallel()
		lock := NewEnhancedLock(owner, FileHandle("file"), 0, 100, LockTypeExclusive)

		result := SplitLock(lock, 50, 50)

		require.Len(t, result, 1)
		assert.Equal(t, uint64(0), result[0].Offset)
		assert.Equal(t, uint64(50), result[0].Length)
	})

	t.Run("unlock in middle creates two locks", func(t *testing.T) {
		t.Parallel()
		lock := NewEnhancedLock(owner, FileHandle("file"), 0, 100, LockTypeExclusive)

		result := SplitLock(lock, 25, 50) // Unlock bytes 25-74

		require.Len(t, result, 2)
		// First part: 0-24
		assert.Equal(t, uint64(0), result[0].Offset)
		assert.Equal(t, uint64(25), result[0].Length)
		// Second part: 75-99
		assert.Equal(t, uint64(75), result[1].Offset)
		assert.Equal(t, uint64(25), result[1].Length)
	})

	t.Run("unlock larger than lock removes lock", func(t *testing.T) {
		t.Parallel()
		lock := NewEnhancedLock(owner, FileHandle("file"), 10, 80, LockTypeExclusive)

		result := SplitLock(lock, 0, 100) // Unlock covers entire lock

		assert.Len(t, result, 0)
	})

	t.Run("unlock non-overlapping preserves lock", func(t *testing.T) {
		t.Parallel()
		lock := NewEnhancedLock(owner, FileHandle("file"), 0, 100, LockTypeExclusive)

		result := SplitLock(lock, 200, 50) // Unlock doesn't overlap

		require.Len(t, result, 1)
		assert.Equal(t, uint64(0), result[0].Offset)
		assert.Equal(t, uint64(100), result[0].Length)
	})

	t.Run("unlock preserves lock properties", func(t *testing.T) {
		t.Parallel()
		lock := NewEnhancedLock(owner, FileHandle("file"), 0, 100, LockTypeExclusive)
		lock.Blocking = true
		lock.Reclaim = true
		lock.ShareReservation = ShareReservationDenyWrite

		result := SplitLock(lock, 25, 50)

		require.Len(t, result, 2)
		for _, r := range result {
			assert.Equal(t, lock.Owner.OwnerID, r.Owner.OwnerID)
			assert.Equal(t, lock.Type, r.Type)
			assert.Equal(t, lock.Blocking, r.Blocking)
			assert.Equal(t, lock.Reclaim, r.Reclaim)
			assert.Equal(t, lock.ShareReservation, r.ShareReservation)
		}
	})

	t.Run("unlock unbounded lock at start", func(t *testing.T) {
		t.Parallel()
		lock := NewEnhancedLock(owner, FileHandle("file"), 0, 0, LockTypeExclusive) // 0 to EOF

		result := SplitLock(lock, 0, 100) // Unlock first 100 bytes

		require.Len(t, result, 1)
		assert.Equal(t, uint64(100), result[0].Offset)
		assert.Equal(t, uint64(0), result[0].Length) // Still unbounded
	})

	t.Run("unlock with unbounded range", func(t *testing.T) {
		t.Parallel()
		lock := NewEnhancedLock(owner, FileHandle("file"), 0, 100, LockTypeExclusive)

		result := SplitLock(lock, 50, 0) // Unlock from 50 to EOF

		require.Len(t, result, 1)
		assert.Equal(t, uint64(0), result[0].Offset)
		assert.Equal(t, uint64(50), result[0].Length)
	})
}

// ============================================================================
// Lock Merging Tests
// ============================================================================

func TestMergeLocks(t *testing.T) {
	t.Parallel()

	owner := LockOwner{OwnerID: "test", ClientID: "client1", ShareName: "/export"}

	t.Run("empty input returns nil", func(t *testing.T) {
		t.Parallel()
		result := MergeLocks(nil)
		assert.Nil(t, result)

		result = MergeLocks([]*EnhancedLock{})
		assert.Nil(t, result)
	})

	t.Run("single lock unchanged", func(t *testing.T) {
		t.Parallel()
		lock := NewEnhancedLock(owner, FileHandle("file"), 0, 100, LockTypeExclusive)

		result := MergeLocks([]*EnhancedLock{lock})

		require.Len(t, result, 1)
		assert.Equal(t, uint64(0), result[0].Offset)
		assert.Equal(t, uint64(100), result[0].Length)
	})

	t.Run("adjacent locks merge", func(t *testing.T) {
		t.Parallel()
		lock1 := NewEnhancedLock(owner, FileHandle("file"), 0, 50, LockTypeExclusive)
		lock2 := NewEnhancedLock(owner, FileHandle("file"), 50, 50, LockTypeExclusive)

		result := MergeLocks([]*EnhancedLock{lock1, lock2})

		require.Len(t, result, 1)
		assert.Equal(t, uint64(0), result[0].Offset)
		assert.Equal(t, uint64(100), result[0].Length)
	})

	t.Run("overlapping locks merge", func(t *testing.T) {
		t.Parallel()
		lock1 := NewEnhancedLock(owner, FileHandle("file"), 0, 60, LockTypeExclusive)
		lock2 := NewEnhancedLock(owner, FileHandle("file"), 40, 60, LockTypeExclusive)

		result := MergeLocks([]*EnhancedLock{lock1, lock2})

		require.Len(t, result, 1)
		assert.Equal(t, uint64(0), result[0].Offset)
		assert.Equal(t, uint64(100), result[0].Length)
	})

	t.Run("non-adjacent locks stay separate", func(t *testing.T) {
		t.Parallel()
		lock1 := NewEnhancedLock(owner, FileHandle("file"), 0, 40, LockTypeExclusive)
		lock2 := NewEnhancedLock(owner, FileHandle("file"), 60, 40, LockTypeExclusive)

		result := MergeLocks([]*EnhancedLock{lock1, lock2})

		require.Len(t, result, 2)
	})

	t.Run("different owners stay separate", func(t *testing.T) {
		t.Parallel()
		owner1 := LockOwner{OwnerID: "owner1"}
		owner2 := LockOwner{OwnerID: "owner2"}
		lock1 := NewEnhancedLock(owner1, FileHandle("file"), 0, 50, LockTypeExclusive)
		lock2 := NewEnhancedLock(owner2, FileHandle("file"), 50, 50, LockTypeExclusive)

		result := MergeLocks([]*EnhancedLock{lock1, lock2})

		require.Len(t, result, 2)
	})

	t.Run("different types stay separate", func(t *testing.T) {
		t.Parallel()
		lock1 := NewEnhancedLock(owner, FileHandle("file"), 0, 50, LockTypeShared)
		lock2 := NewEnhancedLock(owner, FileHandle("file"), 50, 50, LockTypeExclusive)

		result := MergeLocks([]*EnhancedLock{lock1, lock2})

		require.Len(t, result, 2)
	})

	t.Run("different files stay separate", func(t *testing.T) {
		t.Parallel()
		lock1 := NewEnhancedLock(owner, FileHandle("file1"), 0, 50, LockTypeExclusive)
		lock2 := NewEnhancedLock(owner, FileHandle("file2"), 50, 50, LockTypeExclusive)

		result := MergeLocks([]*EnhancedLock{lock1, lock2})

		require.Len(t, result, 2)
	})

	t.Run("merge with unbounded lock", func(t *testing.T) {
		t.Parallel()
		lock1 := NewEnhancedLock(owner, FileHandle("file"), 0, 50, LockTypeExclusive)
		lock2 := NewEnhancedLock(owner, FileHandle("file"), 50, 0, LockTypeExclusive) // Unbounded

		result := MergeLocks([]*EnhancedLock{lock1, lock2})

		require.Len(t, result, 1)
		assert.Equal(t, uint64(0), result[0].Offset)
		assert.Equal(t, uint64(0), result[0].Length) // Result is unbounded
	})
}

// ============================================================================
// Atomic Lock Upgrade Tests
// ============================================================================

func TestUpgradeLock_Atomic_NoOtherReaders(t *testing.T) {
	t.Parallel()

	lm := NewLockManager()
	owner := LockOwner{OwnerID: "owner1", ClientID: "client1", ShareName: "/export"}

	// Add a shared lock
	sharedLock := NewEnhancedLock(owner, FileHandle("file1"), 0, 100, LockTypeShared)
	err := lm.AddEnhancedLock("file1", sharedLock)
	require.NoError(t, err)

	// Upgrade to exclusive - should succeed
	upgraded, err := lm.UpgradeLock("file1", owner, 0, 100)
	require.NoError(t, err)
	require.NotNil(t, upgraded)
	assert.Equal(t, LockTypeExclusive, upgraded.Type)
	assert.Equal(t, owner.OwnerID, upgraded.Owner.OwnerID)
}

func TestUpgradeLock_Fails_WithOtherReaders(t *testing.T) {
	t.Parallel()

	lm := NewLockManager()
	ownerA := LockOwner{OwnerID: "ownerA", ClientID: "clientA", ShareName: "/export"}
	ownerB := LockOwner{OwnerID: "ownerB", ClientID: "clientB", ShareName: "/export"}

	// Add shared locks from both owners
	lockA := NewEnhancedLock(ownerA, FileHandle("file1"), 0, 100, LockTypeShared)
	lockB := NewEnhancedLock(ownerB, FileHandle("file1"), 0, 100, LockTypeShared)

	err := lm.AddEnhancedLock("file1", lockA)
	require.NoError(t, err)
	err = lm.AddEnhancedLock("file1", lockB)
	require.NoError(t, err)

	// Owner A tries to upgrade - should fail because owner B has a shared lock
	upgraded, err := lm.UpgradeLock("file1", ownerA, 0, 100)
	require.Error(t, err)
	assert.Nil(t, upgraded)
	assert.True(t, IsLockConflictError(err))
}

func TestUpgradeLock_NoExistingLock(t *testing.T) {
	t.Parallel()

	lm := NewLockManager()
	owner := LockOwner{OwnerID: "owner1", ClientID: "client1", ShareName: "/export"}

	// Try to upgrade non-existent lock
	upgraded, err := lm.UpgradeLock("file1", owner, 0, 100)
	require.Error(t, err)
	assert.Nil(t, upgraded)

	var storeErr *StoreError
	require.ErrorAs(t, err, &storeErr)
	assert.Equal(t, ErrLockNotFound, storeErr.Code)
}

func TestUpgradeLock_AlreadyExclusive(t *testing.T) {
	t.Parallel()

	lm := NewLockManager()
	owner := LockOwner{OwnerID: "owner1", ClientID: "client1", ShareName: "/export"}

	// Add an exclusive lock
	exclusiveLock := NewEnhancedLock(owner, FileHandle("file1"), 0, 100, LockTypeExclusive)
	err := lm.AddEnhancedLock("file1", exclusiveLock)
	require.NoError(t, err)

	// "Upgrade" already exclusive lock - should succeed as no-op
	upgraded, err := lm.UpgradeLock("file1", owner, 0, 100)
	require.NoError(t, err)
	require.NotNil(t, upgraded)
	assert.Equal(t, LockTypeExclusive, upgraded.Type)
}

func TestUpgradeLock_PartialOverlap(t *testing.T) {
	t.Parallel()

	lm := NewLockManager()
	ownerA := LockOwner{OwnerID: "ownerA", ClientID: "clientA", ShareName: "/export"}
	ownerB := LockOwner{OwnerID: "ownerB", ClientID: "clientB", ShareName: "/export"}

	// Owner A has shared lock on 0-100
	lockA := NewEnhancedLock(ownerA, FileHandle("file1"), 0, 100, LockTypeShared)
	err := lm.AddEnhancedLock("file1", lockA)
	require.NoError(t, err)

	// Owner B has shared lock on 50-150 (partial overlap)
	lockB := NewEnhancedLock(ownerB, FileHandle("file1"), 50, 100, LockTypeShared)
	err = lm.AddEnhancedLock("file1", lockB)
	require.NoError(t, err)

	// Owner A tries to upgrade overlapping range - should fail
	upgraded, err := lm.UpgradeLock("file1", ownerA, 0, 100)
	require.Error(t, err)
	assert.Nil(t, upgraded)
	assert.True(t, IsLockConflictError(err))
}

// ============================================================================
// Cross-Protocol Lock Conflict Tests (LOCK-04)
// ============================================================================

func TestLockConflict_CrossProtocol(t *testing.T) {
	t.Parallel()

	lm := NewLockManager()

	// NLM owner
	nlmOwner := LockOwner{OwnerID: "nlm:client1:pid123", ClientID: "client1", ShareName: "/export"}
	// SMB owner
	smbOwner := LockOwner{OwnerID: "smb:session456:pid789", ClientID: "client2", ShareName: "/export"}

	// NLM client gets exclusive lock first
	nlmLock := NewEnhancedLock(nlmOwner, FileHandle("file1"), 0, 100, LockTypeExclusive)
	err := lm.AddEnhancedLock("file1", nlmLock)
	require.NoError(t, err)

	// SMB client tries to get exclusive lock on same range - should conflict
	smbLock := NewEnhancedLock(smbOwner, FileHandle("file1"), 0, 100, LockTypeExclusive)
	err = lm.AddEnhancedLock("file1", smbLock)
	require.Error(t, err)
	assert.True(t, IsLockConflictError(err))
}

func TestOwnerID_TreatedAsOpaque(t *testing.T) {
	t.Parallel()

	// Test that owner IDs are compared as opaque strings, not parsed

	t.Run("same protocol different clients conflict", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()

		owner1 := LockOwner{OwnerID: "nlm:client1:pid123"}
		owner2 := LockOwner{OwnerID: "nlm:client2:pid456"}

		lock1 := NewEnhancedLock(owner1, FileHandle("file1"), 0, 100, LockTypeExclusive)
		err := lm.AddEnhancedLock("file1", lock1)
		require.NoError(t, err)

		lock2 := NewEnhancedLock(owner2, FileHandle("file1"), 0, 100, LockTypeExclusive)
		err = lm.AddEnhancedLock("file1", lock2)
		require.Error(t, err)
		assert.True(t, IsLockConflictError(err))
	})

	t.Run("same owner string no conflict", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()

		// Exact same owner ID - even from "different" protocol instances
		owner1 := LockOwner{OwnerID: "protocol:id:123", ClientID: "different1"}
		owner2 := LockOwner{OwnerID: "protocol:id:123", ClientID: "different2"}

		lock1 := NewEnhancedLock(owner1, FileHandle("file1"), 0, 100, LockTypeExclusive)
		err := lm.AddEnhancedLock("file1", lock1)
		require.NoError(t, err)

		// Same OwnerID = same logical owner = no conflict
		lock2 := NewEnhancedLock(owner2, FileHandle("file1"), 0, 100, LockTypeExclusive)
		err = lm.AddEnhancedLock("file1", lock2)
		require.NoError(t, err) // Updates existing lock
	})

	t.Run("shared locks across protocols compatible", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()

		nlmOwner := LockOwner{OwnerID: "nlm:client1:pid123"}
		smbOwner := LockOwner{OwnerID: "smb:session456:pid789"}
		nfs4Owner := LockOwner{OwnerID: "nfs4:clientid:stateid"}

		// All three protocols get shared locks on same range - should succeed
		nlmLock := NewEnhancedLock(nlmOwner, FileHandle("file1"), 0, 100, LockTypeShared)
		err := lm.AddEnhancedLock("file1", nlmLock)
		require.NoError(t, err)

		smbLock := NewEnhancedLock(smbOwner, FileHandle("file1"), 0, 100, LockTypeShared)
		err = lm.AddEnhancedLock("file1", smbLock)
		require.NoError(t, err)

		nfs4Lock := NewEnhancedLock(nfs4Owner, FileHandle("file1"), 0, 100, LockTypeShared)
		err = lm.AddEnhancedLock("file1", nfs4Lock)
		require.NoError(t, err)

		// Verify all three locks exist
		locks := lm.ListEnhancedLocks("file1")
		assert.Len(t, locks, 3)
	})
}

// ============================================================================
// IsEnhancedLockConflicting Tests
// ============================================================================

func TestIsEnhancedLockConflicting(t *testing.T) {
	t.Parallel()

	owner1 := LockOwner{OwnerID: "owner1"}
	owner2 := LockOwner{OwnerID: "owner2"}

	tests := []struct {
		name     string
		existing *EnhancedLock
		request  *EnhancedLock
		want     bool
	}{
		{
			name:     "same owner no conflict",
			existing: &EnhancedLock{Owner: owner1, Offset: 0, Length: 100, Type: LockTypeExclusive},
			request:  &EnhancedLock{Owner: owner1, Offset: 0, Length: 100, Type: LockTypeExclusive},
			want:     false,
		},
		{
			name:     "different owner non-overlapping no conflict",
			existing: &EnhancedLock{Owner: owner1, Offset: 0, Length: 100, Type: LockTypeExclusive},
			request:  &EnhancedLock{Owner: owner2, Offset: 200, Length: 100, Type: LockTypeExclusive},
			want:     false,
		},
		{
			name:     "both shared no conflict",
			existing: &EnhancedLock{Owner: owner1, Offset: 0, Length: 100, Type: LockTypeShared},
			request:  &EnhancedLock{Owner: owner2, Offset: 0, Length: 100, Type: LockTypeShared},
			want:     false,
		},
		{
			name:     "existing exclusive request shared conflict",
			existing: &EnhancedLock{Owner: owner1, Offset: 0, Length: 100, Type: LockTypeExclusive},
			request:  &EnhancedLock{Owner: owner2, Offset: 50, Length: 100, Type: LockTypeShared},
			want:     true,
		},
		{
			name:     "existing shared request exclusive conflict",
			existing: &EnhancedLock{Owner: owner1, Offset: 0, Length: 100, Type: LockTypeShared},
			request:  &EnhancedLock{Owner: owner2, Offset: 50, Length: 100, Type: LockTypeExclusive},
			want:     true,
		},
		{
			name:     "both exclusive overlapping conflict",
			existing: &EnhancedLock{Owner: owner1, Offset: 0, Length: 100, Type: LockTypeExclusive},
			request:  &EnhancedLock{Owner: owner2, Offset: 50, Length: 100, Type: LockTypeExclusive},
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := IsEnhancedLockConflicting(tt.existing, tt.request)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ============================================================================
// Enhanced Lock Manager Tests
// ============================================================================

func TestLockManager_EnhancedLocks(t *testing.T) {
	t.Parallel()

	t.Run("AddEnhancedLock and ListEnhancedLocks", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()
		owner := LockOwner{OwnerID: "owner1"}

		lock := NewEnhancedLock(owner, FileHandle("file1"), 0, 100, LockTypeExclusive)
		err := lm.AddEnhancedLock("file1", lock)
		require.NoError(t, err)

		locks := lm.ListEnhancedLocks("file1")
		require.Len(t, locks, 1)
		assert.Equal(t, owner.OwnerID, locks[0].Owner.OwnerID)
	})

	t.Run("RemoveEnhancedLock with POSIX splitting", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()
		owner := LockOwner{OwnerID: "owner1"}

		lock := NewEnhancedLock(owner, FileHandle("file1"), 0, 100, LockTypeExclusive)
		err := lm.AddEnhancedLock("file1", lock)
		require.NoError(t, err)

		// Unlock middle portion - should split
		err = lm.RemoveEnhancedLock("file1", owner, 25, 50)
		require.NoError(t, err)

		locks := lm.ListEnhancedLocks("file1")
		require.Len(t, locks, 2)
	})

	t.Run("RemoveEnhancedFileLocks", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()
		owner := LockOwner{OwnerID: "owner1"}

		lock := NewEnhancedLock(owner, FileHandle("file1"), 0, 100, LockTypeExclusive)
		err := lm.AddEnhancedLock("file1", lock)
		require.NoError(t, err)

		lm.RemoveEnhancedFileLocks("file1")

		locks := lm.ListEnhancedLocks("file1")
		assert.Nil(t, locks)
	})

	t.Run("ListEnhancedLocks returns copy", func(t *testing.T) {
		t.Parallel()
		lm := NewLockManager()
		owner := LockOwner{OwnerID: "owner1"}

		lock := NewEnhancedLock(owner, FileHandle("file1"), 0, 100, LockTypeExclusive)
		err := lm.AddEnhancedLock("file1", lock)
		require.NoError(t, err)

		locks := lm.ListEnhancedLocks("file1")
		locks[0].Offset = 9999

		// Original should be unchanged
		original := lm.ListEnhancedLocks("file1")
		assert.Equal(t, uint64(0), original[0].Offset)
	})
}

// ============================================================================
// Concurrency Tests
// ============================================================================

func TestLockManager_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	lm := NewLockManager()
	const numGoroutines = 10
	const numOpsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			owner := LockOwner{OwnerID: string(rune('A' + id))}

			for j := 0; j < numOpsPerGoroutine; j++ {
				// Each goroutine works on its own file to avoid conflicts
				fileKey := string(rune('A' + id))
				lock := NewEnhancedLock(owner, FileHandle(fileKey), uint64(j*10), 10, LockTypeExclusive)

				_ = lm.AddEnhancedLock(fileKey, lock)
				_ = lm.ListEnhancedLocks(fileKey)
				_ = lm.RemoveEnhancedLock(fileKey, owner, uint64(j*10), 10)
			}
		}(i)
	}

	wg.Wait()
}
