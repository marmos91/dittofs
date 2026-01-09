package metadata

import (
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
