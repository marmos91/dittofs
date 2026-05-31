package blocking

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

func newWaiter(caller, ownerID string, off uint64) *Waiter {
	return &Waiter{
		Lock: &lock.UnifiedLock{
			Owner:  lock.LockOwner{OwnerID: ownerID},
			Offset: off,
			Length: 10,
		},
		CallerName: caller,
	}
}

func TestBlockingQueue_RemoveClientWaiters(t *testing.T) {
	t.Parallel()

	bq := NewBlockingQueue(100)

	// clientA queues waiters on two files; clientB queues one on file1.
	if err := bq.Enqueue("file1", newWaiter("clientA", "nlm:clientA:1:aa", 0)); err != nil {
		t.Fatal(err)
	}
	if err := bq.Enqueue("file2", newWaiter("clientA", "nlm:clientA:2:bb", 0)); err != nil {
		t.Fatal(err)
	}
	bWaiter := newWaiter("clientB", "nlm:clientB:1:cc", 50)
	if err := bq.Enqueue("file1", bWaiter); err != nil {
		t.Fatal(err)
	}

	if got := bq.TotalWaiters(); got != 3 {
		t.Fatalf("want 3 waiters before drain, got %d", got)
	}

	removed := bq.RemoveClientWaiters("clientA")
	if removed != 2 {
		t.Fatalf("want 2 waiters drained, got %d", removed)
	}

	// Only clientB's waiter remains, and it is not cancelled.
	if got := bq.TotalWaiters(); got != 1 {
		t.Fatalf("want 1 waiter after drain, got %d", got)
	}
	if bWaiter.IsCancelled() {
		t.Fatal("clientB waiter must not be cancelled")
	}
	rem := bq.GetWaiters("file1")
	if len(rem) != 1 || rem[0] != bWaiter {
		t.Fatalf("file1 should retain only clientB waiter, got %+v", rem)
	}
	// file2 had only clientA, so it must be fully removed.
	if got := bq.GetWaiters("file2"); got != nil {
		t.Fatalf("file2 should be empty after drain, got %+v", got)
	}
}

func TestBlockingQueue_RemoveClientWaiters_CancelsDrained(t *testing.T) {
	t.Parallel()

	bq := NewBlockingQueue(100)
	w := newWaiter("clientA", "nlm:clientA:1:aa", 0)
	if err := bq.Enqueue("file1", w); err != nil {
		t.Fatal(err)
	}

	if w.IsCancelled() {
		t.Fatal("waiter should not be cancelled before drain")
	}
	if removed := bq.RemoveClientWaiters("clientA"); removed != 1 {
		t.Fatalf("want 1 removed, got %d", removed)
	}
	if !w.IsCancelled() {
		t.Fatal("drained waiter must be marked cancelled so in-flight grants bail")
	}
}

func TestBlockingQueue_RemoveClientWaiters_NoMatchIsSafe(t *testing.T) {
	t.Parallel()

	bq := NewBlockingQueue(100)
	if err := bq.Enqueue("file1", newWaiter("clientA", "nlm:clientA:1:aa", 0)); err != nil {
		t.Fatal(err)
	}

	if removed := bq.RemoveClientWaiters("ghost"); removed != 0 {
		t.Fatalf("want 0 removed for unknown client, got %d", removed)
	}
	if got := bq.TotalWaiters(); got != 1 {
		t.Fatalf("untouched waiter should remain, got %d", got)
	}

	empty := NewBlockingQueue(100)
	if removed := empty.RemoveClientWaiters("anyone"); removed != 0 {
		t.Fatalf("want 0 removed on empty queue, got %d", removed)
	}
}
