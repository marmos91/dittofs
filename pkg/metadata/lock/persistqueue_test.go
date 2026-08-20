package lock

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// slowLockStore adds a fixed per-call delay to the mock store, standing in for
// a lock store whose writes cross a network.
type slowLockStore struct {
	*mockLockStore
	putDelay time.Duration
	delDelay time.Duration
}

func (s *slowLockStore) PutLock(ctx context.Context, lock *PersistedLock) error {
	time.Sleep(s.putDelay)
	return s.mockLockStore.PutLock(ctx, lock)
}

func (s *slowLockStore) DeleteLock(ctx context.Context, lockID string) error {
	time.Sleep(s.delDelay)
	return s.mockLockStore.DeleteLock(ctx, lockID)
}

func newSlowLockStore(put, del time.Duration) *slowLockStore {
	return &slowLockStore{mockLockStore: newMockLockStore(), putDelay: put, delDelay: del}
}

func benchFileLock(id, session uint64, openID string) FileLock {
	return FileLock{
		ID:        id,
		SessionID: session,
		OpenID:    openID,
		Offset:    0,
		Length:    16,
		Exclusive: true,
	}
}

// TestPersistQueue_WriteIsDurableBeforeReturn pins the contract that moving the
// store call out of lm.mu must not make persistence asynchronous: by the time
// Lock returns, the record is in the store, and by the time Unlock returns it
// is gone.
func TestPersistQueue_WriteIsDurableBeforeReturn(t *testing.T) {
	ctx := context.Background()
	store := newSlowLockStore(20*time.Millisecond, 20*time.Millisecond)

	mgr := NewManager()
	mgr.SetLockStore(store)
	mgr.SetShareName("share-a")

	const handleKey = "share-a:file-1"
	fl := benchFileLock(1, 7, "open-1")

	require.NoError(t, mgr.Lock(handleKey, fl))
	persisted, err := store.ListLocks(ctx, LockQuery{ShareName: "share-a"})
	require.NoError(t, err)
	require.Len(t, persisted, 1, "Lock must not return before its record reached the store")

	require.NoError(t, mgr.Unlock(handleKey, fl.OpenID, fl.SessionID, fl.Offset, fl.Length))
	persisted, err = store.ListLocks(ctx, LockQuery{ShareName: "share-a"})
	require.NoError(t, err)
	require.Empty(t, persisted, "Unlock must not return before its delete reached the store")
}

// TestPersistQueue_DeleteNeverOvertakesPut hammers one file — so every write
// lands on the same lane — with a store whose PutLock is far slower than its
// DeleteLock. An unordered flush lets a release's DeleteLock run before the
// acquire's PutLock it is meant to undo, which resurrects the record; the lane
// ticket is what forbids that. The store must end empty.
func TestPersistQueue_DeleteNeverOvertakesPut(t *testing.T) {
	ctx := context.Background()
	store := newSlowLockStore(5*time.Millisecond, 0)

	mgr := NewManager()
	mgr.SetLockStore(store)
	mgr.SetShareName("share-a")

	const handleKey = "share-a:file-1"
	const workers = 8
	const rounds = 10

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			// Non-overlapping ranges so the workers never conflict with each
			// other and every attempt actually reaches the store.
			offset := uint64(w) * 16
			fl := FileLock{
				ID:        uint64(w),
				SessionID: uint64(w),
				OpenID:    fmt.Sprintf("open-%d", w),
				Offset:    offset,
				Length:    16,
				Exclusive: true,
			}
			for r := 0; r < rounds; r++ {
				require.NoError(t, mgr.Lock(handleKey, fl))
				require.NoError(t, mgr.Unlock(handleKey, fl.OpenID, fl.SessionID, fl.Offset, fl.Length))
			}
		}(w)
	}
	wg.Wait()

	require.Empty(t, mgr.ListLocks(handleKey))

	persisted, err := store.ListLocks(ctx, LockQuery{ShareName: "share-a"})
	require.NoError(t, err)
	require.Empty(t, persisted, "a delete must never land before the put it undoes")
}

// TestPersistQueue_ClientBulkDeleteIsABarrier checks that the client-wide bulk
// delete, which is not scoped to one file, still orders against per-file writes
// on every file: locks acquired before it are gone afterwards even though they
// live on different lanes.
func TestPersistQueue_ClientBulkDeleteIsABarrier(t *testing.T) {
	ctx := context.Background()
	store := newSlowLockStore(2*time.Millisecond, 0)

	mgr := NewManager()
	mgr.SetLockStore(store)
	mgr.SetShareName("share-a")

	for i := 0; i < 32; i++ {
		handleKey := fmt.Sprintf("share-a:file-%d", i)
		lock := NewUnifiedLock(
			LockOwner{OwnerID: fmt.Sprintf("owner-%d", i), ClientID: "client-1", ShareName: "share-a"},
			FileHandle(handleKey), 0, 16, LockTypeExclusive)
		require.NoError(t, mgr.AddUnifiedLock(handleKey, lock))
	}

	persisted, err := store.ListLocks(ctx, LockQuery{ShareName: "share-a"})
	require.NoError(t, err)
	require.Len(t, persisted, 32)

	mgr.RemoveClientLocks("client-1")

	persisted, err = store.ListLocks(ctx, LockQuery{ShareName: "share-a"})
	require.NoError(t, err)
	require.Empty(t, persisted, "client bulk delete must order after every per-file write")
}

// BenchmarkLockUnlock_StoreLatency measures byte-range lock+unlock on distinct
// files — zero logical conflict — against lock stores of increasing round-trip
// cost. It is the shape that showed the share-wide mutex serializing every
// store round-trip: with the call inside lm.mu the result was flat across
// GOMAXPROCS at roughly two round-trips per operation no matter how many
// goroutines were pushing.
func BenchmarkLockUnlock_StoreLatency(b *testing.B) {
	for _, delay := range []time.Duration{0, 100 * time.Microsecond, time.Millisecond} {
		b.Run(delay.String(), func(b *testing.B) {
			store := newSlowLockStore(delay, delay)
			mgr := NewManager()
			mgr.SetLockStore(store)
			mgr.SetShareName("bench")

			var next atomic.Uint64
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				id := next.Add(1)
				handleKey := fmt.Sprintf("bench:file-%d", id)
				fl := benchFileLock(id, id, fmt.Sprintf("open-%d", id))
				for pb.Next() {
					if err := mgr.Lock(handleKey, fl); err != nil {
						b.Fatal(err)
					}
					if err := mgr.Unlock(handleKey, fl.OpenID, fl.SessionID, fl.Offset, fl.Length); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}
