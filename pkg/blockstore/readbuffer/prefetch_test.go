package readbuffer

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockLocalChecker implements LocalChecker for tests.
type mockLocalChecker struct {
	mu    sync.Mutex
	local map[blockKey]bool
}

func newMockLocalChecker() *mockLocalChecker {
	return &mockLocalChecker{local: make(map[blockKey]bool)}
}

func (m *mockLocalChecker) IsBlockLocal(_ context.Context, payloadID string, blockIdx uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.local[blockKey{payloadID: payloadID, blockIdx: blockIdx}]
}

func (m *mockLocalChecker) setLocal(payloadID string, blockIdx uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.local[blockKey{payloadID: payloadID, blockIdx: blockIdx}] = true
}

// trackingLoadFn creates a LoadBlockFn that records calls and returns test data.
type loadTracker struct {
	mu    sync.Mutex
	calls []blockKey
	ch    chan struct{} // optional: signaled on each call
}

func newLoadTracker() *loadTracker {
	return &loadTracker{ch: make(chan struct{}, 100)}
}

func (lt *loadTracker) loadFn(_ context.Context, payloadID string, blockIdx uint64) ([]byte, uint32, error) {
	lt.mu.Lock()
	lt.calls = append(lt.calls, blockKey{payloadID: payloadID, blockIdx: blockIdx})
	lt.mu.Unlock()
	select {
	case lt.ch <- struct{}{}:
	default:
	}
	data := makeData(testBlockSize, byte(blockIdx))
	return data, uint32(testBlockSize), nil
}

func (lt *loadTracker) getCalls() []blockKey {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	result := make([]blockKey, len(lt.calls))
	copy(result, lt.calls)
	return result
}

func (lt *loadTracker) waitForCalls(n int, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for i := 0; i < n; i++ {
		select {
		case <-lt.ch:
		case <-deadline:
			return false
		}
	}
	return true
}

// --- OnRead: First read ---

func TestPrefetch_OnRead_FirstRead(t *testing.T) {
	buf := New(testBlockSize * 32)
	require.NotNil(t, buf)
	defer buf.Close()

	tracker := newLoadTracker()
	p := NewPrefetcher(2, buf, tracker.loadFn, nil)
	require.NotNil(t, p)
	defer p.Close()

	p.OnRead("file1", 0)

	// First read should create tracker but not trigger prefetch
	time.Sleep(50 * time.Millisecond)
	calls := tracker.getCalls()
	assert.Empty(t, calls, "first read should not trigger any prefetch")
}

// --- OnRead: Sequential but below threshold ---

func TestPrefetch_OnRead_Sequential(t *testing.T) {
	buf := New(testBlockSize * 32)
	require.NotNil(t, buf)
	defer buf.Close()

	tracker := newLoadTracker()
	p := NewPrefetcher(2, buf, tracker.loadFn, nil)
	require.NotNil(t, p)
	defer p.Close()

	p.OnRead("file1", 0)
	p.OnRead("file1", 1) // seqCount=1, still below threshold=2

	time.Sleep(50 * time.Millisecond)
	calls := tracker.getCalls()
	assert.Empty(t, calls, "two sequential reads should not trigger prefetch (threshold=2)")
}

// --- OnRead: Threshold trigger ---

func TestPrefetch_OnRead_ThresholdTrigger(t *testing.T) {
	buf := New(testBlockSize * 32)
	require.NotNil(t, buf)
	defer buf.Close()

	tracker := newLoadTracker()
	p := NewPrefetcher(2, buf, tracker.loadFn, nil)
	require.NotNil(t, p)
	defer p.Close()

	p.OnRead("file1", 0)
	p.OnRead("file1", 1)
	p.OnRead("file1", 2) // seqCount=2 == threshold, triggers prefetch depth=1

	ok := tracker.waitForCalls(1, 2*time.Second)
	require.True(t, ok, "expected at least 1 prefetch call")

	calls := tracker.getCalls()
	assert.Equal(t, blockKey{payloadID: "file1", blockIdx: 3}, calls[0],
		"prefetch should load block 3 (next after current)")
}

// --- OnRead: Adaptive depth ---

func TestPrefetch_OnRead_AdaptiveDepth(t *testing.T) {
	buf := New(testBlockSize * 64)
	require.NotNil(t, buf)
	defer buf.Close()

	tracker := newLoadTracker()
	p := NewPrefetcher(4, buf, tracker.loadFn, nil)
	require.NotNil(t, p)
	defer p.Close()

	// Build up: blocks 0,1,2 trigger depth=1 prefetch of block 3
	p.OnRead("file1", 0)
	p.OnRead("file1", 1)
	p.OnRead("file1", 2) // depth=1, prefetch block 3
	tracker.waitForCalls(1, 2*time.Second)

	// Block 3: depth doubles to 2, prefetch blocks 4,5
	p.OnRead("file1", 3)
	tracker.waitForCalls(2, 2*time.Second) // 2 more calls

	// Block 4: depth doubles to 4, prefetch blocks 5,6,7,8
	p.OnRead("file1", 4)
	// Wait for workers to process
	time.Sleep(200 * time.Millisecond)

	calls := tracker.getCalls()
	// First trigger: block 3
	// Second trigger: blocks 4,5 (but 4 is current read +1, so 4 and 5)
	// Third trigger: blocks 5,6,7,8 (depth=4)
	// Some may be skipped if already cached from previous prefetch
	assert.True(t, len(calls) >= 3, "expected adaptive depth to produce multiple prefetch calls, got %d", len(calls))
}

// --- OnRead: Max depth ---

func TestPrefetch_OnRead_MaxDepth(t *testing.T) {
	buf := New(testBlockSize * 64)
	require.NotNil(t, buf)
	defer buf.Close()

	tracker := newLoadTracker()
	p := NewPrefetcher(4, buf, tracker.loadFn, nil)
	require.NotNil(t, p)
	defer p.Close()

	// Feed many sequential reads to max out depth
	for i := uint64(0); i < 20; i++ {
		p.OnRead("file1", i)
	}

	// Wait for all prefetch operations
	time.Sleep(500 * time.Millisecond)

	// Verify depth is capped by checking the tracker state
	p.mu.Lock()
	tr := p.trackers["file1"]
	p.mu.Unlock()

	require.NotNil(t, tr)
	assert.LessOrEqual(t, tr.depth, maxPrefetchDepth, "depth should not exceed maxPrefetchDepth")
}

// --- OnRead: Non-sequential reset ---

func TestPrefetch_OnRead_NonSequentialReset(t *testing.T) {
	buf := New(testBlockSize * 32)
	require.NotNil(t, buf)
	defer buf.Close()

	tracker := newLoadTracker()
	p := NewPrefetcher(2, buf, tracker.loadFn, nil)
	require.NotNil(t, p)
	defer p.Close()

	p.OnRead("file1", 0)
	p.OnRead("file1", 1)
	// Now a non-sequential read
	p.OnRead("file1", 10)

	time.Sleep(50 * time.Millisecond)
	calls := tracker.getCalls()
	assert.Empty(t, calls, "non-sequential read should reset tracker, no prefetch")

	// Verify tracker was reset
	p.mu.Lock()
	tr := p.trackers["file1"]
	p.mu.Unlock()

	require.NotNil(t, tr)
	assert.Equal(t, 0, tr.seqCount)
	assert.Equal(t, 1, tr.depth)
}

// --- OnRead: Skips cached blocks ---

func TestPrefetch_OnRead_SkipsCachedBlocks(t *testing.T) {
	buf := New(testBlockSize * 32)
	require.NotNil(t, buf)
	defer buf.Close()

	// Pre-populate read buffer with block 3
	buf.Put("file1", 3, makeData(testBlockSize, 0x33), uint32(testBlockSize))

	tracker := newLoadTracker()
	p := NewPrefetcher(2, buf, tracker.loadFn, nil)
	require.NotNil(t, p)
	defer p.Close()

	p.OnRead("file1", 0)
	p.OnRead("file1", 1)
	p.OnRead("file1", 2) // triggers prefetch of block 3 -- but 3 is in read buffer

	time.Sleep(200 * time.Millisecond)
	calls := tracker.getCalls()
	// Block 3 is already in the read buffer, so loadFn should NOT be called for it
	for _, c := range calls {
		assert.NotEqual(t, uint64(3), c.blockIdx, "should not load block 3 (already buffered)")
	}
}

// --- OnRead: Skips local blocks ---

func TestPrefetch_OnRead_SkipsLocalBlocks(t *testing.T) {
	buf := New(testBlockSize * 32)
	require.NotNil(t, buf)
	defer buf.Close()

	local := newMockLocalChecker()
	local.setLocal("file1", 3) // block 3 is on local disk

	tracker := newLoadTracker()
	p := NewPrefetcher(2, buf, tracker.loadFn, local)
	require.NotNil(t, p)
	defer p.Close()

	p.OnRead("file1", 0)
	p.OnRead("file1", 1)
	p.OnRead("file1", 2) // triggers prefetch depth=1 -> block 3

	time.Sleep(200 * time.Millisecond)
	calls := tracker.getCalls()
	// Block 3 is on local disk, worker should skip calling loadFn for it
	for _, c := range calls {
		assert.NotEqual(t, uint64(3), c.blockIdx, "should not load block 3 (on local disk)")
	}
}

// --- Reset ---

func TestPrefetch_Reset_ClearsTracker(t *testing.T) {
	buf := New(testBlockSize * 32)
	require.NotNil(t, buf)
	defer buf.Close()

	tracker := newLoadTracker()
	p := NewPrefetcher(2, buf, tracker.loadFn, nil)
	require.NotNil(t, p)
	defer p.Close()

	p.OnRead("file1", 0)
	p.OnRead("file1", 1)
	p.Reset("file1")

	p.mu.Lock()
	_, exists := p.trackers["file1"]
	p.mu.Unlock()

	assert.False(t, exists, "tracker should be removed after Reset")
}

func TestPrefetch_Reset_UnknownPayload(t *testing.T) {
	buf := New(testBlockSize * 32)
	require.NotNil(t, buf)
	defer buf.Close()

	tracker := newLoadTracker()
	p := NewPrefetcher(2, buf, tracker.loadFn, nil)
	require.NotNil(t, p)
	defer p.Close()

	// Should not panic
	p.Reset("nonexistent")
}

// --- Bounded pool ---

func TestPrefetch_BoundedPool_DropsWhenFull(t *testing.T) {
	buf := New(testBlockSize * 64)
	require.NotNil(t, buf)
	defer buf.Close()

	var started atomic.Int32
	blocker := make(chan struct{})

	// Slow loadFn that blocks until released
	slowLoadFn := func(_ context.Context, payloadID string, blockIdx uint64) ([]byte, uint32, error) {
		started.Add(1)
		<-blocker
		return makeData(testBlockSize, byte(blockIdx)), uint32(testBlockSize), nil
	}

	// 1 worker, channel capacity = 1*4 = 4
	p := NewPrefetcher(1, buf, slowLoadFn, nil)
	require.NotNil(t, p)

	// Submit many prefetch requests through OnRead
	// Fill up the channel and verify some are dropped
	for i := 0; i < 20; i++ {
		p.submit("file1", uint64(i))
	}

	// Give workers time to pick up items
	time.Sleep(50 * time.Millisecond)

	// Unblock
	close(blocker)
	p.Close()

	// The worker should have processed some but not all 20
	// (1 in-flight + 4 buffered = 5 max, rest dropped)
	processed := int(started.Load())
	assert.Less(t, processed, 20, "bounded pool should have dropped some requests")
	assert.Greater(t, processed, 0, "at least some requests should have been processed")
}

// --- Close ---

func TestPrefetch_Close_StopsWorkers(t *testing.T) {
	buf := New(testBlockSize * 32)
	require.NotNil(t, buf)
	defer buf.Close()

	tracker := newLoadTracker()
	p := NewPrefetcher(2, buf, tracker.loadFn, nil)
	require.NotNil(t, p)

	p.Close()

	// After close, OnRead should not panic and not submit
	p.OnRead("file1", 0)
}

func TestPrefetch_Close_Idempotent(t *testing.T) {
	buf := New(testBlockSize * 32)
	require.NotNil(t, buf)
	defer buf.Close()

	tracker := newLoadTracker()
	p := NewPrefetcher(2, buf, tracker.loadFn, nil)
	require.NotNil(t, p)

	// Calling Close twice should not panic
	p.Close()
	p.Close()
}

// --- Concurrency ---

func TestPrefetch_Concurrency_OnReadMultiFiles(t *testing.T) {
	buf := New(testBlockSize * 64)
	require.NotNil(t, buf)
	defer buf.Close()

	tracker := newLoadTracker()
	p := NewPrefetcher(4, buf, tracker.loadFn, nil)
	require.NotNil(t, p)
	defer p.Close()

	const goroutines = 8
	const reads = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			pid := "file" + string(rune('A'+id))
			for i := uint64(0); i < reads; i++ {
				p.OnRead(pid, i)
			}
		}(g)
	}

	wg.Wait()
}

// --- NewPrefetcher edge cases ---

func TestPrefetch_NewPrefetcher_NilBuffer(t *testing.T) {
	tracker := newLoadTracker()
	p := NewPrefetcher(2, nil, tracker.loadFn, nil)
	assert.Nil(t, p, "NewPrefetcher with nil read buffer should return nil")
}

func TestPrefetch_NewPrefetcher_DefaultWorkers(t *testing.T) {
	buf := New(testBlockSize * 32)
	require.NotNil(t, buf)
	defer buf.Close()

	tracker := newLoadTracker()
	p := NewPrefetcher(0, buf, tracker.loadFn, nil)
	require.NotNil(t, p, "NewPrefetcher with workers=0 should default to defaultPrefetchWorkers")
	defer p.Close()
}
