package lock

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// DirChangeNotifier Interface Tests
// ============================================================================

func TestDirChangeNotifier_Interface(t *testing.T) {
	t.Parallel()

	// Verify Manager satisfies DirChangeNotifier at compile time
	var _ DirChangeNotifier = (*Manager)(nil)
}

func TestOnDirChange_BreaksDirectoryLeases(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	// Track break callbacks
	var breakCalled bool
	var breakHandleKey string
	var breakToState uint32
	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lock *UnifiedLock, bts uint32) {
			breakCalled = true
			breakHandleKey = handleKey
			breakToState = bts
			// Manager already set Breaking=true before dispatching
		},
	})

	// Grant directory lease (RH is valid for directories)
	state, _, err := mgr.RequestLease(ctx, FileHandle("dir1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateHandle, true)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateRead|LeaseStateHandle, state)

	// Simulate directory change from a different client
	mgr.OnDirChange(FileHandle("dir1"), DirChangeAddEntry, "client2")

	assert.True(t, breakCalled, "break callback should have been called")
	assert.Equal(t, "dir1", breakHandleKey)
	assert.Equal(t, LeaseStateNone, breakToState, "directory lease should break to None")
}

func TestOnDirChange_ExcludesOriginClient(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	var breakCalled bool
	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lock *UnifiedLock, bts uint32) {
			breakCalled = true
		},
	})

	// Grant directory lease
	_, _, err := mgr.RequestLease(ctx, FileHandle("dir1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead, true)
	require.NoError(t, err)

	// Dir change from same client - should NOT break
	mgr.OnDirChange(FileHandle("dir1"), DirChangeAddEntry, "client1")

	assert.False(t, breakCalled, "should not break own client's lease")
}

func TestOnDirChange_IgnoresFileLeases(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	leaseKey := [16]byte{1, 2, 3}
	parentKey := [16]byte{}

	var breakCalled bool
	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lock *UnifiedLock, bts uint32) {
			breakCalled = true
		},
	})

	// Grant FILE lease (not directory)
	_, _, err := mgr.RequestLease(ctx, FileHandle("dir1"), leaseKey, parentKey, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite, false)
	require.NoError(t, err)

	// Dir change - should NOT break file leases
	mgr.OnDirChange(FileHandle("dir1"), DirChangeAddEntry, "client2")

	assert.False(t, breakCalled, "should not break file leases on dir change")
}

// ============================================================================
// Recently-Broken Cache Tests
// ============================================================================

func TestRecentlyBrokenCache_BlocksDirectoryLease(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	key1 := [16]byte{1, 0, 0, 0}
	key2 := [16]byte{2, 0, 0, 0}
	parentKey := [16]byte{}

	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lock *UnifiedLock, bts uint32) {
			// Manager already set Breaking=true before dispatching
		},
	})

	// Grant directory lease
	_, _, err := mgr.RequestLease(ctx, FileHandle("dir1"), key1, parentKey, "owner1", "client1", "/share", LeaseStateRead, true)
	require.NoError(t, err)

	// Trigger dir change (marks as recently-broken)
	mgr.OnDirChange(FileHandle("dir1"), DirChangeAddEntry, "client2")

	// Immediately request new directory lease on same dir - should be blocked by recently-broken cache
	state, _, err := mgr.RequestLease(ctx, FileHandle("dir1"), key2, parentKey, "owner2", "client2", "/share", LeaseStateRead, true)
	require.NoError(t, err)
	assert.Equal(t, LeaseStateNone, state, "recently-broken directory should not get new lease")
}

// ============================================================================
// DirChangeType Constants Tests
// ============================================================================

func TestDirChangeType_Constants(t *testing.T) {
	t.Parallel()

	// Verify the constants exist and are distinct
	assert.NotEqual(t, DirChangeAddEntry, DirChangeRemoveEntry)
	assert.NotEqual(t, DirChangeAddEntry, DirChangeRenameEntry)
	assert.NotEqual(t, DirChangeRemoveEntry, DirChangeRenameEntry)
}

// ============================================================================
// recentlyBrokenCache unit tests
// ============================================================================

func TestRecentlyBrokenCache_IsRecentlyBroken(t *testing.T) {
	t.Parallel()

	cache := newRecentlyBrokenCache(5 * time.Second)

	// Not broken yet
	assert.False(t, cache.IsRecentlyBroken("dir1"))

	// Mark as broken
	cache.Mark("dir1")
	assert.True(t, cache.IsRecentlyBroken("dir1"))

	// Different key not broken
	assert.False(t, cache.IsRecentlyBroken("dir2"))
}

func TestRecentlyBrokenCache_Expiry(t *testing.T) {
	t.Parallel()

	// Use very short TTL for testing
	cache := newRecentlyBrokenCache(10 * time.Millisecond)

	cache.Mark("dir1")
	assert.True(t, cache.IsRecentlyBroken("dir1"))

	// Wait for expiry
	time.Sleep(20 * time.Millisecond)
	assert.False(t, cache.IsRecentlyBroken("dir1"), "should expire after TTL")
}
