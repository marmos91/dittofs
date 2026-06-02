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

// ============================================================================
// #470 C3 — Rename (dual-parent break + parent-key suppression)
// ============================================================================
//
// These tests mirror the Samba `dlt_renames` matrix
// (source4/torture/smb2/lease.c:7028) at the lock-manager layer. The C3 rename
// branch in internal/adapter/smb/handlers/set_info.go invokes
// BreakLeasesOnOpenConflict + BreakReadLeasesForParentDir on BOTH src-parent
// and dst-parent (RH → ""), each honoring the renamer's ClientID + parent-key
// suppression. We verify the underlying break path produces the right per-key
// break count for each matrix row.
//
// We exercise the dual call directly because the parent-key gating is the
// load-bearing rule (smbtorture rows: correct vs wrong vs no parent_key, in
// otherdir / samedir layout).

// breakBothParentsAsRename simulates the C3 post-rename break invocation:
// strip H then R on a parent handle, with parent-key suppression. Mirrors
// internal/adapter/smb/handlers/set_info.go::breakParentDirLeasesForContentChangeOn
// composed twice (src and dst).
func breakBothParentsAsRename(t *testing.T, mgr *Manager, srcParent, dstParent string, excludeClientID string, excludeKey [16]byte, hasKey bool) {
	t.Helper()
	owner := &LockOwner{
		ClientID:                    excludeClientID,
		ExcludeParentDirLeaseKey:    excludeKey,
		HasExcludeParentDirLeaseKey: hasKey,
	}
	for _, parent := range []string{srcParent, dstParent} {
		if parent == "" {
			continue
		}
		require.NoError(t, mgr.BreakLeasesOnOpenConflict(parent, owner, BreakReasonSharingViolation))
		require.NoError(t, mgr.BreakReadLeasesForParentDir(parent, owner))
	}
}

// TestRenameC3_OtherDir_NoParentKey_BreaksBoth covers
// dlt_renames "otherdir-no-parent-leaskey" (lease.c:7096): rename across two
// directories by a renamer without a parent_key — both src and dst dir leases
// must break.
func TestRenameC3_OtherDir_NoParentKey_BreaksBoth(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	srcLeaseKey := [16]byte{0x01}
	dstLeaseKey := [16]byte{0x02}

	breakKeys := map[string]int{}
	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lk *UnifiedLock, bts uint32) {
			breakKeys[handleKey]++
		},
	})

	// Two dir leases held by a "second client" on separate parent handles.
	_, _, err := mgr.RequestLease(ctx, FileHandle("srcdir"), srcLeaseKey, [16]byte{},
		"owner-src", "clientA", "/share", LeaseStateRead|LeaseStateHandle, true)
	require.NoError(t, err)
	_, _, err = mgr.RequestLease(ctx, FileHandle("dstdir"), dstLeaseKey, [16]byte{},
		"owner-dst", "clientA", "/share", LeaseStateRead|LeaseStateHandle, true)
	require.NoError(t, err)

	// Renamer is a third client with no parent_key.
	breakBothParentsAsRename(t, mgr, "srcdir", "dstdir", "clientB", [16]byte{}, false)

	assert.GreaterOrEqual(t, breakKeys["srcdir"], 1, "src dir lease must break when renamer carries no matching parent_key")
	assert.GreaterOrEqual(t, breakKeys["dstdir"], 1, "dst dir lease must break when renamer carries no matching parent_key")
}

// TestRenameC3_OtherDir_CorrectSrcParentKey_SuppressesSrc covers
// "otherdir-correct-srcparent-leaskey" (lease.c:7063): renamer's parent_key
// matches src-parent's lease → src suppressed, dst breaks.
func TestRenameC3_OtherDir_CorrectSrcParentKey_SuppressesSrc(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	srcLeaseKey := [16]byte{0x01}
	dstLeaseKey := [16]byte{0x02}

	breakKeys := map[string]int{}
	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lk *UnifiedLock, bts uint32) {
			breakKeys[handleKey]++
		},
	})

	_, _, err := mgr.RequestLease(ctx, FileHandle("srcdir"), srcLeaseKey, [16]byte{},
		"owner-src", "clientA", "/share", LeaseStateRead|LeaseStateHandle, true)
	require.NoError(t, err)
	_, _, err = mgr.RequestLease(ctx, FileHandle("dstdir"), dstLeaseKey, [16]byte{},
		"owner-dst", "clientA", "/share", LeaseStateRead|LeaseStateHandle, true)
	require.NoError(t, err)

	// Renamer carries src-parent's lease key as its own parent_key.
	breakBothParentsAsRename(t, mgr, "srcdir", "dstdir", "clientB", srcLeaseKey, true)

	assert.Equal(t, 0, breakKeys["srcdir"], "src dir lease must be suppressed by matching parent_key (#470 C3)")
	assert.GreaterOrEqual(t, breakKeys["dstdir"], 1, "dst dir lease must still break when parent_key matches src only")
}

// TestRenameC3_OtherDir_CorrectDstParentKey_SuppressesDst covers
// "otherdir-correct-dstparent-leaskey" (lease.c:7074): symmetric case where
// renamer's parent_key matches dst-parent's lease → dst suppressed, src breaks.
func TestRenameC3_OtherDir_CorrectDstParentKey_SuppressesDst(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	srcLeaseKey := [16]byte{0x01}
	dstLeaseKey := [16]byte{0x02}

	breakKeys := map[string]int{}
	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lk *UnifiedLock, bts uint32) {
			breakKeys[handleKey]++
		},
	})

	_, _, err := mgr.RequestLease(ctx, FileHandle("srcdir"), srcLeaseKey, [16]byte{},
		"owner-src", "clientA", "/share", LeaseStateRead|LeaseStateHandle, true)
	require.NoError(t, err)
	_, _, err = mgr.RequestLease(ctx, FileHandle("dstdir"), dstLeaseKey, [16]byte{},
		"owner-dst", "clientA", "/share", LeaseStateRead|LeaseStateHandle, true)
	require.NoError(t, err)

	breakBothParentsAsRename(t, mgr, "srcdir", "dstdir", "clientB", dstLeaseKey, true)

	assert.GreaterOrEqual(t, breakKeys["srcdir"], 1, "src dir lease must still break when parent_key matches dst only")
	assert.Equal(t, 0, breakKeys["dstdir"], "dst dir lease must be suppressed by matching parent_key (#470 C3)")
}

// TestRenameC3_SameDir_CorrectParentKey_NoBreak covers
// "samedir-correct-parent-leaskey" (lease.c:7030): same-directory rename by a
// renamer carrying the dir's parent_key → no break at all. The handler-level
// code de-dupes the (src == dst) case so the helper fires exactly once on the
// single dir lease, which is then suppressed by the parent-key match.
func TestRenameC3_SameDir_CorrectParentKey_NoBreak(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	dirLeaseKey := [16]byte{0x01}

	breakKeys := map[string]int{}
	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lk *UnifiedLock, bts uint32) {
			breakKeys[handleKey]++
		},
	})

	_, _, err := mgr.RequestLease(ctx, FileHandle("dir1"), dirLeaseKey, [16]byte{},
		"owner-d", "clientA", "/share", LeaseStateRead|LeaseStateHandle, true)
	require.NoError(t, err)

	// Same-dir rename: handler passes srcParent == dstParent; the helper de-dupes
	// to a single break invocation. Renamer's parent_key matches → suppressed.
	breakBothParentsAsRename(t, mgr, "dir1", "", "clientB", dirLeaseKey, true)

	assert.Equal(t, 0, breakKeys["dir1"], "same-dir rename with matching parent_key must not break (samedir-correct-parent-leaskey)")
}

// TestRenameC3_SameDir_WrongParentKey_Breaks covers
// "samedir-wrong-parent-leaskey" (lease.c:7041): renamer carries a parent_key
// that does NOT match the dir's lease key → break fires.
func TestRenameC3_SameDir_WrongParentKey_Breaks(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	dirLeaseKey := [16]byte{0x01}
	wrongKey := [16]byte{0x03}

	breakKeys := map[string]int{}
	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lk *UnifiedLock, bts uint32) {
			breakKeys[handleKey]++
		},
	})

	_, _, err := mgr.RequestLease(ctx, FileHandle("dir1"), dirLeaseKey, [16]byte{},
		"owner-d", "clientA", "/share", LeaseStateRead|LeaseStateHandle, true)
	require.NoError(t, err)

	breakBothParentsAsRename(t, mgr, "dir1", "", "clientB", wrongKey, true)

	assert.GreaterOrEqual(t, breakKeys["dir1"], 1, "same-dir rename with wrong parent_key must break (samedir-wrong-parent-leaskey)")
}

// TestRenameC3_DstParent_PreConflictBreak_OnlyStripsHandle covers the
// rename_dst_parent first-phase semantics (lease.c:7331). The dst-parent
// dir-lease holder must receive an H-strip break (RH → R) before the
// SHARING_VIOLATION return — this is the BreakReasonSharingViolation path the
// new breakDstParentDirHandleLeasesForRename helper takes. Read caching is
// preserved (no R-strip happens at this stage).
func TestRenameC3_DstParent_PreConflictBreak_OnlyStripsHandle(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	ctx := context.Background()
	dstLeaseKey := [16]byte{0x01}

	var lastBreakTo uint32
	var breakCount int
	mgr.RegisterBreakCallbacks(&testBreakCallbacks{
		onOpLockBreak: func(handleKey string, lk *UnifiedLock, bts uint32) {
			breakCount++
			lastBreakTo = bts
		},
	})

	_, _, err := mgr.RequestLease(ctx, FileHandle("dstdir"), dstLeaseKey, [16]byte{},
		"owner-d", "clientA", "/share", LeaseStateRead|LeaseStateHandle, true)
	require.NoError(t, err)

	// Renamer (clientB) carries no parent_key → break fires; reason
	// SharingViolation strips H, preserves R (RH → R).
	require.NoError(t, mgr.BreakLeasesOnOpenConflict("dstdir",
		&LockOwner{ClientID: "clientB"},
		BreakReasonSharingViolation,
	))

	assert.Equal(t, 1, breakCount, "dst-parent dir lease must break exactly once on the pre-conflict path")
	assert.Equal(t, uint32(LeaseStateRead), lastBreakTo, "pre-conflict dst-parent break must strip H but preserve R (RH → R, rename_dst_parent Phase 1)")
}
