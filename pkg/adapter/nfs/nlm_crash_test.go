package nfs

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/blocking"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
	"github.com/stretchr/testify/require"
)

// addNLMLock seeds an NLM byte-range lock on lm with the given owner ID.
func addNLMLock(t *testing.T, lm *metadata.LockManager, file, ownerID string, off uint64) {
	t.Helper()
	err := lm.AddUnifiedLock(file, &lock.UnifiedLock{
		ID:         ownerID,
		Owner:      lock.LockOwner{OwnerID: ownerID, ClientID: "ignored"},
		FileHandle: lock.FileHandle(file),
		Offset:     off,
		Length:     10,
		Type:       lock.LockTypeExclusive,
	})
	require.NoError(t, err)
}

func enqueueWaiter(t *testing.T, bq *blocking.BlockingQueue, file, caller, ownerID string) *blocking.Waiter {
	t.Helper()
	w := &blocking.Waiter{
		Lock:       &lock.UnifiedLock{Owner: lock.LockOwner{OwnerID: ownerID}},
		CallerName: caller,
	}
	require.NoError(t, bq.Enqueue(file, w))
	return w
}

// TestReleaseCrashedClientLocks proves that when client A crashes, all of A's
// NLM locks (across multiple shares) and its queued waiters are released, while
// client B's locks/waiters survive and B can subsequently acquire the range A
// had held.
func TestReleaseCrashedClientLocks(t *testing.T) {
	t.Parallel()

	// Two shares, each with its own lock manager.
	lmShareA := lock.NewManager()
	lmShareB := lock.NewManager()
	lockMgrFor := func(share string) *metadata.LockManager {
		switch share {
		case "share-a":
			return lmShareA
		case "share-b":
			return lmShareB
		default:
			return nil
		}
	}

	// Client A holds locks on both shares; client B holds one on share-a.
	addNLMLock(t, lmShareA, "share-a:file1", "nlm:clientA:1:aa", 0)
	addNLMLock(t, lmShareB, "share-b:file9", "nlm:clientA:2:bb", 0)
	addNLMLock(t, lmShareA, "share-a:file1", "nlm:clientB:1:cc", 100)
	// Prefix-collision client that MUST survive (no trailing-colon false match).
	addNLMLock(t, lmShareA, "share-a:file1", "nlm:clientA10:1:dd", 200)

	// Blocking queue: A and B each have a queued waiter.
	bq := blocking.NewBlockingQueue(100)
	aWaiter := enqueueWaiter(t, bq, "share-a:file1", "clientA", "nlm:clientA:3:ee")
	bWaiter := enqueueWaiter(t, bq, "share-a:file1", "clientB", "nlm:clientB:2:ff")

	// Sanity: A's locks are held BEFORE the crash.
	require.Len(t, lmShareA.ListUnifiedLocks("share-a:file1"), 3)
	require.Len(t, lmShareB.ListUnifiedLocks("share-b:file9"), 1)
	require.Equal(t, 2, bq.TotalWaiters())

	// Simulate the crash of client A.
	releaseCrashedClientLocks("clientA", []string{"share-a", "share-b"}, lockMgrFor, bq)

	// A's locks gone on both shares; B and the prefix-collision client survive.
	remA := lmShareA.ListUnifiedLocks("share-a:file1")
	require.Len(t, remA, 2, "only clientB and clientA10 should remain on share-a")
	for _, l := range remA {
		require.NotEqual(t, "nlm:clientA:1:aa", l.Owner.OwnerID, "clientA lock must be released")
	}
	require.Empty(t, lmShareB.ListUnifiedLocks("share-b:file9"), "clientA's share-b lock must be released")

	// A's waiter drained + cancelled; B's waiter survives intact.
	require.Equal(t, 1, bq.TotalWaiters(), "only clientB's waiter should remain")
	require.True(t, aWaiter.IsCancelled(), "clientA's drained waiter must be cancelled")
	require.False(t, bWaiter.IsCancelled(), "clientB's waiter must not be cancelled")

	// Client B can now acquire the exact range clientA had held on share-a.
	err := lmShareA.AddUnifiedLock("share-a:file1", &lock.UnifiedLock{
		ID:         "nlm:clientB:9:gg",
		Owner:      lock.LockOwner{OwnerID: "nlm:clientB:9:gg"},
		FileHandle: "share-a:file1",
		Offset:     0, // the range clientA held
		Length:     10,
		Type:       lock.LockTypeExclusive,
	})
	require.NoError(t, err, "B must be able to acquire the freed range after A's crash")
}

// TestReleaseCrashedClientLocks_NoLocksHeld proves idempotency / safety when the
// crashed client held nothing and a share has no lock manager.
func TestReleaseCrashedClientLocks_NoLocksHeld(t *testing.T) {
	t.Parallel()

	lm := lock.NewManager()
	addNLMLock(t, lm, "share-a:file1", "nlm:other:1:aa", 0)

	lockMgrFor := func(share string) *metadata.LockManager {
		if share == "share-a" {
			return lm
		}
		return nil // share-b has no lock manager
	}

	bq := blocking.NewBlockingQueue(100)

	// Crash of a client that holds nothing; nil blocking queue tolerated too.
	releaseCrashedClientLocks("ghost", []string{"share-a", "share-b"}, lockMgrFor, bq)
	releaseCrashedClientLocks("ghost", []string{"share-a"}, lockMgrFor, nil)

	require.Len(t, lm.ListUnifiedLocks("share-a:file1"), 1, "unrelated lock must survive")
}
