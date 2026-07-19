package metadata

import (
	"sync"
	"testing"
)

// TestLockCreateName_SameKeySerializes verifies that lockCreateName mutually
// excludes concurrent creators of the same (parent, name): many goroutines
// increment a plain (unsynchronized) counter under the lock and the result must
// be exact. Run under -race, a broken shard mapping that handed out different
// mutexes for the same key would both trip the race detector and skew the count.
// (Distinct names may share a shard by design, so no non-serialization property
// is asserted here — only the correctness-critical same-key exclusion.)
func TestLockCreateName_SameKeySerializes(t *testing.T) {
	t.Parallel()

	s := New()
	parent := FileHandle("parent-handle")

	const n = 200
	var counter int
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			unlock := s.lockCreateName(parent, "same")
			defer unlock()
			counter++ // guarded solely by lockCreateName
		}()
	}
	wg.Wait()

	if counter != n {
		t.Fatalf("lockCreateName did not serialize same (parent,name): got %d, want %d", counter, n)
	}
}
