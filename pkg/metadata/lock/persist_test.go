package lock

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
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

// TestRemoveUnifiedLock_SplitFragmentsPersistIndependently pins R1: when a
// partial unlock splits a unified byte-range lock into two fragments, each
// fragment must persist under a DISTINCT id. SplitLock previously cloned the
// original ID verbatim into both fragments, so the second PutLock overwrote
// the first (store keyed by ID) and one byte-range was silently lost on
// restart — allowing a conflicting write.
func TestRemoveUnifiedLock_SplitFragmentsPersistIndependently(t *testing.T) {
	ctx := context.Background()
	store := newMockLockStore()

	mgr := NewManager()
	mgr.SetLockStore(store)
	mgr.SetShareName("share-a")

	const handleKey = "share-a:file-split"
	owner := LockOwner{OwnerID: "nlm:client-z", ClientID: "client-z", ShareName: "share-a"}
	ul := &UnifiedLock{
		ID:         "unified-split",
		Owner:      owner,
		FileHandle: FileHandle(handleKey),
		Offset:     0,
		Length:     100,
		Type:       LockTypeExclusive,
	}
	require.NoError(t, mgr.AddUnifiedLock(handleKey, ul))

	// Unlock the middle [40,60) -> fragments [0,40) and [60,100).
	require.NoError(t, mgr.RemoveUnifiedLock(handleKey, owner, 40, 20))

	// Both fragments must survive in memory and in the store as DISTINCT records.
	require.Len(t, mgr.ListUnifiedLocks(handleKey), 2, "split must yield two in-memory fragments")

	persisted, err := store.ListLocks(ctx, LockQuery{ShareName: "share-a"})
	require.NoError(t, err)
	require.Len(t, persisted, 2, "both split fragments must persist under distinct ids")

	ids := map[string]bool{}
	for _, pl := range persisted {
		ids[pl.ID] = true
	}
	require.Len(t, ids, 2, "fragment ids must be distinct")

	// After restart, both byte-ranges must still conflict-check.
	fresh := NewManager()
	fresh.SetLockStore(store)
	require.NoError(t, fresh.RestoreLocks(persisted))
	restored := fresh.ListUnifiedLocks(handleKey)
	require.Len(t, restored, 2, "both fragments must restore after a restart")

	var ranges [][2]uint64
	for _, r := range restored {
		ranges = append(ranges, [2]uint64{r.Offset, r.Length})
	}
	require.ElementsMatch(t, [][2]uint64{{0, 40}, {60, 40}}, ranges,
		"restored fragments must be [0,40) and [60,100)")
}

// TestAddUnifiedLock_StampsManagerShareName pins R2: NFSv4/NLM producers build
// LockOwner with ShareName="" (the byte-range path never carries it). The
// manager must stamp its own share name at persist time so the per-share
// recovery query (ListLocks{ShareName}) finds the lock on restart. Without
// this, NFSv4 byte-range locks were silently dropped after a restart.
func TestAddUnifiedLock_StampsManagerShareName(t *testing.T) {
	ctx := context.Background()
	store := newMockLockStore()

	mgr := NewManager()
	mgr.SetLockStore(store)
	mgr.SetShareName("share-x")

	const handleKey = "share-x:file-nfs4"
	// Mimic the NFSv4 acquireLock producer: ShareName intentionally empty.
	ul := &UnifiedLock{
		ID: "nfs4-lock-1",
		Owner: LockOwner{
			OwnerID:   "nfs4:1:deadbeef",
			ClientID:  "nfs4:1",
			ShareName: "",
		},
		FileHandle: FileHandle(handleKey),
		Offset:     0,
		Length:     0,
		Type:       LockTypeExclusive,
	}
	require.NoError(t, mgr.AddUnifiedLock(handleKey, ul))

	// The per-share recovery query must find it.
	persisted, err := store.ListLocks(ctx, LockQuery{ShareName: "share-x"})
	require.NoError(t, err)
	require.Len(t, persisted, 1, "lock must be persisted under the manager's share name")
	require.Equal(t, "share-x", persisted[0].ShareName)

	// End-to-end restore via the per-share query finds and restores it.
	fresh := NewManager()
	fresh.SetLockStore(store)
	require.NoError(t, fresh.RestoreLocks(persisted))
	require.Len(t, fresh.ListUnifiedLocks(handleKey), 1, "lock must restore after restart")
}

// TestLock_StampsClientIDForCleanup pins R3: SMB byte-range locks must persist
// a ClientID that DeleteLocksByClient (RemoveClientLocks) will match. Without
// it, a disconnecting client's persisted byte-range rows survived forever and
// resurrected on the next restart, blocking legitimate IO.
func TestLock_StampsClientIDForCleanup(t *testing.T) {
	ctx := context.Background()
	store := newMockLockStore()

	mgr := NewManager()
	mgr.SetLockStore(store)
	mgr.SetShareName("share-a")

	const handleKey = "share-a:file-client"
	fl := FileLock{
		SessionID: 7,
		OpenID:    "open-1",
		ClientID:  "smb:7",
		Offset:    0,
		Length:    10,
		Exclusive: true,
	}
	require.NoError(t, mgr.Lock(handleKey, fl))

	persisted, err := store.ListLocks(ctx, LockQuery{ShareName: "share-a"})
	require.NoError(t, err)
	require.Len(t, persisted, 1)
	require.Equal(t, "smb:7", persisted[0].ClientID, "byte-range lock must persist its client id")

	// Client disconnect: RemoveClientLocks must purge the persisted row.
	mgr.RemoveClientLocks("smb:7")

	persisted, err = store.ListLocks(ctx, LockQuery{})
	require.NoError(t, err)
	require.Empty(t, persisted, "DeleteLocksByClient must match and remove the byte-range lock")
}

// TestLock_StackedAfterRestart_NoPersistIDCollision pins R3-2: lockSeq reset
// to 0 on a fresh Manager and RestoreLocks never restored it, so a new stacked
// lock regenerated a persistID identical to a restored one. The id-keyed
// PutLock upsert then overwrote the restored record, resurrecting the
// stacked-unlock data-loss bug. With UUID persist IDs there is no collision
// surface across restarts.
//
// Scenario: stack 2 identical SMB shared locks pre-restart, restore into a
// fresh manager, stack a 3rd identical lock, then unlock one — the store must
// hold 3 distinct records before the unlock and exactly 2 after.
func TestLock_StackedAfterRestart_NoPersistIDCollision(t *testing.T) {
	ctx := context.Background()
	store := newMockLockStore()

	mgr := NewManager()
	mgr.SetLockStore(store)
	mgr.SetShareName("share-a")

	const handleKey = "share-a:file-stack-restart"
	fl := FileLock{
		SessionID: 7,
		OpenID:    "open-1", // SMB per-open => stacking semantics
		Offset:    100,
		Length:    50,
		Exclusive: false, // shared locks stack
	}
	// Pre-restart: stack two identical shared locks.
	require.NoError(t, mgr.Lock(handleKey, fl))
	require.NoError(t, mgr.Lock(handleKey, fl))

	persisted, err := store.ListLocks(ctx, LockQuery{ShareName: "share-a"})
	require.NoError(t, err)
	require.Len(t, persisted, 2, "two stacked records pre-restart")

	// Simulate restart: fresh manager (lockSeq would reset to 0), restore.
	fresh := NewManager()
	fresh.SetLockStore(store)
	fresh.SetShareName("share-a")
	require.NoError(t, fresh.RestoreLocks(persisted))
	require.Len(t, fresh.ListLocks(handleKey), 2, "two restored stacked locks")

	// Stack a 3rd identical lock post-restart. A deterministic seq-based
	// persistID would collide with a restored record here and overwrite it.
	require.NoError(t, fresh.Lock(handleKey, fl))
	require.Len(t, fresh.ListLocks(handleKey), 3, "three stacked locks in memory")

	persisted, err = store.ListLocks(ctx, LockQuery{ShareName: "share-a"})
	require.NoError(t, err)
	require.Len(t, persisted, 3, "all three stacked locks must persist as DISTINCT records (no collision)")

	ids := map[string]bool{}
	for _, pl := range persisted {
		ids[pl.ID] = true
	}
	require.Len(t, ids, 3, "persist IDs must be distinct across the restart boundary")

	// Unlock ONE: two in-memory entries remain, so exactly two persisted too.
	require.NoError(t, fresh.Unlock(handleKey, "open-1", 7, 100, 50))
	require.Len(t, fresh.ListLocks(handleKey), 2, "two stacked locks remain in memory")

	persisted, err = store.ListLocks(ctx, LockQuery{ShareName: "share-a"})
	require.NoError(t, err)
	require.Len(t, persisted, 2, "exactly two persisted records survive the partial unlock")
}

// TestUpgradeLock_PersistsAndReloads pins R3-3: UpgradeLock flipped a shared
// lock to exclusive in memory but emitted no persist, so the upgrade was lost
// on restart — the lock reverted to shared and a reader could be wrongly
// granted against an intended-exclusive lock.
func TestUpgradeLock_PersistsAndReloads(t *testing.T) {
	ctx := context.Background()
	store := newMockLockStore()

	mgr := NewManager()
	mgr.SetLockStore(store)
	mgr.SetShareName("share-a")

	const handleKey = "share-a:file-upgrade"
	owner := LockOwner{OwnerID: "nlm:client-u", ClientID: "client-u", ShareName: "share-a"}
	ul := &UnifiedLock{
		ID:         "upgrade-lock-1",
		Owner:      owner,
		FileHandle: FileHandle(handleKey),
		Offset:     0,
		Length:     100,
		Type:       LockTypeShared,
	}
	require.NoError(t, mgr.AddUnifiedLock(handleKey, ul))

	// Upgrade shared -> exclusive.
	upgraded, err := mgr.UpgradeLock(handleKey, owner, 0, 100)
	require.NoError(t, err)
	require.Equal(t, LockTypeExclusive, upgraded.Type)

	// The persisted record must reflect the upgraded (exclusive) type.
	persisted, err := store.ListLocks(ctx, LockQuery{ShareName: "share-a"})
	require.NoError(t, err)
	require.Len(t, persisted, 1)
	require.Equal(t, int(LockTypeExclusive), persisted[0].LockType,
		"persisted lock must reflect the upgrade, not the pre-upgrade shared type")

	// After restart the lock must still be exclusive.
	fresh := NewManager()
	fresh.SetLockStore(store)
	require.NoError(t, fresh.RestoreLocks(persisted))
	restored := fresh.ListUnifiedLocks(handleKey)
	require.Len(t, restored, 1)
	require.Equal(t, LockTypeExclusive, restored[0].Type,
		"upgraded lock must restore as exclusive, not revert to shared")
}

// TestUpgradeLock_WholeLockOnly pins the documented UpgradeLock precondition:
// callers upgrade a lock at exactly the range it was granted at (NFSv4 LOCK
// upgrades the lock-owner's whole existing lock), so promoting the entire
// matched range to exclusive — and persisting that whole range — is correct.
// This test asserts the whole-lock upgrade case: the matched lock keeps its
// original [offset,length) and flips to exclusive in memory and in the store.
//
// It also documents the boundary: there are no production sub-range upgrade
// callers. If one is ever added (upgrading a strict sub-range of a wider shared
// lock), UpgradeLock must split before promoting; see the precondition comment
// on UpgradeLock.
func TestUpgradeLock_WholeLockOnly(t *testing.T) {
	ctx := context.Background()
	store := newMockLockStore()

	mgr := NewManager()
	mgr.SetLockStore(store)
	mgr.SetShareName("share-a")

	const handleKey = "share-a:file-whole-upgrade"
	owner := LockOwner{OwnerID: "nlm:client-w", ClientID: "client-w", ShareName: "share-a"}
	ul := &UnifiedLock{
		ID:         "whole-upgrade-1",
		Owner:      owner,
		FileHandle: FileHandle(handleKey),
		Offset:     10,
		Length:     90,
		Type:       LockTypeShared,
	}
	require.NoError(t, mgr.AddUnifiedLock(handleKey, ul))

	// Upgrade at exactly the granted range (whole-lock upgrade).
	upgraded, err := mgr.UpgradeLock(handleKey, owner, 10, 90)
	require.NoError(t, err)
	require.Equal(t, LockTypeExclusive, upgraded.Type)
	require.Equal(t, uint64(10), upgraded.Offset, "upgrade must not move the range")
	require.Equal(t, uint64(90), upgraded.Length, "upgrade must not resize the range")

	// In memory there is still exactly one lock, now exclusive at [10,100).
	live := mgr.ListUnifiedLocks(handleKey)
	require.Len(t, live, 1, "whole-lock upgrade must not split or duplicate the lock")
	require.Equal(t, LockTypeExclusive, live[0].Type)
	require.Equal(t, uint64(10), live[0].Offset)
	require.Equal(t, uint64(90), live[0].Length)

	// Exactly one persisted record, reflecting the whole-range exclusive upgrade.
	persisted, err := store.ListLocks(ctx, LockQuery{ShareName: "share-a"})
	require.NoError(t, err)
	require.Len(t, persisted, 1, "whole-lock upgrade persists a single record")
	require.Equal(t, int(LockTypeExclusive), persisted[0].LockType)
	require.Equal(t, uint64(10), persisted[0].Offset)
	require.Equal(t, uint64(90), persisted[0].Length)
}

// TestLockManager_ConcurrentStorm_StoreMatchesMemory is the permanent net for
// the persist-ordering / resurrection bug class (R3-1): if a PutLock/DeleteLock
// were ever dispatched OUTSIDE lm.mu (async), the store could observe two
// mutations on the SAME persisted key in a different order than memory did,
// leaving a record present in the store but absent in memory (resurrection) or
// absent in the store but present in memory (lost persist). The current design
// persists synchronously inside lm.mu, so mutex order == store order and the
// class is closed; this test exists to FAIL the moment that guarantee regresses.
//
// To exercise that class the storm must (a) hammer the SAME persisted store key
// with interleaved put/delete, and (b) observe the store at a restart boundary
// WHILE the storm is still running — not only after it quiesces. We do both:
//
//   - SAME-KEY CONTENTION. We drive a SMALL, fixed set of owners over a SMALL,
//     fixed set of OVERLAPPING ranges on ONE handle, via two paths whose persist
//     key is stable across a lock→unlock→re-lock cycle (so put and delete race
//     the same store row, not fresh UUIDs that can never collide):
//   - NLM/POSIX byte-range (OpenID==""): Lock re-locks the same
//     (owner,offset,length) IN PLACE, reusing the same persistID; Unlock
//     deletes that same persistID. Re-Lock-after-Unlock from another
//     goroutine reuses it again.
//   - Unified locks with a deterministic ID keyed only by (owner,range):
//     AddUnifiedLock upserts that ID, RemoveUnifiedLock deletes it,
//     UpgradeLock rewrites it — all on the one store key.
//   - MID-STORM RESTART. A snapshotter goroutine periodically snapshots the
//     LockStore, restores it into a brand-new Manager, and asserts each snapshot
//     is INTERNALLY CONSISTENT (no duplicate keys, every restored record is a
//     well-formed lock) — i.e. the store is never caught in a half-written state
//     that violates an invariant, at an arbitrary instant during the storm.
//
// After quiesce we assert the strong bijection: every in-memory lock has exactly
// one store record and every store record has exactly one in-memory lock — no
// orphans, no resurrections, no lost stacked entries.
//
// HOW THIS CATCHES A persist-outside-mutex REGRESSION. Suppose persistence were
// moved back to an async goroutine. Take one stable key K (e.g. the NLM re-lock
// at a fixed owner+range). Goroutine A does Unlock(K) -> enqueues DeleteLock(K);
// goroutine B does re-Lock(K) -> enqueues PutLock(K). In-memory the final state
// is whatever ran last under lm.mu, but the async dispatcher can apply
// PutLock(K) then DeleteLock(K) (or vice-versa) in the opposite order. The store
// then either holds K with no in-memory lock (orphan) or lacks K while memory
// holds it (lost persist). The post-quiesce bijection flags exactly that:
// len(memIDs) != len(storeIDs) or a key present on one side only. The mid-storm
// snapshotter additionally catches a transient half-applied batch. Under the
// synchronous-under-mutex design the two mutations on K are serialized by lm.mu
// in the same order memory sees them, so neither divergence can arise and the
// test stays green. It is scheduler-probabilistic (it relies on goroutines
// actually interleaving on K), so we loop it; see the subtest loop below.
func TestLockManager_ConcurrentStorm_StoreMatchesMemory(t *testing.T) {
	// Loop so the scheduler gets many chances to interleave put/delete on the
	// same key. Each round is a fresh store+manager. Running the whole test
	// under `go test -race -run ConcurrentStorm -count=N` multiplies this.
	const rounds = 20
	for r := 0; r < rounds; r++ {
		t.Run(fmt.Sprintf("round-%d", r), func(t *testing.T) {
			runConcurrentStormRound(t, int64(r))
		})
	}
}

func runConcurrentStormRound(t *testing.T, seedBase int64) {
	t.Helper()

	store := newMockLockStore()

	mgr := NewManager()
	mgr.SetLockStore(store)
	mgr.SetShareName("share-storm")

	const handleKey = handleKeyStorm

	const (
		goroutines = 12
		iterations = 200
		// Small fixed cardinalities force collisions on the SAME store key:
		// distinct (owner,range) pairs map to a stable, reused persist key.
		numOwners = 3
		numRanges = 4
	)

	// Fixed overlapping ranges on the one handle. They overlap heavily so
	// conflict checks, upgrades and POSIX splits all fire on shared rows.
	ranges := [numRanges][2]uint64{{0, 40}, {20, 40}, {0, 100}, {30, 30}}

	// Stable unified-lock ID per (owner,range): this is the store key that
	// AddUnifiedLock / RemoveUnifiedLock / UpgradeLock all contend on.
	ulID := func(ownerIdx, rangeIdx int) string {
		return fmt.Sprintf("ul-o%d-r%d", ownerIdx, rangeIdx)
	}

	stop := make(chan struct{})

	// Snapshotter: mid-storm, repeatedly snapshot the store, restore into a
	// fresh manager, and check each snapshot is internally consistent. The first
	// inconsistency is captured into snapErr and asserted on the main goroutine
	// after snapWG.Wait() — testify's require.* calls t.FailNow (runtime.Goexit),
	// which must run on the test goroutine, not here.
	var (
		snapWG  sync.WaitGroup
		snapErr error
	)
	snapWG.Add(1)
	go func() {
		defer snapWG.Done()
		ctx := context.Background()
		for {
			select {
			case <-stop:
				return
			default:
			}
			snap, err := store.ListLocks(ctx, LockQuery{ShareName: "share-storm"})
			if err != nil {
				snapErr = fmt.Errorf("mid-storm ListLocks: %w", err)
				return
			}
			if err := snapshotInternalConsistencyErr(snap); err != nil {
				snapErr = err
				return
			}

			fresh := NewManager()
			fresh.SetLockStore(store)
			fresh.SetShareName("share-storm")
			if err := fresh.RestoreLocks(snap); err != nil {
				snapErr = fmt.Errorf("mid-storm RestoreLocks: %w", err)
				return
			}
			// The restore must reproduce exactly the snapshot's records, with no
			// half-written/orphaned state surviving into the fresh manager.
			if got := len(liveManagerPersistIDs(fresh, handleKey)); got != len(snap) {
				snapErr = fmt.Errorf("mid-storm restore reproduced %d records, snapshot had %d", got, len(snap))
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seedBase*1000 + int64(seed)))
			for i := 0; i < iterations; i++ {
				ownerIdx := rng.Intn(numOwners)
				rangeIdx := rng.Intn(numRanges)
				off, length := ranges[rangeIdx][0], ranges[rangeIdx][1]
				owner := LockOwner{
					OwnerID:   fmt.Sprintf("nlm:client-%d", ownerIdx),
					ClientID:  fmt.Sprintf("client-%d", ownerIdx),
					ShareName: "share-storm",
				}
				switch rng.Intn(6) {
				case 0, 1:
					// NLM/POSIX byte-range (OpenID==""): re-lock in place reuses
					// the SAME persistID -> put/delete contend the same store key.
					_ = mgr.Lock(handleKey, FileLock{
						SessionID: uint64(ownerIdx),
						Offset:    off,
						Length:    length,
						Exclusive: rng.Intn(2) == 0,
						ClientID:  owner.ClientID,
					})
				case 2:
					_ = mgr.Unlock(handleKey, "", uint64(ownerIdx), off, length)
				case 3:
					// Unified lock with a STABLE id per (owner,range): upsert and
					// delete hammer the same store key as RemoveUnifiedLock/Upgrade.
					_ = mgr.AddUnifiedLock(handleKey, &UnifiedLock{
						ID:         ulID(ownerIdx, rangeIdx),
						Owner:      owner,
						FileHandle: FileHandle(handleKey),
						Offset:     off,
						Length:     length,
						Type:       LockTypeShared,
					})
				case 4:
					_ = mgr.RemoveUnifiedLock(handleKey, owner, off, length)
				case 5:
					_, _ = mgr.UpgradeLock(handleKey, owner, off, length)
				}
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	snapWG.Wait()

	// Surface any mid-storm inconsistency on the test goroutine (require.* must
	// not run inside the snapshotter goroutine).
	require.NoError(t, snapErr, "mid-storm snapshot was internally inconsistent")

	// Simulate a final restart: snapshot the store, restore into a fresh
	// manager, and assert the restored in-memory state matches the store exactly.
	ctx := context.Background()
	snapshot, err := store.ListLocks(ctx, LockQuery{ShareName: "share-storm"})
	require.NoError(t, err)

	fresh := NewManager()
	fresh.SetLockStore(store)
	fresh.SetShareName("share-storm")
	require.NoError(t, fresh.RestoreLocks(snapshot))

	// Collect persist IDs from the store snapshot (no duplicate keys allowed).
	storeIDs := map[string]bool{}
	for _, pl := range snapshot {
		require.False(t, storeIDs[pl.ID], "store must not hold duplicate persist IDs")
		storeIDs[pl.ID] = true
	}

	// Bijection: each in-memory lock has exactly one matching store record (no
	// orphans), and every store record maps to an in-memory lock (no
	// resurrections). A persist-outside-mutex reorder on a shared key shows up
	// here as a one-sided key or a count mismatch.
	memIDs := liveManagerPersistIDs(mgr, handleKey)

	for id := range memIDs {
		require.True(t, storeIDs[id],
			"in-memory lock %s has no store record (lost persist / reorder)", id)
	}
	for id := range storeIDs {
		require.True(t, memIDs[id],
			"store record %s has no in-memory lock (orphan / resurrection)", id)
	}
	require.Equal(t, len(memIDs), len(storeIDs),
		"store record count must equal in-memory lock count after the storm")
}

// snapshotInternalConsistencyErr checks that a mid-storm store snapshot is not
// caught in a state that violates a record-level invariant: no duplicate persist
// keys, and every record is a well-formed lock (non-empty key, share stamped,
// a routable shape). A half-written/orphaned record trips this and returns an
// error. It returns (rather than asserting) so it is safe to call from the
// snapshotter goroutine; the caller surfaces the error on the test goroutine.
func snapshotInternalConsistencyErr(snap []*PersistedLock) error {
	seen := map[string]bool{}
	for _, pl := range snap {
		if pl.ID == "" {
			return fmt.Errorf("snapshot record has empty persist id")
		}
		if seen[pl.ID] {
			return fmt.Errorf("snapshot holds duplicate persist id %s", pl.ID)
		}
		seen[pl.ID] = true
		if pl.ShareName != "share-storm" {
			return fmt.Errorf("snapshot record %s has share %q, want share-storm", pl.ID, pl.ShareName)
		}
		if pl.FileID != handleKeyStorm {
			return fmt.Errorf("snapshot record %s has handle %q, want %s", pl.ID, pl.FileID, handleKeyStorm)
		}
	}
	return nil
}

// handleKeyStorm mirrors the storm test's single handle so the snapshot
// consistency check can assert every persisted record belongs to it.
const handleKeyStorm = "share-storm:file-storm"

// legacyPersistIDsForTest returns the persist IDs of the legacy byte-range
// locks held for handleKey. Same-package test accessor for the unexported
// persistID field, used by the concurrency-storm invariant check.
func (lm *Manager) legacyPersistIDsForTest(handleKey string) []string {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	var ids []string
	for i := range lm.locks[handleKey] {
		ids = append(ids, lm.locks[handleKey][i].persistID)
	}
	return ids
}

// liveManagerPersistIDs returns the set of persist IDs the manager currently
// holds in memory for handleKey, across both the legacy byte-range map and the
// unified-lock map. Byte-range entries expose their persist ID via the
// unexported persistID field, which this same-package test can read through a
// helper on Manager.
func liveManagerPersistIDs(mgr *Manager, handleKey string) map[string]bool {
	ids := map[string]bool{}
	for _, id := range mgr.legacyPersistIDsForTest(handleKey) {
		ids[id] = true
	}
	for _, ul := range mgr.ListUnifiedLocks(handleKey) {
		ids[ul.ID] = true
	}
	return ids
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
