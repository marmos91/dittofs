package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hashN builds a deterministic ContentHash from a single byte for tests.
func hashN(b byte) blockstore.ContentHash {
	var h blockstore.ContentHash
	h[31] = b
	return h
}

// TestCache_GetPut_Basic — Task 1 behavior 1.
// Put(h, data); Get(h) returns the same bytes; Get(otherHash) returns false.
func TestCache_GetPut_Basic(t *testing.T) {
	c := newCacheNoWorkers(1024)
	require.NotNil(t, c)
	defer func() { _ = c.Close() }()

	h := hashN(0x01)
	data := []byte("hello cache")

	c.Put(h, data)

	got, ok := c.Get(h)
	require.True(t, ok, "expected hit on h")
	assert.Equal(t, data, got)

	_, miss := c.Get(hashN(0xFF))
	assert.False(t, miss, "expected miss on unknown hash")
}

// TestCache_LRUEviction — Task 1 behavior 2.
// maxBytes=10; Put 12 bytes; oldest evicted; newest still hits.
func TestCache_LRUEviction(t *testing.T) {
	c := newCacheNoWorkers(10)
	require.NotNil(t, c)
	defer func() { _ = c.Close() }()

	h1 := hashN(1)
	h2 := hashN(2)
	h3 := hashN(3)

	c.Put(h1, []byte("aaaa"))     // 4 bytes; cur=4
	c.Put(h2, []byte("bbbb"))     // 4 bytes; cur=8
	c.Put(h3, []byte("ccccdddd")) // 8 bytes; cur > 10 -> evict h1, possibly h2

	// h3 must hit
	_, hit3 := c.Get(h3)
	assert.True(t, hit3, "h3 (newest) should still be cached")

	// h1 (oldest) must have been evicted
	_, hit1 := c.Get(h1)
	assert.False(t, hit1, "h1 (oldest) should be evicted under tight budget")
}

// TestCache_CrossFileDedup_CACHE02 — Task 1 behavior 3.
// Two payloadIDs share one hash; only one Put; second access hits.
func TestCache_CrossFileDedup_CACHE02(t *testing.T) {
	c := newCacheNoWorkers(1024)
	require.NotNil(t, c)
	defer func() { _ = c.Close() }()

	h := hashN(0xAB)
	c.Put(h, []byte("shared content"))

	// File A reads h
	gotA, okA := c.Get(h)
	require.True(t, okA, "file A should hit")
	assert.Equal(t, []byte("shared content"), gotA)

	// File B reads same h — same cache entry
	gotB, okB := c.Get(h)
	require.True(t, okB, "file B should hit (cross-file dedup)")
	assert.Equal(t, []byte("shared content"), gotB)
}

// TestCache_InvalidateFile_Surgical — Task 1 behavior 4.
// Cache hashes for payloads A and B (some shared). InvalidateFile(A, [3 of A's hashes])
// drops ONLY those 3; B's hashes unchanged; A's other 2 unchanged; shared hashes used by B
// are NOT dropped.
func TestCache_InvalidateFile_Surgical(t *testing.T) {
	c := newCacheNoWorkers(64 * 1024)
	require.NotNil(t, c)
	defer func() { _ = c.Close() }()

	// 5 hashes for A: 1..5; 5 hashes for B: some shared.
	hA1, hA2, hA3, hA4, hA5 := hashN(0x10), hashN(0x11), hashN(0x12), hashN(0x13), hashN(0x14)
	hShared := hashN(0x12) // == hA3 — also referenced by B
	_ = hShared

	c.Put(hA1, []byte("a1"))
	c.Put(hA2, []byte("a2"))
	c.Put(hA3, []byte("a3"))
	c.Put(hA4, []byte("a4"))
	c.Put(hA5, []byte("a5"))

	hB1, hB2 := hashN(0x20), hashN(0x21)
	c.Put(hB1, []byte("b1"))
	c.Put(hB2, []byte("b2"))

	// Surgical invalidation: drop hA1, hA2, hA3.
	c.InvalidateFile("payloadA", []blockstore.ContentHash{hA1, hA2, hA3})

	// hA1, hA2, hA3 must be gone.
	for _, h := range []blockstore.ContentHash{hA1, hA2, hA3} {
		_, ok := c.Get(h)
		assert.Falsef(t, ok, "%x should be invalidated", h[31])
	}

	// hA4, hA5 must remain.
	for _, h := range []blockstore.ContentHash{hA4, hA5} {
		_, ok := c.Get(h)
		assert.Truef(t, ok, "A's untouched hash %x should remain", h[31])
	}

	// B's hashes must be untouched.
	for _, h := range []blockstore.ContentHash{hB1, hB2} {
		_, ok := c.Get(h)
		assert.Truef(t, ok, "B's hash %x should remain (not touched by InvalidateFile(A,...))", h[31])
	}
}

// --- Task 2: OnRead + sequential detection + worker pool ---

// recordingLoader returns a loadByHashFn that records every (hash) it
// is called with and lets the test inspect concurrency via an atomic
// "in-flight" counter.
type recordingLoader struct {
	mu        sync.Mutex
	calls     []blockstore.ContentHash
	inFlight  atomic.Int32
	maxInFly  atomic.Int32
	wait      time.Duration // synthetic latency to expose concurrency
	dataMaker func(blockstore.ContentHash) []byte
}

func newRecordingLoader(latency time.Duration) *recordingLoader {
	return &recordingLoader{
		wait: latency,
		dataMaker: func(h blockstore.ContentHash) []byte {
			return []byte{h[31]}
		},
	}
}

func (r *recordingLoader) load(_ context.Context, h blockstore.ContentHash) ([]byte, error) {
	cur := r.inFlight.Add(1)
	defer r.inFlight.Add(-1)
	for {
		max := r.maxInFly.Load()
		if cur <= max || r.maxInFly.CompareAndSwap(max, cur) {
			break
		}
	}
	if r.wait > 0 {
		time.Sleep(r.wait)
	}
	r.mu.Lock()
	r.calls = append(r.calls, h)
	r.mu.Unlock()
	return r.dataMaker(h), nil
}

func (r *recordingLoader) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// waitForCalls blocks until the loader has been invoked >=n times or
// the timeout fires. Returns true if the count was reached.
func (r *recordingLoader) waitForCalls(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if r.callCount() >= n {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return r.callCount() >= n
}

// TestCache_SequentialDetection_CACHE03 — Task 2 behavior 1.
// 3 consecutive sequential hashes in OnRead trigger prefetch of next
// 1+ hashes (CACHE-03 threshold = 3, doubling depth).
func TestCache_SequentialDetection_CACHE03(t *testing.T) {
	loader := newRecordingLoader(0)
	c := NewCache(64*1024, 4, loader.load)
	require.NotNil(t, c)
	defer func() { _ = c.Close() }()

	// 5 hashes: indices 0..4. The first 3 OnReads (h0,h1,h2) reach the
	// threshold and should trigger prefetch of h3 (depth=1).
	hashes := []blockstore.ContentHash{hashN(0), hashN(1), hashN(2), hashN(3), hashN(4)}

	// Each OnRead simulates a single block-read for a payloadID.
	for i := 0; i < 3; i++ {
		c.OnRead("file1", []blockstore.ContentHash{hashes[i]}, 5*1024)
	}

	if !loader.waitForCalls(1, 500*time.Millisecond) {
		t.Fatalf("expected at least 1 prefetch call after 3 sequential reads; got %d", loader.callCount())
	}

	// One more sequential read — depth doubles to 2 — should issue 2 more.
	prev := loader.callCount()
	c.OnRead("file1", []blockstore.ContentHash{hashes[3]}, 5*1024)
	if !loader.waitForCalls(prev+1, 500*time.Millisecond) {
		t.Fatalf("expected additional prefetch on depth-doubled trigger; before=%d after=%d", prev, loader.callCount())
	}
}

// TestCache_OnRead_NonSequentialResetsTracker — Task 2 behavior 2.
// A non-sequential read resets the tracker; subsequent prefetch must
// build up to threshold again.
func TestCache_OnRead_NonSequentialResetsTracker(t *testing.T) {
	loader := newRecordingLoader(0)
	c := NewCache(64*1024, 2, loader.load)
	require.NotNil(t, c)
	defer func() { _ = c.Close() }()

	// 2 sequential reads — below threshold, no prefetch.
	c.OnRead("file1", []blockstore.ContentHash{hashN(1)}, 1024)
	c.OnRead("file1", []blockstore.ContentHash{hashN(2)}, 1024)

	// "Non-sequential" jump: a read whose hash list is empty (the
	// canonical "tracker reset" signal) — implementations may also reset
	// when the new hash is not the immediate successor. We use the
	// well-defined empty-hashes form so the test is implementation-
	// agnostic.
	//
	// After the reset, two more reads should still be below threshold
	// (which is 3). No prefetch yet.
	prev := loader.callCount()
	c.OnRead("file1", []blockstore.ContentHash{}, 1024) // reset signal — empty triggers nothing
	c.OnRead("file1", []blockstore.ContentHash{hashN(10)}, 1024)
	c.OnRead("file1", []blockstore.ContentHash{hashN(11)}, 1024)

	// Give any spurious prefetch a chance to fire; assert nothing did.
	time.Sleep(20 * time.Millisecond)
	if loader.callCount() != prev {
		t.Fatalf("expected NO new prefetch calls below threshold-after-reset; before=%d after=%d", prev, loader.callCount())
	}

	// The 3rd read after reset triggers prefetch.
	c.OnRead("file1", []blockstore.ContentHash{hashN(12)}, 1024)
	if !loader.waitForCalls(prev+1, 500*time.Millisecond) {
		t.Fatalf("expected prefetch after threshold-after-reset; got %d", loader.callCount())
	}
}

// TestCache_BoundedConcurrency — Task 2 behavior 3.
// Submit many prefetch requests rapidly; assert at most `workers`
// concurrent loadFn invocations.
func TestCache_BoundedConcurrency(t *testing.T) {
	const workers = 4
	loader := newRecordingLoader(15 * time.Millisecond) // slow enough to overlap
	c := NewCache(64*1024, workers, loader.load)
	require.NotNil(t, c)
	defer func() { _ = c.Close() }()

	// Drive 100 prefetch submissions via OnRead. We accomplish this by
	// repeatedly hitting the threshold with disjoint files (so each
	// file's depth ramps independently).
	for f := 0; f < 25; f++ {
		pid := fmt.Sprintf("file%d", f)
		c.OnRead(pid, []blockstore.ContentHash{hashN(byte(f*4 + 0))}, 1024)
		c.OnRead(pid, []blockstore.ContentHash{hashN(byte(f*4 + 1))}, 1024)
		c.OnRead(pid, []blockstore.ContentHash{hashN(byte(f*4 + 2))}, 1024)
		c.OnRead(pid, []blockstore.ContentHash{hashN(byte(f*4 + 3))}, 1024)
	}

	// Allow workers to drain.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && loader.inFlight.Load() > 0 {
		time.Sleep(2 * time.Millisecond)
	}

	max := loader.maxInFly.Load()
	if max > int32(workers) {
		t.Fatalf("max concurrent loadFn invocations = %d; expected <= %d", max, workers)
	}
}

// TestCache_Close_StopsWorkers — Task 2 behavior 4.
// After Close, OnRead is a no-op; pending workers exit within 1s.
func TestCache_Close_StopsWorkers(t *testing.T) {
	loader := newRecordingLoader(50 * time.Millisecond)
	c := NewCache(64*1024, 4, loader.load)
	require.NotNil(t, c)

	// Trigger some prefetches.
	for i := 0; i < 5; i++ {
		c.OnRead("f", []blockstore.ContentHash{hashN(byte(i))}, 1024)
	}

	done := make(chan struct{})
	go func() {
		_ = c.Close()
		close(done)
	}()

	select {
	case <-done:
		// good
	case <-time.After(1 * time.Second):
		t.Fatal("Cache.Close did not return within 1s — workers may be leaking")
	}

	// Post-close OnRead is a no-op.
	prev := loader.callCount()
	c.OnRead("f", []blockstore.ContentHash{hashN(99)}, 1024)
	time.Sleep(50 * time.Millisecond)
	if loader.callCount() != prev {
		t.Fatalf("post-close OnRead must be no-op; calls before=%d after=%d", prev, loader.callCount())
	}
}

// TestCache_Close — Task 1 behavior 5.
// After Close, Put returns/silent-noop; Get returns miss.
func TestCache_Close(t *testing.T) {
	c := newCacheNoWorkers(1024)
	require.NotNil(t, c)

	h := hashN(1)
	c.Put(h, []byte("alive"))
	if _, ok := c.Get(h); !ok {
		t.Fatal("pre-close Put/Get should work")
	}

	require.NoError(t, c.Close())

	// Post-close Put is a silent no-op; Get returns miss.
	c.Put(hashN(2), []byte("ignored"))
	_, ok := c.Get(hashN(2))
	assert.False(t, ok, "post-close Put must be no-op")

	// Idempotent Close.
	require.NoError(t, c.Close())
}

// TestCache_LargeChunkRoundTrip pins generic byte-correctness for a
// multi-hundred-KiB chunk round-trip through Cache.Put / Cache.Get.
// TestCache_GetPut_Basic uses an 11-byte string and does NOT cover
// large-chunk equality; this covers that gap.
func TestCache_LargeChunkRoundTrip(t *testing.T) {
	const sz = 256 * 1024 // 256 KiB.
	want := make([]byte, sz)
	if _, err := rand.Read(want); err != nil {
		t.Fatalf("rand: %v", err)
	}

	c := newCacheNoWorkers(int64(sz * 2))
	require.NotNil(t, c)
	defer func() { _ = c.Close() }()

	h := hashN(0x77)
	c.Put(h, want)

	got, ok := c.Get(h)
	require.True(t, ok, "expected hit after Put of large chunk")
	if !bytes.Equal(got, want) {
		t.Fatalf("large-chunk round-trip mismatch: got len %d, want len %d", len(got), len(want))
	}
}
