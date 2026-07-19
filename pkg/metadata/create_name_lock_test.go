package metadata

import (
	"sync"
	"testing"
)

// TestLockCreateName_SameKeySerializes verifies that lockCreateName mutually
// excludes concurrent creators of the same (parent, name): many goroutines each
// increment a plain (unsynchronized) counter under the lock, many times, and the
// total must be exact. All goroutines block on a start barrier so they contend
// from the same instant, and each loops the guarded increment so a broken shard
// mapping (different mutexes for the same key) loses updates and skews the count
// even without the race detector; -race also flags it directly.
// (Distinct names may share a shard by design, so no non-serialization property
// is asserted here — only the correctness-critical same-key exclusion.)
func TestLockCreateName_SameKeySerializes(t *testing.T) {
	t.Parallel()

	s := New()
	parent := FileHandle("parent-handle")

	const (
		goroutines = 100
		iters      = 50
	)
	start := make(chan struct{})
	var counter int
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start // release all goroutines together to maximize contention
			for j := 0; j < iters; j++ {
				unlock := s.lockCreateName(parent, "same")
				counter++ // guarded solely by lockCreateName
				unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if want := goroutines * iters; counter != want {
		t.Fatalf("lockCreateName did not serialize same (parent,name): got %d, want %d", counter, want)
	}
}
