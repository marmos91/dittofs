package blocking

import (
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// TestWaiter_SnapshotIsRaceFreeWithMutations drives every mutating path the
// queue has against concurrent Snapshot reads on the same waiter. A queued
// waiter is handed to the grant path by pointer, so the grant path reads it
// while the queue may still cancel it, stamp it, or drain it -- the waiter's
// own mutex is what makes that safe, and this is the check that fails if a
// field is ever read or written without it. Run under -race.
func TestWaiter_SnapshotIsRaceFreeWithMutations(t *testing.T) {
	const handle = "file1"

	bq := NewBlockingQueue(100)
	waiter := &Waiter{
		Lock: &lock.UnifiedLock{
			Owner:      lock.LockOwner{OwnerID: "nlm:clientA:1:aa"},
			FileHandle: lock.FileHandle(handle),
			Offset:     0,
			Length:     10,
		},
		CallerName:   "clientA",
		CallbackHost: "192.0.2.10",
		CallbackVers: 4,
	}
	if err := bq.Enqueue(handle, waiter); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Reader: the grant path's view of the waiter.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s := waiter.Snapshot()
				_ = s.Cancelled
				_ = s.CallerName
				_ = s.CallbackHost
				_ = s.Lock.Owner.OwnerID
			}
		}
	}()

	// Writers and other queue readers, each a real production path.
	for _, fn := range []func(){
		func() { waiter.setQueuedAt(time.Now()) },
		func() { waiter.Cancel() },
		func() { _ = waiter.IsCancelled() },
		func() { bq.Cancel(handle, "nlm:clientA:1:aa", 0, 10) },
		func() { bq.GetWaiters(handle) },
		func() { bq.TotalWaiters() },
		func() {
			// Retransmission of the same request: this is the path that reads
			// the queued waiter's identity fields to decide whether to keep it.
			_ = bq.Enqueue(handle, &Waiter{
				Lock: &lock.UnifiedLock{
					Owner:  lock.LockOwner{OwnerID: "nlm:clientA:1:aa"},
					Offset: 0,
					Length: 10,
				},
				CallerName: "clientA",
			})
		},
		func() { bq.RemoveClientWaiters("clientA") },
	} {
		wg.Add(1)
		go func(f func()) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					f()
				}
			}
		}(fn)
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}
