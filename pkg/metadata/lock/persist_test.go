package lock

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLock_PersistsAndReloads verifies that a byte-range lock acquired via
// Lock() is persisted to the lock store and can be reloaded into a fresh
// manager after a simulated restart.
func TestLock_PersistsAndReloads(t *testing.T) {
	ctx := context.Background()
	store := newMockLockStore()

	mgr := NewManager()
	mgr.SetLockStore(store)
	mgr.SetShareName("share-a")

	const handleKey = "share-a:file-1"
	fl := FileLock{
		ID:        42,
		SessionID: 7,
		OpenID:    "open-1",
		Offset:    100,
		Length:    50,
		Exclusive: true,
	}
	require.NoError(t, mgr.Lock(handleKey, fl))

	// The lock must be persisted with its share name so the per-share
	// recovery query (LockQuery{ShareName}) matches it on restart.
	persisted, err := store.ListLocks(ctx, LockQuery{ShareName: "share-a"})
	require.NoError(t, err)
	require.Len(t, persisted, 1, "byte-range lock should be persisted under its share")

	// Simulate restart: fresh manager, restore from the store.
	fresh := NewManager()
	fresh.SetLockStore(store)
	require.NoError(t, fresh.RestoreLocks(persisted))

	// SMB byte-range locks restore into the legacy locks map (consulted by
	// Lock/Unlock/TestLock/CheckForIO), not the unified-lock map.
	restored := fresh.ListLocks(handleKey)
	require.Len(t, restored, 1, "lock should be present after restore")
	require.Equal(t, uint64(100), restored[0].Offset)
	require.Equal(t, uint64(50), restored[0].Length)
}

// TestAddUnifiedLock_PersistsAndReloads verifies that an NLM/unified lock
// acquired via AddUnifiedLock() is persisted and reloads after a restart.
func TestAddUnifiedLock_PersistsAndReloads(t *testing.T) {
	ctx := context.Background()
	store := newMockLockStore()

	mgr := NewManager()
	mgr.SetLockStore(store)

	const handleKey = "share-a:file-2"
	ul := &UnifiedLock{
		ID: "unified-lock-1",
		Owner: LockOwner{
			OwnerID:   "nlm:client-x",
			ClientID:  "client-x",
			ShareName: "share-a",
		},
		FileHandle: FileHandle(handleKey),
		Offset:     0,
		Length:     0,
		Type:       LockTypeExclusive,
	}
	require.NoError(t, mgr.AddUnifiedLock(handleKey, ul))

	persisted, err := store.ListLocks(ctx, LockQuery{})
	require.NoError(t, err)
	require.Len(t, persisted, 1, "unified lock should be persisted")

	// Simulate restart.
	fresh := NewManager()
	fresh.SetLockStore(store)
	require.NoError(t, fresh.RestoreLocks(persisted))

	restored := fresh.ListUnifiedLocks(handleKey)
	require.Len(t, restored, 1, "unified lock should be present after restore")
	require.Equal(t, "nlm:client-x", restored[0].Owner.OwnerID)
}

// TestUnlock_DeletesPersisted verifies that releasing a byte-range lock via
// Unlock() removes it from the persistent store.
func TestUnlock_DeletesPersisted(t *testing.T) {
	ctx := context.Background()
	store := newMockLockStore()

	mgr := NewManager()
	mgr.SetLockStore(store)

	const handleKey = "share-a:file-3"
	fl := FileLock{
		SessionID: 7,
		OpenID:    "open-1",
		Offset:    0,
		Length:    10,
		Exclusive: true,
	}
	require.NoError(t, mgr.Lock(handleKey, fl))

	persisted, err := store.ListLocks(ctx, LockQuery{})
	require.NoError(t, err)
	require.Len(t, persisted, 1)

	require.NoError(t, mgr.Unlock(handleKey, "open-1", 7, 0, 10))

	persisted, err = store.ListLocks(ctx, LockQuery{})
	require.NoError(t, err)
	require.Empty(t, persisted, "lock should be deleted from store after Unlock")
}

// TestRemoveUnifiedLock_DeletesPersisted verifies that removing a unified lock
// deletes it from the persistent store.
func TestRemoveUnifiedLock_DeletesPersisted(t *testing.T) {
	ctx := context.Background()
	store := newMockLockStore()

	mgr := NewManager()
	mgr.SetLockStore(store)

	const handleKey = "share-a:file-4"
	owner := LockOwner{OwnerID: "nlm:client-y", ClientID: "client-y", ShareName: "share-a"}
	ul := &UnifiedLock{
		ID:         "unified-lock-2",
		Owner:      owner,
		FileHandle: FileHandle(handleKey),
		Offset:     0,
		Length:     0,
		Type:       LockTypeExclusive,
	}
	require.NoError(t, mgr.AddUnifiedLock(handleKey, ul))

	persisted, err := store.ListLocks(ctx, LockQuery{})
	require.NoError(t, err)
	require.Len(t, persisted, 1)

	require.NoError(t, mgr.RemoveUnifiedLock(handleKey, owner, 0, 0))

	persisted, err = store.ListLocks(ctx, LockQuery{})
	require.NoError(t, err)
	require.Empty(t, persisted, "unified lock should be deleted from store")
}

// TestLock_StackedSMBLocksPersistIndependently verifies HIGH-3: SMB2 permits
// stacking multiple identical (same open/offset/length/type) byte-range locks,
// each requiring its own Unlock. The persisted identity must match this
// stacking so unlocking ONE stacked entry does not drop the persisted record
// while another in-memory entry survives.
func TestLock_StackedSMBLocksPersistIndependently(t *testing.T) {
	ctx := context.Background()
	store := newMockLockStore()

	mgr := NewManager()
	mgr.SetLockStore(store)
	mgr.SetShareName("share-a")

	const handleKey = "share-a:file-stack"
	// SMB stacks identical SHARED locks from the same open (Samba
	// brl_lock_windows); exclusive re-locks on the same range are rejected.
	fl := FileLock{
		SessionID: 7,
		OpenID:    "open-1", // SMB per-open => stacking semantics
		Offset:    100,
		Length:    50,
		Exclusive: false,
	}
	// Stack two identical locks.
	require.NoError(t, mgr.Lock(handleKey, fl))
	require.NoError(t, mgr.Lock(handleKey, fl))

	require.Len(t, mgr.ListLocks(handleKey), 2, "two stacked locks held in memory")

	persisted, err := store.ListLocks(ctx, LockQuery{ShareName: "share-a"})
	require.NoError(t, err)
	require.Len(t, persisted, 2, "each stacked lock must persist as a distinct record")

	// Unlock once: one in-memory entry remains, so a persisted record must too.
	require.NoError(t, mgr.Unlock(handleKey, "open-1", 7, 100, 50))
	require.Len(t, mgr.ListLocks(handleKey), 1, "one stacked lock remains in memory")

	persisted, err = store.ListLocks(ctx, LockQuery{ShareName: "share-a"})
	require.NoError(t, err)
	require.Len(t, persisted, 1, "one persisted record must survive partial unlock")

	// Unlock the last entry: now the store is empty.
	require.NoError(t, mgr.Unlock(handleKey, "open-1", 7, 100, 50))
	persisted, err = store.ListLocks(ctx, LockQuery{ShareName: "share-a"})
	require.NoError(t, err)
	require.Empty(t, persisted, "store empty after both stacked locks released")
}

// TestLock_ZeroByteLockPreservesIsZeroByte verifies HIGH-4: an SMB2 zero-byte
// lock (Length==0 but IsZeroByte) must NOT be restored as an unbounded
// (to-EOF) lock — that would produce wrong conflict checks after restart.
func TestLock_ZeroByteLockPreservesIsZeroByte(t *testing.T) {
	store := newMockLockStore()

	mgr := NewManager()
	mgr.SetLockStore(store)
	mgr.SetShareName("share-a")

	const handleKey = "share-a:file-zb"
	zb := FileLock{
		SessionID:  7,
		OpenID:     "open-1",
		Offset:     10,
		Length:     0,
		IsZeroByte: true,
		Exclusive:  true,
	}
	require.NoError(t, mgr.Lock(handleKey, zb))

	persisted, err := store.ListLocks(context.Background(), LockQuery{ShareName: "share-a"})
	require.NoError(t, err)
	require.Len(t, persisted, 1)
	require.True(t, persisted[0].IsZeroByte, "zero-byte flag must be persisted")

	// Restore into a fresh manager and verify the restored byte-range lock is
	// still zero-byte (does not block an overlapping I/O / lock to EOF).
	fresh := NewManager()
	fresh.SetLockStore(store)
	require.NoError(t, fresh.RestoreLocks(persisted))

	restored := fresh.ListLocks(handleKey)
	require.Len(t, restored, 1, "zero-byte lock must restore into legacy locks map")
	require.True(t, restored[0].IsZeroByte, "restored lock must remain zero-byte, not to-EOF")
}

// TestRestoreLocks_ByteRangeEnforcedAfterRestart verifies HIGH-5: byte-range
// locks restored after a restart must be enforced by the legacy byte-range ops
// (TestLock / CheckForIO), which consult lm.locks — not lm.unifiedLocks.
func TestRestoreLocks_ByteRangeEnforcedAfterRestart(t *testing.T) {
	store := newMockLockStore()

	mgr := NewManager()
	mgr.SetLockStore(store)
	mgr.SetShareName("share-a")

	const handleKey = "share-a:file-br"
	held := FileLock{
		SessionID: 7,
		OpenID:    "open-1",
		Offset:    0,
		Length:    100,
		Exclusive: true,
	}
	require.NoError(t, mgr.Lock(handleKey, held))

	persisted, err := store.ListLocks(context.Background(), LockQuery{ShareName: "share-a"})
	require.NoError(t, err)
	require.Len(t, persisted, 1)

	// Simulate restart.
	fresh := NewManager()
	fresh.SetLockStore(store)
	require.NoError(t, fresh.RestoreLocks(persisted))

	// A conflicting lock from a different open must be denied.
	conflicting := FileLock{
		SessionID: 8,
		OpenID:    "open-2",
		Offset:    50,
		Length:    20,
		Exclusive: true,
	}
	conflict, err := fresh.TestLock(handleKey, conflicting)
	require.NoError(t, err)
	require.NotNil(t, conflict, "restored byte-range lock must be enforced by TestLock")

	// CheckForIO from a different open must see the conflict too.
	ioConflict := fresh.CheckForIO(handleKey, "open-2", 8, 50, 20, true)
	require.NotNil(t, ioConflict, "restored byte-range lock must be enforced by CheckForIO")
}
