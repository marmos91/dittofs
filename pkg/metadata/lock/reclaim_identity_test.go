package lock

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestReclaimLease_ReturnsTheReclaimingClientsOwnLease drives a real
// restart-into-grace and inspects which record the reclaim comes back with.
//
// Two clients may hold the same 16-byte lease key value on different files of
// one share: the cross-file uniqueness rule is per (client, file), so the value
// alone is not an identity. The persisted-record lookup is correctly scoped by
// clientID, but the in-memory short-circuit that follows it is not — so once
// one client's record is back in memory, the other's reclaim can resolve to it.
func TestReclaimLease_ReturnsTheReclaimingClientsOwnLease(t *testing.T) {
	ctx := context.Background()
	store := newMockLockStore()

	leaseKey := [16]byte{0x5E, 0xED}
	const rh = LeaseStateRead | LeaseStateHandle

	// Two clients take the same key value on different files, and both leases
	// are persisted.
	mgr := NewManager()
	mgr.SetLockStore(store)
	mgr.SetShareName("share-a")

	_, _, err := mgr.RequestLease(ctx, FileHandle("share-a:file-X"), leaseKey, [16]byte{},
		"owner-A", "clientA", "share-a", rh, false)
	require.NoError(t, err)
	_, _, err = mgr.RequestLease(ctx, FileHandle("share-a:file-Y"), leaseKey, [16]byte{},
		"owner-B", "clientB", "share-a", rh, false)
	require.NoError(t, err)

	persisted, err := store.ListLocks(ctx, LockQuery{ShareName: "share-a"})
	require.NoError(t, err)
	require.Len(t, persisted, 2, "both clients' leases should be persisted")

	// Restart: fresh manager over the same store, inside the grace window.
	fresh := NewManagerWithGracePeriod(NewGracePeriodManager(2*time.Minute, func() {}))
	fresh.SetLockStore(store)
	fresh.SetShareName("share-a")
	fresh.EnterGracePeriod([]string{"clientA", "clientB"})
	require.True(t, fresh.IsInGracePeriod())

	// Client B reclaims first, putting its record back in memory.
	lockB, err := fresh.ReclaimLease(ctx, leaseKey, rh, false, "clientB")
	require.NoError(t, err)
	require.Equal(t, FileHandle("share-a:file-Y"), lockB.FileHandle,
		"client B must reclaim its own lease on file-Y")

	// Client A now reclaims. It must get its own lease on file-X.
	lockA, err := fresh.ReclaimLease(ctx, leaseKey, rh, false, "clientA")
	require.NoError(t, err)
	require.Equal(t, FileHandle("share-a:file-X"), lockA.FileHandle,
		"client A reclaimed a lease on the wrong file — the in-memory short-circuit resolved by lease key alone and handed back client B's record")
	require.Equal(t, "clientA", lockA.Owner.ClientID,
		"client A reclaimed a record owned by another client")
}
