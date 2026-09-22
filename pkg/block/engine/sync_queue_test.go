package engine

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestSyncQueue_EnqueuePrefetchCounts(t *testing.T) {
	cfg := DefaultSyncQueueConfig()
	cfg.QueueSize = 10
	q := NewSyncQueue(nil, cfg)

	for i := 0; i < 5; i++ {
		req := TransferRequest{
			PayloadID:  "test-content",
			BlockIndex: uint64(i),
		}
		if !q.EnqueuePrefetch(req) {
			t.Errorf("EnqueuePrefetch(%d) returned false", i)
		}
	}

	if q.Pending() != 5 {
		t.Errorf("Pending() = %d, want 5", q.Pending())
	}
}

func TestSyncQueue_QueueFull(t *testing.T) {
	cfg := SyncQueueConfig{
		QueueSize: 2,
	}
	q := NewSyncQueue(nil, cfg)
	// Don't start workers - queue will fill up

	req1 := TransferRequest{PayloadID: "c1", BlockIndex: 0}
	req2 := TransferRequest{PayloadID: "c2", BlockIndex: 0}
	req3 := TransferRequest{PayloadID: "c3", BlockIndex: 0}

	if !q.EnqueuePrefetch(req1) {
		t.Error("EnqueuePrefetch(1) should succeed")
	}
	if !q.EnqueuePrefetch(req2) {
		t.Error("EnqueuePrefetch(2) should succeed")
	}
	if q.EnqueuePrefetch(req3) {
		t.Error("EnqueuePrefetch(3) should fail (queue full)")
	}

	if q.Pending() != 2 {
		t.Errorf("Pending() = %d, want 2", q.Pending())
	}
}

func TestSyncQueue_StopNotStarted(t *testing.T) {
	cfg := DefaultSyncQueueConfig()
	q := NewSyncQueue(nil, cfg)

	// Stop without starting - should not panic
	q.Stop(time.Second)
}

func TestSyncQueue_DoubleStart(t *testing.T) {
	cfg := DefaultSyncQueueConfig()
	q := NewSyncQueue(nil, cfg)

	ctx := context.Background()
	q.Start(ctx)
	q.Start(ctx) // Should be a no-op

	q.Stop(time.Second)
}

// TestSyncQueue_StopConcurrentAndRepeated asserts that Stop() is safe to call
// concurrently and more than once: the sync.Once guard around close(stopCh)
// must prevent a double-close panic. Run under -race.
func TestSyncQueue_StopConcurrentAndRepeated(t *testing.T) {
	cfg := DefaultSyncQueueConfig()
	q := NewSyncQueue(nil, cfg)

	ctx := context.Background()
	q.Start(ctx)

	const callers = 8
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			q.Stop(time.Second) // must not panic on double-close(stopCh)
		}()
	}
	wg.Wait()

	// A further sequential Stop after all goroutines returned must also be a
	// no-op rather than panicking.
	q.Stop(time.Second)
}

// TestSyncQueue_StopCancelsWorkerCtx asserts that closing stopCh propagates
// cancellation to the workerCtx used by processRequest. Without this, an
// in-flight processDownload would block on a hung remote and leak the worker
// goroutine past Close()/Stop() deadlines.
func TestSyncQueue_StopCancelsWorkerCtx(t *testing.T) {
	cfg := DefaultSyncQueueConfig()
	q := NewSyncQueue(nil, cfg)

	ctx := context.Background()
	q.Start(ctx)

	wctx := q.workerCtx
	if wctx == nil {
		t.Fatal("workerCtx is nil after Start")
	}

	q.Stop(time.Second)

	select {
	case <-wctx.Done():
	case <-time.After(time.Second):
		t.Fatal("workerCtx was not cancelled within 1s of Stop")
	}
}

func TestSyncQueueConfig_Defaults(t *testing.T) {
	cfg := DefaultSyncQueueConfig()

	if cfg.QueueSize != 1000 {
		t.Errorf("default QueueSize = %d, want 1000", cfg.QueueSize)
	}
	if cfg.DownloadWorkers != DefaultParallelDownloads {
		t.Errorf("default DownloadWorkers = %d, want %d", cfg.DownloadWorkers, DefaultParallelDownloads)
	}
}

func TestNewSyncQueue_InvalidConfig(t *testing.T) {
	// Test with invalid config values - should use defaults
	cfg := SyncQueueConfig{
		QueueSize: -1,
	}
	q := NewSyncQueue(nil, cfg)

	// Queue should have been created with defaults
	if cap(q.prefetch) != 1000 {
		t.Errorf("prefetch queue capacity = %d, want 1000", cap(q.prefetch))
	}
	if q.downloadWorkers != DefaultParallelDownloads {
		t.Errorf("downloadWorkers = %d, want %d", q.downloadWorkers, DefaultParallelDownloads)
	}
}

func TestSyncQueue_PrefetchDropWhenFull(t *testing.T) {
	cfg := SyncQueueConfig{
		QueueSize: 1,
	}
	q := NewSyncQueue(nil, cfg)
	// Don't start workers - queue will fill up

	// First prefetch should succeed
	if !q.EnqueuePrefetch(TransferRequest{PayloadID: "payload", BlockIndex: 0}) {
		t.Error("First prefetch should succeed")
	}

	// Second prefetch should be dropped silently (queue full)
	// This should NOT return false but drop silently - check pending count
	q.EnqueuePrefetch(TransferRequest{PayloadID: "payload", BlockIndex: 1})

	// Only 1 should be pending (second was dropped)
	if pending := q.Pending(); pending != 1 {
		t.Errorf("prefetch pending = %d, want 1 (second should be dropped)", pending)
	}
}
