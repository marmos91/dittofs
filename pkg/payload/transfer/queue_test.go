package transfer

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
		req := NewTransferRequest("export", "handle", "test-content")
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

	req1 := NewTransferRequest("export", "h1", "c1")
	req2 := NewTransferRequest("export", "h2", "c2")
	req3 := NewTransferRequest("export", "h3", "c3")

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
	if cap(q.queue) != 1000 {
		t.Errorf("queue capacity = %d, want 1000", cap(q.queue))
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
	q.Enqueue(NewTransferRequest("export", "h1", "c1"))
	q.Enqueue(NewTransferRequest("export", "h2", "c2"))

	pending, _, _ = q.Stats()
	if pending != 2 {
		t.Errorf("Stats() pending = %d, want 2", pending)
	}
}

func TestTransferQueue_LastError(t *testing.T) {
	cfg := DefaultTransferQueueConfig()
	q := NewTransferQueue(nil, cfg)

	err, at := q.LastError()
	if err != nil {
		t.Errorf("LastError() error = %v, want nil", err)
	}
	if !at.IsZero() {
		t.Errorf("LastError() time should be zero initially")
	}
}
