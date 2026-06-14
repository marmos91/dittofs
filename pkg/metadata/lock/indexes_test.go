package lock

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertIndexesConsistent recomputes the reverse indexes from the authoritative
// unifiedLocks map and asserts they match the maintained indexes exactly. This
// is the core invariant: the indexes are derived state and must never drift
// from unifiedLocks regardless of which mutation path ran.
func assertIndexesConsistent(t *testing.T, lm *Manager) {
	t.Helper()
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	// Expected clientHandleIndex: per (clientID, handleKey) the count of locks.
	wantClient := make(map[string]map[string]int)
	// Expected leaseKeyIndex: per lease key, the count of records each bucket
	// holds for it. The index is ref-counted per bucket and must match this
	// recomputation exactly — every holder bucket tracked, none dropped while
	// a record remains.
	wantLease := make(map[[16]byte]map[string]int)

	for handleKey, locks := range lm.unifiedLocks {
		for _, l := range locks {
			if cid := l.Owner.ClientID; cid != "" {
				if wantClient[cid] == nil {
					wantClient[cid] = make(map[string]int)
				}
				wantClient[cid][handleKey]++
			}
			if l.Lease != nil {
				if wantLease[l.Lease.LeaseKey] == nil {
					wantLease[l.Lease.LeaseKey] = make(map[string]int)
				}
				wantLease[l.Lease.LeaseKey][handleKey]++
			}
		}
	}

	// clientHandleIndex must equal the recomputed counts exactly.
	gotClient := make(map[string]map[string]int)
	for cid, set := range lm.clientHandleIndex {
		for hk, n := range set {
			if gotClient[cid] == nil {
				gotClient[cid] = make(map[string]int)
			}
			gotClient[cid][hk] = n
		}
	}
	assert.Equal(t, wantClient, gotClient, "clientHandleIndex drifted from unifiedLocks")

	// leaseKeyIndex must equal the recomputed per-bucket holder counts exactly:
	// every bucket holding the key tracked with the right count, and no stale
	// keys/buckets left behind.
	gotLease := make(map[[16]byte]map[string]int)
	for key, set := range lm.leaseKeyIndex {
		for hk, n := range set {
			if gotLease[key] == nil {
				gotLease[key] = make(map[string]int)
			}
			gotLease[key][hk] = n
		}
	}
	assert.Equal(t, wantLease, gotLease, "leaseKeyIndex drifted from unifiedLocks")
}

func TestIndex_StaysConsistentAcrossLeaseLifecycle(t *testing.T) {
	t.Parallel()
	mgr := NewManager()
	ctx := context.Background()
	key := [16]byte{1, 2, 3}

	_, _, err := mgr.RequestLease(ctx, FileHandle("fileA"), key, [16]byte{}, "owner1", "client1", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	assertIndexesConsistent(t, mgr)

	// findLeaseByKey must resolve via the index.
	hk, lk, _ := func() (string, *UnifiedLock, int) {
		mgr.mu.RLock()
		defer mgr.mu.RUnlock()
		return mgr.findLeaseByKey(key)
	}()
	assert.Equal(t, "fileA", hk)
	require.NotNil(t, lk)

	// Upgrade in place (no key/client change) keeps the index consistent.
	_, _, err = mgr.RequestLease(ctx, FileHandle("fileA"), key, [16]byte{}, "owner1", "client1", "/share", LeaseStateRead|LeaseStateWrite, false)
	require.NoError(t, err)
	assertIndexesConsistent(t, mgr)

	// Release: index entries gone.
	require.NoError(t, mgr.ReleaseLease(ctx, key))
	assertIndexesConsistent(t, mgr)
	mgr.mu.RLock()
	_, lk2, _ := mgr.findLeaseByKey(key)
	mgr.mu.RUnlock()
	assert.Nil(t, lk2, "lease should be unresolvable after release")
}

func TestIndex_ReleaseLeaseForHandleKeepsOtherFileBinding(t *testing.T) {
	t.Parallel()
	mgr := NewManager()
	ctx := context.Background()
	key := [16]byte{9, 9, 9}

	// Same lease key constant on two different files (distinct buckets).
	_, _, err := mgr.RequestLease(ctx, FileHandle("fileA"), key, [16]byte{}, "ownerA", "clientA", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	_, _, err = mgr.RequestLease(ctx, FileHandle("fileB"), key, [16]byte{}, "ownerB", "clientB", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	assertIndexesConsistent(t, mgr)

	// Release on fileA only; fileB's record must survive and stay resolvable.
	require.NoError(t, mgr.ReleaseLeaseForHandle(ctx, "fileA", key))
	assertIndexesConsistent(t, mgr)

	_, _, found := mgr.GetLeaseState(ctx, key)
	assert.True(t, found, "fileB lease record must survive fileA release")
}

// TestIndex_ReleaseBoundBucketKeepsOtherFileResolvable pins the case the
// single-bucket index got wrong: the same numeric lease key on two files, then
// releasing the bucket that was added LAST (the one a single-bucket index would
// have bound the key to). The remaining file's record must stay resolvable —
// dropping the whole key entry here would make findLeaseByKey report "not
// found" for a record that still exists.
func TestIndex_ReleaseBoundBucketKeepsOtherFileResolvable(t *testing.T) {
	t.Parallel()
	mgr := NewManager()
	ctx := context.Background()
	key := [16]byte{4, 2}

	_, _, err := mgr.RequestLease(ctx, FileHandle("fileA"), key, [16]byte{}, "ownerA", "clientA", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	// fileB added last — under the old single-bucket index this is the bucket
	// the key resolved to.
	_, _, err = mgr.RequestLease(ctx, FileHandle("fileB"), key, [16]byte{}, "ownerB", "clientB", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	assertIndexesConsistent(t, mgr)

	// Release the last-added bucket. fileA's record must survive and resolve.
	require.NoError(t, mgr.ReleaseLeaseForHandle(ctx, "fileB", key))
	assertIndexesConsistent(t, mgr)

	mgr.mu.RLock()
	hk, lk, _ := mgr.findLeaseByKey(key)
	mgr.mu.RUnlock()
	assert.Equal(t, "fileA", hk, "fileA must still resolve after releasing fileB")
	require.NotNil(t, lk, "fileA lease record must remain resolvable")

	_, _, found := mgr.GetLeaseState(ctx, key)
	assert.True(t, found, "fileA lease record must survive fileB release")
}

func TestIndex_RemoveClientLocksTouchesOnlyClientBuckets(t *testing.T) {
	t.Parallel()
	mgr := NewManager()
	ctx := context.Background()

	// client1 holds leases on two files; client2 on a third.
	_, _, err := mgr.RequestLease(ctx, FileHandle("f1"), [16]byte{1}, [16]byte{}, "o1", "client1", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	_, _, err = mgr.RequestLease(ctx, FileHandle("f2"), [16]byte{2}, [16]byte{}, "o2", "client1", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	_, _, err = mgr.RequestLease(ctx, FileHandle("f3"), [16]byte{3}, [16]byte{}, "o3", "client2", "/share", LeaseStateRead, false)
	require.NoError(t, err)
	assertIndexesConsistent(t, mgr)

	mgr.RemoveClientLocks("client1")
	assertIndexesConsistent(t, mgr)

	// client1's leases gone, client2's intact.
	_, _, f1 := mgr.GetLeaseState(ctx, [16]byte{1})
	_, _, f2 := mgr.GetLeaseState(ctx, [16]byte{2})
	_, _, f3 := mgr.GetLeaseState(ctx, [16]byte{3})
	assert.False(t, f1, "client1 f1 lease should be removed")
	assert.False(t, f2, "client1 f2 lease should be removed")
	assert.True(t, f3, "client2 f3 lease must remain")

	// clientHandleIndex must no longer mention client1.
	mgr.mu.RLock()
	_, present := mgr.clientHandleIndex["client1"]
	mgr.mu.RUnlock()
	assert.False(t, present, "client1 should be gone from clientHandleIndex")
}

func TestIndex_ByteRangeSplitKeepsClientCountConsistent(t *testing.T) {
	t.Parallel()
	mgr := NewManager()

	owner := LockOwner{OwnerID: "nlm:c:1:aa", ClientID: "nlmClient"}
	err := mgr.AddUnifiedLock("f1", &UnifiedLock{
		Owner:      owner,
		FileHandle: FileHandle("f1"),
		Offset:     0,
		Length:     100,
		Type:       LockTypeExclusive,
	})
	require.NoError(t, err)
	assertIndexesConsistent(t, mgr)

	// Unlock the middle, splitting the single lock into two fragments.
	require.NoError(t, mgr.RemoveUnifiedLock("f1", owner, 40, 20))
	assertIndexesConsistent(t, mgr)
}

func TestIndex_ReleaseByOwnerPrefixConsistent(t *testing.T) {
	t.Parallel()
	mgr := NewManager()

	add := func(handle, ownerID, clientID string) {
		require.NoError(t, mgr.AddUnifiedLock(handle, &UnifiedLock{
			Owner:      LockOwner{OwnerID: ownerID, ClientID: clientID},
			FileHandle: FileHandle(handle),
			Length:     10,
			Type:       LockTypeExclusive,
		}))
	}
	add("f1", "nlm:host1:1:aa", "c1")
	add("f2", "nlm:host1:2:bb", "c1")
	add("f3", "nlm:host10:1:cc", "c2") // must NOT match "nlm:host1:"
	assertIndexesConsistent(t, mgr)

	released := mgr.ReleaseByOwnerPrefix("nlm:host1:")
	assert.Equal(t, 2, released)
	assertIndexesConsistent(t, mgr)

	mgr.mu.RLock()
	_, c2still := mgr.clientHandleIndex["c2"]
	mgr.mu.RUnlock()
	assert.True(t, c2still, "host10 lock (c2) must survive prefix release")
}

func TestIndex_DelegationGrantAndReturnConsistent(t *testing.T) {
	t.Parallel()
	mgr := NewManager()

	deleg := NewDelegation(DelegTypeRead, "nfsClient", "/export", false)
	require.NoError(t, mgr.GrantDelegation("dfile", deleg))
	assertIndexesConsistent(t, mgr)

	// clientHandleIndex must record the delegation's ClientID.
	mgr.mu.RLock()
	_, present := mgr.clientHandleIndex["nfsClient"]
	mgr.mu.RUnlock()
	assert.True(t, present, "delegation ClientID must be indexed")

	require.NoError(t, mgr.ReturnDelegation("dfile", deleg.DelegationID))
	assertIndexesConsistent(t, mgr)

	mgr.mu.RLock()
	_, stillPresent := mgr.clientHandleIndex["nfsClient"]
	mgr.mu.RUnlock()
	assert.False(t, stillPresent, "delegation ClientID must be removed from index on return")
}

func TestIndex_ReclaimAddsToIndex(t *testing.T) {
	t.Parallel()
	mgr := NewManager()

	persisted := []*PersistedLock{
		{
			ID:         "id1",
			FileID:     "fileR",
			OwnerID:    "ownerR",
			ClientID:   "clientR",
			ShareName:  "/share",
			LeaseKey:   make([]byte, 16),
			LeaseState: LeaseStateRead,
			LeaseEpoch: 3,
		},
	}
	persisted[0].LeaseKey[0] = 7
	require.NoError(t, mgr.RestoreLocks(persisted))
	assertIndexesConsistent(t, mgr)

	var key [16]byte
	key[0] = 7
	mgr.mu.RLock()
	hk, lk, _ := mgr.findLeaseByKey(key)
	mgr.mu.RUnlock()
	assert.Equal(t, "fileR", hk)
	require.NotNil(t, lk, "restored lease must be resolvable via the index")
}
