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

	_, _, found := mgr.GetLeaseState(ctx, "fileB", key)
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

	_, _, found := mgr.GetLeaseState(ctx, "fileA", key)
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
	_, _, f1 := mgr.GetLeaseState(ctx, "f1", [16]byte{1})
	_, _, f2 := mgr.GetLeaseState(ctx, "f2", [16]byte{2})
	_, _, f3 := mgr.GetLeaseState(ctx, "f3", [16]byte{3})
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

// ============================================================================
// Index lookup vs full scan equivalence
// ============================================================================

// scanLeaseKeyMatches is a reference implementation of the record selection
// SetLeaseEpoch performs: a full sweep of every handleKey bucket, collecting the
// records bound to leaseKey ON handleKey and no others. A record for the same
// key on a different file belongs to a different lease.
func scanLeaseKeyMatches(lm *Manager, handleKey string, leaseKey [16]byte) map[string]bool {
	out := make(map[string]bool)
	for hk, locks := range lm.unifiedLocks {
		if hk != handleKey {
			continue
		}
		for _, l := range locks {
			if l.Lease != nil && l.Lease.LeaseKey == leaseKey {
				out[l.ID] = true
			}
		}
	}
	return out
}

// scanHasLeaseKeyOnOtherFile is the reference implementation
// hasLeaseKeyOnOtherFile used before it consulted leaseKeyIndex.
func scanHasLeaseKeyOnOtherFile(lm *Manager, leaseKey [16]byte, excludeHandleKey, clientID string) bool {
	for handleKey, locks := range lm.unifiedLocks {
		if handleKey == excludeHandleKey {
			continue
		}
		for _, l := range locks {
			if l.Lease == nil || l.Lease.LeaseKey != leaseKey {
				continue
			}
			if l.Owner.ClientID != clientID {
				continue
			}
			return true
		}
	}
	return false
}

// buildMixedLockSet populates a Manager with a realistic mix: the same numeric
// lease key bound on several files by different clients, distinct keys, a
// directory lease, byte-range (non-lease) records, and a released record — so
// the index-vs-scan comparison covers empty buckets and shared keys rather than
// just the happy path. Returns the lease keys and handle keys in play.
func buildMixedLockSet(t *testing.T, mgr *Manager) (keys [][16]byte, handleKeys []string) {
	t.Helper()
	ctx := context.Background()

	keyA := [16]byte{0xA}
	keyB := [16]byte{0xB}
	keyC := [16]byte{0xC}
	// keyD is never granted: probes for it must come back empty from both paths.
	keyD := [16]byte{0xD}

	grants := []struct {
		file     string
		key      [16]byte
		client   string
		isDir    bool
		newState uint32
	}{
		{"file1", keyA, "client1", false, LeaseStateRead},
		{"file2", keyA, "client2", false, LeaseStateRead},
		{"file3", keyA, "client3", false, LeaseStateRead | LeaseStateHandle},
		{"file1", keyB, "client2", false, LeaseStateRead},
		{"dir1", keyC, "client1", true, LeaseStateRead | LeaseStateHandle},
	}
	for _, g := range grants {
		_, _, err := mgr.RequestLease(ctx, FileHandle(g.file), g.key, [16]byte{},
			"owner-"+g.file+"-"+g.client, g.client, "/share", g.newState, g.isDir)
		require.NoError(t, err)
	}

	// Byte-range records on a file with no lease at all, plus one on a file that
	// does hold a lease — both must be invisible to every lease-key probe.
	for _, br := range []struct{ file, client string }{{"file4", "client1"}, {"file2", "client1"}} {
		err := mgr.AddUnifiedLock(br.file, NewUnifiedLock(
			LockOwner{OwnerID: "nlm:" + br.file + ":" + br.client, ClientID: br.client, ShareName: "/share"},
			FileHandle(br.file), 0, 4096, LockTypeExclusive))
		require.NoError(t, err)
	}

	// Release one holder of the shared key so a bucket drops out of the index.
	require.NoError(t, mgr.ReleaseLeaseForHandle(ctx, "file3", keyA))

	return [][16]byte{keyA, keyB, keyC, keyD},
		[]string{"file1", "file2", "file3", "file4", "dir1", "absent"}
}

// TestSetLeaseEpoch_MatchesFullScan proves the record selection in
// SetLeaseEpoch reaches the same records a full unifiedLocks sweep scoped to the
// same file would, converges every one of them, and leaves records for the same
// key on other files where they were. Runs over every (key, file) pair the
// fixture can produce, including pairs that hold no lease.
func TestSetLeaseEpoch_MatchesFullScan(t *testing.T) {
	t.Parallel()
	mgr := NewManager()
	keys, handleKeys := buildMixedLockSet(t, mgr)
	assertIndexesConsistent(t, mgr)

	const target = uint16(7)
	for _, key := range keys {
		for _, handleKey := range handleKeys {
			mgr.mu.RLock()
			want := scanLeaseKeyMatches(mgr, handleKey, key)
			// Snapshot every other record bound to this key so the assertion
			// below can show none of them moved.
			untouched := make(map[*UnifiedLock]uint16)
			for hk, locks := range mgr.unifiedLocks {
				for _, l := range locks {
					if l.Lease != nil && l.Lease.LeaseKey == key && hk != handleKey {
						untouched[l] = l.Lease.Epoch
					}
				}
			}
			mgr.mu.RUnlock()

			// SetLeaseEpoch reports found iff the scoped scan found records,
			// and lifts every one of them to the same epoch.
			ok := mgr.SetLeaseEpoch(handleKey, key, target)
			assert.Equal(t, len(want) > 0, ok,
				"SetLeaseEpoch found-flag must match the scan for key %x on %s", key, handleKey)

			mgr.mu.RLock()
			for _, l := range mgr.unifiedLocks[handleKey] {
				if l.Lease != nil && l.Lease.LeaseKey == key {
					assert.GreaterOrEqual(t, l.Lease.Epoch, target,
						"every record for key %x on %s must converge to at least the target epoch", key, handleKey)
				}
			}
			for l, epoch := range untouched {
				assert.Equal(t, epoch, l.Lease.Epoch,
					"a record for key %x on another file is a different lease and must not move", key)
			}
			mgr.mu.RUnlock()
		}
	}
}

// TestHasLeaseKeyOnOtherFile_IndexMatchesFullScan proves the index-driven
// uniqueness probe answers identically to the full scan it replaced, across
// every (key, excluded bucket, client) triple the fixture can produce.
func TestHasLeaseKeyOnOtherFile_IndexMatchesFullScan(t *testing.T) {
	t.Parallel()
	mgr := NewManager()
	keys, handleKeys := buildMixedLockSet(t, mgr)
	assertIndexesConsistent(t, mgr)

	clients := []string{"client1", "client2", "client3", "client4", ""}

	mgr.mu.RLock()
	defer mgr.mu.RUnlock()

	checked := 0
	for _, key := range keys {
		for _, exclude := range handleKeys {
			for _, client := range clients {
				want := scanHasLeaseKeyOnOtherFile(mgr, key, exclude, client)
				got := mgr.hasLeaseKeyOnOtherFile(key, exclude, client)
				assert.Equal(t, want, got,
					"key=%x exclude=%s client=%s", key, exclude, client)
				checked++
			}
		}
	}
	// Guard against the fixture silently collapsing to a trivial case.
	require.Greater(t, checked, 50, "expected a broad triple sweep")
}
