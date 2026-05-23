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
	mgr.OnDirChange(FileHandle("dir1"), DirChangeAddEntry, "client2", [16]byte{}, false)

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
	mgr.OnDirChange(FileHandle("dir1"), DirChangeAddEntry, "client1", [16]byte{}, false)

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
	mgr.OnDirChange(FileHandle("dir1"), DirChangeAddEntry, "client2", [16]byte{}, false)

	assert.False(t, breakCalled, "should not break file leases on dir change")
}

// ============================================================================
// Parent-Key Suppression Tests (#470 C2)
// ============================================================================
//
// Mirrors Samba `smbd_dirlease.c::dirlease_should_break`:
//   - hasExcludeKey=true + LeaseKey == excludeParentLeaseKey + IsDirectory ⇒
//     suppress the break.
//   - Any other combination ⇒ break.
//
// The matrix below approximates `test_dirlease_setinfo` 1.1/2.1/3.1/2.2/2.3
// at the lock-layer boundary. Test names follow the format
//   {Correct,Bad,No}Key + {SameClient,SecondClient}
// matching the 6 sub-cases per setinfo type that the smbtorture matrix runs.

func TestOnDirChange_ParentKey_CorrectKeySameClient_Suppresses(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	dirLeaseKey := [16]byte{0xA, 0xB, 0xC, 0xD}

	var breakCount int
	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lock *UnifiedLock, bts uint32) {
			breakCount++
		},
	})

	_, _, err := mgr.RequestLease(ctx, FileHandle("dir1"), dirLeaseKey, [16]byte{},
		"owner1", "client1", "/share", LeaseStateRead|LeaseStateHandle, true)
	require.NoError(t, err)

	// "Correct parent_key" path: same client and ParentLeaseKey matches the
	// dir lease's LeaseKey ⇒ no break (Samba 1.1 sub-case).
	mgr.OnDirChange(FileHandle("dir1"), DirChangeAddEntry, "client1", dirLeaseKey, true)

	assert.Equal(t, 0, breakCount, "correct parent_key on same client must suppress the dir lease break")
}

func TestOnDirChange_ParentKey_CorrectKeySecondClient_Suppresses(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	dirLeaseKey := [16]byte{0xA, 0xB, 0xC, 0xD}

	var breakCount int
	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lock *UnifiedLock, bts uint32) {
			breakCount++
		},
	})

	_, _, err := mgr.RequestLease(ctx, FileHandle("dir1"), dirLeaseKey, [16]byte{},
		"owner1", "client1", "/share", LeaseStateRead|LeaseStateHandle, true)
	require.NoError(t, err)

	// Second-client path: even though origin differs, parent_key matches ⇒
	// suppress (Samba 2.1 sub-case).
	mgr.OnDirChange(FileHandle("dir1"), DirChangeAddEntry, "client2", dirLeaseKey, true)

	assert.Equal(t, 0, breakCount, "correct parent_key on second client must suppress the dir lease break")
}

func TestOnDirChange_ParentKey_BadKeySameClient_Breaks(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	dirLeaseKey := [16]byte{0xA, 0xB, 0xC, 0xD}
	badKey := [16]byte{0xDE, 0xAD, 0xBE, 0xEF}

	var breakCount int
	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lock *UnifiedLock, bts uint32) {
			breakCount++
		},
	})

	// Use a different client so the originClient skip doesn't mask the
	// parent-key path. The "same client" name refers to Samba's matrix
	// terminology for "the setinfo is on the same handle that holds a
	// parent_key linkage" — but bad parent_key still breaks (Samba 1.2).
	_, _, err := mgr.RequestLease(ctx, FileHandle("dir1"), dirLeaseKey, [16]byte{},
		"owner1", "client1", "/share", LeaseStateRead|LeaseStateHandle, true)
	require.NoError(t, err)

	mgr.OnDirChange(FileHandle("dir1"), DirChangeAddEntry, "client2", badKey, true)

	assert.Equal(t, 1, breakCount, "bad parent_key must NOT suppress the break")
}

func TestOnDirChange_ParentKey_NoKey_Breaks(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	dirLeaseKey := [16]byte{0xA, 0xB, 0xC, 0xD}

	var breakCount int
	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lock *UnifiedLock, bts uint32) {
			breakCount++
		},
	})

	_, _, err := mgr.RequestLease(ctx, FileHandle("dir1"), dirLeaseKey, [16]byte{},
		"owner1", "client1", "/share", LeaseStateRead|LeaseStateHandle, true)
	require.NoError(t, err)

	// hasExcludeKey=false: the (zero) key value is ignored and the break
	// fires (Samba 1.3, 2.3 sub-cases — child CREATE had no parent linkage).
	mgr.OnDirChange(FileHandle("dir1"), DirChangeAddEntry, "client2", [16]byte{}, false)

	assert.Equal(t, 1, breakCount, "absence of parent_key linkage must NOT suppress the break")
}

// TestOnDirChange_ParentKey_DoesNotSuppressFileLeases enforces the critical
// invariant from #470 plan §4 risk callout #1: parent-key suppression must
// apply to dir leases ONLY. A coincidental key collision with a file lease
// must not suppress an otherwise-required file-lease break. (File leases on
// the dir handle itself are unusual but possible — and OnDirChange already
// ignores them via the IsDirectory gate; this test pins that the new
// suppression branch does NOT regress that gate.)
func TestOnDirChange_ParentKey_DoesNotSuppressFileLeases(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	fileLeaseKey := [16]byte{0xA, 0xB, 0xC, 0xD}

	var breakCount int
	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lock *UnifiedLock, bts uint32) {
			breakCount++
		},
	})

	// Grant a FILE lease (isDirectory=false) on the handle.
	_, _, err := mgr.RequestLease(ctx, FileHandle("file1"), fileLeaseKey, [16]byte{},
		"owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite, false)
	require.NoError(t, err)

	// File leases are already ignored by OnDirChange (the IsDirectory gate)
	// regardless of parent-key. This test asserts the new branch does not
	// flip that contract — break count stays at 0 because the lease is a
	// file lease, not because of parent-key suppression.
	mgr.OnDirChange(FileHandle("file1"), DirChangeAddEntry, "client2", fileLeaseKey, true)

	assert.Equal(t, 0, breakCount, "file leases are not broken on dir change regardless of parent-key")
}

// TestBreakOpLocks_ParentKey_DoesNotSuppressFileLeases covers the same
// invariant for the BreakParent* family path (used by SET_INFO /
// breakParentDirLeasesForContentChange). The break helper goes through
// breakOpLocks with HasExcludeParentDirLeaseKey=true; it must NOT match
// against file leases that coincidentally share the parent_key value.
func TestBreakOpLocks_ParentKey_DoesNotSuppressFileLeases(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	collidingKey := [16]byte{0xA, 0xB, 0xC, 0xD}

	var breakCount int
	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lock *UnifiedLock, bts uint32) {
			breakCount++
		},
	})

	// Grant a FILE lease (isDirectory=false) on the same handle.
	_, _, err := mgr.RequestLease(ctx, FileHandle("file1"), collidingKey, [16]byte{},
		"owner1", "client1", "/share", LeaseStateRead|LeaseStateHandle, false)
	require.NoError(t, err)

	// Drive a break with HasExcludeParentDirLeaseKey set to the colliding key
	// (e.g. as if BreakParentHandleLeasesOnCreate was invoked with this
	// parent-key value). The break path's `shouldBreak` predicate triggers
	// (the lease has R+H caching). The new suppression branch is gated on
	// IsDirectory so it must NOT skip this file lease.
	require.NoError(t, mgr.BreakLeasesOnOpenConflict(
		"file1",
		&LockOwner{
			ClientID:                    "client2",
			ExcludeParentDirLeaseKey:    collidingKey,
			HasExcludeParentDirLeaseKey: true,
		},
		BreakReasonSharingViolation,
	))

	assert.Equal(t, 1, breakCount, "parent-key suppression must NOT apply to file leases (#470 plan §4 risk #1)")
}

// TestBreakOpLocks_ParentKey_SuppressesDirLeaseOnly verifies the
// BreakParent* path consumes ExcludeParentDirLeaseKey to skip the dir lease
// holding that key. This is the direct path exercised by
// breakParentDirLeasesForContentChange (set_info / write / close-on-delete).
func TestBreakOpLocks_ParentKey_SuppressesDirLeaseOnly(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	dirLeaseKey := [16]byte{0xA, 0xB, 0xC, 0xD}

	var breakCount int
	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lock *UnifiedLock, bts uint32) {
			breakCount++
		},
	})

	// Grant a DIR lease.
	_, _, err := mgr.RequestLease(ctx, FileHandle("dir1"), dirLeaseKey, [16]byte{},
		"owner1", "client1", "/share", LeaseStateRead|LeaseStateHandle, true)
	require.NoError(t, err)

	// Suppression with matching key: no break.
	require.NoError(t, mgr.BreakLeasesOnOpenConflict(
		"dir1",
		&LockOwner{
			ClientID:                    "client2",
			ExcludeParentDirLeaseKey:    dirLeaseKey,
			HasExcludeParentDirLeaseKey: true,
		},
		BreakReasonSharingViolation,
	))
	assert.Equal(t, 0, breakCount, "matching parent_key must suppress the dir lease break")

	// Suppression with non-matching key: break fires.
	require.NoError(t, mgr.BreakLeasesOnOpenConflict(
		"dir1",
		&LockOwner{
			ClientID:                    "client2",
			ExcludeParentDirLeaseKey:    [16]byte{0xFF},
			HasExcludeParentDirLeaseKey: true,
		},
		BreakReasonSharingViolation,
	))
	assert.Equal(t, 1, breakCount, "non-matching parent_key must NOT suppress the dir lease break")
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
	mgr.OnDirChange(FileHandle("dir1"), DirChangeAddEntry, "client2", [16]byte{}, false)

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
