package engine

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// snapshotMapLen reads len(gcRootLocks) under gcRootLocksMu.
func snapshotMapLen() int {
	gcRootLocksMu.Lock()
	defer gcRootLocksMu.Unlock()
	return len(gcRootLocks)
}

// TestGCRootLock_MapShrinksAfterRelease verifies the refcount cleanup
// mechanism: after acquire+release across many distinct keys, the map
// returns to its pre-test size.
func TestGCRootLock_MapShrinksAfterRelease(t *testing.T) {
	startLen := snapshotMapLen()
	const n = 100
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("/tmp/dittofs-gc-test-%d", i)
		lock := acquireGCRootLock(key)
		releaseGCRootLock(key, lock)
	}
	if got := snapshotMapLen(); got != startLen {
		t.Fatalf("gcRootLocks map did not shrink: start=%d after=%d", startLen, got)
	}
}

// TestGCRootLock_RefcountSerializesSameKey verifies that two goroutines
// acquiring the same key serialize on the underlying mutex, and that
// the map entry is reclaimed once both release.
func TestGCRootLock_RefcountSerializesSameKey(t *testing.T) {
	startLen := snapshotMapLen()
	const key = "/tmp/dittofs-gc-test-serialize"

	var holders int32
	var maxConcurrent int32

	criticalSection := func(t *testing.T) {
		cur := atomic.AddInt32(&holders, 1)
		for {
			peak := atomic.LoadInt32(&maxConcurrent)
			if cur <= peak || atomic.CompareAndSwapInt32(&maxConcurrent, peak, cur) {
				break
			}
		}
		// Hold briefly so the second goroutine has time to block in
		// acquireGCRootLock if mutual exclusion were broken.
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&holders, -1)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			<-start
			lock := acquireGCRootLock(key)
			defer releaseGCRootLock(key, lock)
			criticalSection(t)
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(&maxConcurrent); got != 1 {
		t.Fatalf("expected mutual exclusion (maxConcurrent=1), got %d", got)
	}
	if got := snapshotMapLen(); got != startLen {
		t.Fatalf("map entry not reclaimed: start=%d after=%d", startLen, got)
	}
}

// TestGCRootLock_EmptyKey_SerializesAcrossCallers verifies the
// documented behavior that callers passing an empty GCStateRoot (the
// temp-root variant) share a single mutex.
func TestGCRootLock_EmptyKey_SerializesAcrossCallers(t *testing.T) {
	startLen := snapshotMapLen()

	// Acquire the empty key twice — second acquire must hit the same
	// map entry with refCount==2 and block on the mutex.
	a := acquireGCRootLock("")

	gcRootLocksMu.Lock()
	entry, ok := gcRootLocks[""]
	gcRootLocksMu.Unlock()
	if !ok {
		t.Fatal("empty key not present in gcRootLocks after acquire")
	}
	if entry != a {
		t.Fatal("empty-key entry differs from returned lock")
	}

	// Spawn a second acquirer that should block until we release.
	acquired := make(chan *gcRootLock, 1)
	go func() {
		b := acquireGCRootLock("")
		acquired <- b
	}()

	select {
	case <-acquired:
		t.Fatal("second acquireGCRootLock(\"\") did not block — empty key not serializing")
	case <-time.After(50 * time.Millisecond):
		// expected: blocked on the shared mutex
	}

	// While the second acquirer is parked, refCount must reflect both
	// holders so release-from-first does not drop the entry from the map.
	gcRootLocksMu.Lock()
	rc := entry.refCount
	gcRootLocksMu.Unlock()
	if rc != 2 {
		t.Fatalf("expected refCount=2 with one holder + one waiter, got %d", rc)
	}

	releaseGCRootLock("", a)

	var b *gcRootLock
	select {
	case b = <-acquired:
	case <-time.After(time.Second):
		t.Fatal("second acquirer never unblocked after release")
	}
	if b != entry {
		t.Fatal("second acquirer received a different lock object — empty key not shared")
	}
	releaseGCRootLock("", b)

	if got := snapshotMapLen(); got != startLen {
		t.Fatalf("empty-key entry not reclaimed: start=%d after=%d", startLen, got)
	}
}
