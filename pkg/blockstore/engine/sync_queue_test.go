package engine

import (
	"context"
	"testing"
	"time"
)

func TestSyncQueue_EnqueueUpload(t *testing.T) {
	cfg := DefaultSyncQueueConfig()
	cfg.QueueSize = 10
	q := NewSyncQueue(nil, cfg)

	for i := 0; i < 5; i++ {
		req := NewBlockUploadRequest("test-content", uint64(i))
		if !q.EnqueueUpload(req) {
			t.Errorf("EnqueueUpload(%d) returned false", i)
		}
	}

	if q.Pending() != 5 {
		t.Errorf("Pending() = %d, want 5", q.Pending())
	}
}

func TestSyncQueue_QueueFull(t *testing.T) {
	cfg := SyncQueueConfig{
		QueueSize: 2,
		Workers:   1,
	}
	q := NewSyncQueue(nil, cfg)
	// Don't start workers - queue will fill up

	req1 := NewBlockUploadRequest("c1", 0)
	req2 := NewBlockUploadRequest("c2", 0)
	req3 := NewBlockUploadRequest("c3", 0)

	if !q.EnqueueUpload(req1) {
		t.Error("EnqueueUpload(1) should succeed")
	}
	if !q.EnqueueUpload(req2) {
		t.Error("EnqueueUpload(2) should succeed")
	}
	if q.EnqueueUpload(req3) {
		t.Error("EnqueueUpload(3) should fail (queue full)")
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

func TestSyncQueueConfig_Defaults(t *testing.T) {
	cfg := DefaultSyncQueueConfig()

	if cfg.QueueSize != 1000 {
		t.Errorf("default QueueSize = %d, want 1000", cfg.QueueSize)
	}
	if cfg.Workers != 4 {
		t.Errorf("default Workers = %d, want 4", cfg.Workers)
	}
	if cfg.DownloadWorkers != DefaultParallelDownloads {
		t.Errorf("default DownloadWorkers = %d, want %d", cfg.DownloadWorkers, DefaultParallelDownloads)
	}
}

func TestNewSyncQueue_InvalidConfig(t *testing.T) {
	// Test with invalid config values - should use defaults
	cfg := SyncQueueConfig{
		QueueSize: -1,
		Workers:   -1,
	}
	q := NewSyncQueue(nil, cfg)

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
	if q.uploadWorkers != 4 {
		t.Errorf("uploadWorkers = %d, want 4", q.uploadWorkers)
	}
	if q.downloadWorkers != DefaultParallelDownloads {
		t.Errorf("downloadWorkers = %d, want %d", q.downloadWorkers, DefaultParallelDownloads)
	}
}

func TestSyncQueue_Stats(t *testing.T) {
	cfg := DefaultSyncQueueConfig()
	q := NewSyncQueue(nil, cfg)

	pending, completed, failed := q.Stats()
	if pending != 0 || completed != 0 || failed != 0 {
		t.Errorf("Stats() = (%d, %d, %d), want (0, 0, 0)", pending, completed, failed)
	}

	q.EnqueueUpload(NewBlockUploadRequest("c1", 0))
	q.EnqueueUpload(NewBlockUploadRequest("c2", 1))

	pending, _, _ = q.Stats()
	if pending != 2 {
		t.Errorf("Stats() pending = %d, want 2", pending)
	}
}

func TestSyncQueue_LastError(t *testing.T) {
	cfg := DefaultSyncQueueConfig()
	q := NewSyncQueue(nil, cfg)

	at, err := q.LastError()
	if err != nil {
		t.Errorf("LastError() error = %v, want nil", err)
	}
	if !at.IsZero() {
		t.Errorf("LastError() time should be zero initially")
	}
}

func TestSyncQueue_EnqueueByType(t *testing.T) {
	cfg := SyncQueueConfig{
		QueueSize: 10,
		Workers:   1,
	}
	q := NewSyncQueue(nil, cfg)

	// Test download enqueue
	if !q.EnqueueDownload(NewDownloadRequest("payload", 0, nil)) {
		t.Error("EnqueueDownload should succeed")
	}

	// Test upload enqueue
	if !q.EnqueueUpload(NewBlockUploadRequest("payload", 0)) {
		t.Error("EnqueueUpload should succeed")
	}

	// Test prefetch enqueue
	if !q.EnqueuePrefetch(NewPrefetchRequest("payload", 1)) {
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

func TestSyncQueue_PrefetchDropWhenFull(t *testing.T) {
	cfg := SyncQueueConfig{
		QueueSize: 1,
		Workers:   1,
	}
	q := NewSyncQueue(nil, cfg)
	// Don't start workers - queue will fill up

	// First prefetch should succeed
	if !q.EnqueuePrefetch(NewPrefetchRequest("payload", 0)) {
		t.Error("First prefetch should succeed")
	}

	// Second prefetch should be dropped silently (queue full)
	// This should NOT return false but simply drop - check pending count
	q.EnqueuePrefetch(NewPrefetchRequest("payload", 1))

	// Only 1 should be pending (second was dropped)
	_, _, prefetch := q.PendingByType()
	if prefetch != 1 {
		t.Errorf("prefetch pending = %d, want 1 (second should be dropped)", prefetch)
	}
}
