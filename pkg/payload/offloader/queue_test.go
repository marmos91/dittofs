package offloader

import (
	"context"
	"testing"
	"time"
)

func TestTransferQueue_Enqueue(t *testing.T) {
	cfg := DefaultTransferQueueConfig()
	cfg.QueueSize = 10
	q := NewTransferQueue(nil, cfg)

	// Enqueue requests
	for i := 0; i < 5; i++ {
		req := NewBlockUploadRequest("test-content", 0, uint32(i))
		if !q.Enqueue(req) {
			t.Errorf("Enqueue(%d) returned false", i)
		}
	}

	if q.Pending() != 5 {
		t.Errorf("Pending() = %d, want 5", q.Pending())
	}
}

func TestTransferQueue_QueueFull(t *testing.T) {
	cfg := TransferQueueConfig{
		QueueSize: 2,
		Workers:   1,
	}
	q := NewTransferQueue(nil, cfg)
	// Don't start workers - queue will fill up

	req1 := NewBlockUploadRequest("c1", 0, 0)
	req2 := NewBlockUploadRequest("c2", 0, 0)
	req3 := NewBlockUploadRequest("c3", 0, 0)

	if !q.Enqueue(req1) {
		t.Error("Enqueue(1) should succeed")
	}
	if !q.Enqueue(req2) {
		t.Error("Enqueue(2) should succeed")
	}
	if q.Enqueue(req3) {
		t.Error("Enqueue(3) should fail (queue full)")
	}

	if q.Pending() != 2 {
		t.Errorf("Pending() = %d, want 2", q.Pending())
	}
}

func TestTransferQueue_StopNotStarted(t *testing.T) {
	cfg := DefaultTransferQueueConfig()
	q := NewTransferQueue(nil, cfg)

	// Stop without starting - should not panic
	q.Stop(time.Second)
}

func TestTransferQueue_DoubleStart(t *testing.T) {
	cfg := DefaultTransferQueueConfig()
	q := NewTransferQueue(nil, cfg)

	ctx := context.Background()
	q.Start(ctx)
	q.Start(ctx) // Should be a no-op

	q.Stop(time.Second)
}

func TestTransferQueueConfig_Defaults(t *testing.T) {
	cfg := DefaultTransferQueueConfig()

	if cfg.QueueSize != 1000 {
		t.Errorf("default QueueSize = %d, want 1000", cfg.QueueSize)
	}
	if cfg.Workers != 4 {
		t.Errorf("default Workers = %d, want 4", cfg.Workers)
	}
}

func TestNewTransferQueue_InvalidConfig(t *testing.T) {
	// Test with invalid config values - should use defaults
	cfg := TransferQueueConfig{
		QueueSize: -1,
		Workers:   -1,
	}
	q := NewTransferQueue(nil, cfg)

	// Queue should have been created with defaults
	// Check upload channel capacity (all channels have same capacity)
	if cap(q.uploads) != 1000 {
		t.Errorf("uploads queue capacity = %d, want 1000", cap(q.uploads))
	}
	if cap(q.downloads) != 1000 {
		t.Errorf("downloads queue capacity = %d, want 1000", cap(q.downloads))
	}
	if cap(q.prefetch) != 1000 {
		t.Errorf("prefetch queue capacity = %d, want 1000", cap(q.prefetch))
	}
	if q.workers != 4 {
		t.Errorf("workers = %d, want 4", q.workers)
	}
}

func TestTransferQueue_Stats(t *testing.T) {
	cfg := DefaultTransferQueueConfig()
	q := NewTransferQueue(nil, cfg)

	pending, completed, failed := q.Stats()
	if pending != 0 || completed != 0 || failed != 0 {
		t.Errorf("Stats() = (%d, %d, %d), want (0, 0, 0)", pending, completed, failed)
	}

	// Enqueue some requests
	q.Enqueue(NewBlockUploadRequest("c1", 0, 0))
	q.Enqueue(NewBlockUploadRequest("c2", 0, 1))

	pending, _, _ = q.Stats()
	if pending != 2 {
		t.Errorf("Stats() pending = %d, want 2", pending)
	}
}

func TestTransferQueue_LastError(t *testing.T) {
	cfg := DefaultTransferQueueConfig()
	q := NewTransferQueue(nil, cfg)

	at, err := q.LastError()
	if err != nil {
		t.Errorf("LastError() error = %v, want nil", err)
	}
	if !at.IsZero() {
		t.Errorf("LastError() time should be zero initially")
	}
}

func TestTransferQueue_EnqueueByType(t *testing.T) {
	cfg := TransferQueueConfig{
		QueueSize: 10,
		Workers:   1,
	}
	q := NewTransferQueue(nil, cfg)

	// Test download enqueue
	if !q.EnqueueDownload(NewDownloadRequest("payload", 0, 0, nil)) {
		t.Error("EnqueueDownload should succeed")
	}

	// Test upload enqueue
	if !q.EnqueueUpload(NewBlockUploadRequest("payload", 0, 0)) {
		t.Error("EnqueueUpload should succeed")
	}

	// Test prefetch enqueue
	if !q.EnqueuePrefetch(NewPrefetchRequest("payload", 0, 1)) {
		t.Error("EnqueuePrefetch should succeed")
	}

	// Check pending counts by type
	download, upload, prefetch := q.PendingByType()
	if download != 1 {
		t.Errorf("download pending = %d, want 1", download)
	}
	if upload != 1 {
		t.Errorf("upload pending = %d, want 1", upload)
	}
	if prefetch != 1 {
		t.Errorf("prefetch pending = %d, want 1", prefetch)
	}

	// Total should be 3
	if q.Pending() != 3 {
		t.Errorf("total Pending() = %d, want 3", q.Pending())
	}
}

func TestTransferQueue_PrefetchDropWhenFull(t *testing.T) {
	cfg := TransferQueueConfig{
		QueueSize: 1,
		Workers:   1,
	}
	q := NewTransferQueue(nil, cfg)
	// Don't start workers - queue will fill up

	// First prefetch should succeed
	if !q.EnqueuePrefetch(NewPrefetchRequest("payload", 0, 0)) {
		t.Error("First prefetch should succeed")
	}

	// Second prefetch should be dropped silently (queue full)
	// This should NOT return false but simply drop - check pending count
	q.EnqueuePrefetch(NewPrefetchRequest("payload", 0, 1))

	// Only 1 should be pending (second was dropped)
	_, _, prefetch := q.PendingByType()
	if prefetch != 1 {
		t.Errorf("prefetch pending = %d, want 1 (second should be dropped)", prefetch)
	}
}
