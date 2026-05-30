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

	// The lock must have been persisted.
	persisted, err := store.ListLocks(ctx, LockQuery{})
	require.NoError(t, err)
	require.Len(t, persisted, 1, "byte-range lock should be persisted")

	// Simulate restart: fresh manager, restore from the store.
	fresh := NewManager()
	fresh.SetLockStore(store)
	require.NoError(t, fresh.RestoreLocks(persisted))

	restored := fresh.ListUnifiedLocks(handleKey)
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
		FileHandle: FileHandle("file-4"),
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
