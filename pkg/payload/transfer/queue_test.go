package transfer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// mockEntry is a test implementation of TransferQueueEntry.
type mockEntry struct {
	shareName  string
	fileHandle string
	payloadID  string
	priority   int
	executed   atomic.Bool
	executeErr error
}

func (e *mockEntry) ShareName() string     { return e.shareName }
func (e *mockEntry) FileHandle() string    { return e.fileHandle }
func (e *mockEntry) PayloadID() string     { return e.payloadID }
func (e *mockEntry) Priority() int         { return e.priority }
func (e *mockEntry) Execute(ctx context.Context, m *TransferManager) error {
	e.executed.Store(true)
	return e.executeErr
}

func TestTransferQueue_EnqueueAndProcess(t *testing.T) {
	// Create queue without manager (we'll use mock entries)
	cfg := DefaultTransferQueueConfig()
	cfg.QueueSize = 10
	cfg.Workers = 2
	q := NewTransferQueue(nil, cfg)

	// Start the queue
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	// Create and enqueue entries
	entries := make([]*mockEntry, 5)
	for i := 0; i < 5; i++ {
		entries[i] = &mockEntry{
			shareName:  "export",
			fileHandle: "handle",
			payloadID:  "test-content",
		}
		if !q.Enqueue(entries[i]) {
			t.Errorf("Enqueue(%d) returned false", i)
		}
	}

	// Wait for processing
	time.Sleep(100 * time.Millisecond)

	// Stop queue
	q.Stop(time.Second)

	// Verify all entries were executed
	for i, e := range entries {
		if !e.executed.Load() {
			t.Errorf("entry[%d] was not executed", i)
		}
	}

	// Check stats
	pending, completed, failed := q.Stats()
	if pending != 0 {
		t.Errorf("Stats() pending = %d, want 0", pending)
	}
	if completed != 5 {
		t.Errorf("Stats() completed = %d, want 5", completed)
	}
	if failed != 0 {
		t.Errorf("Stats() failed = %d, want 0", failed)
	}
}

func TestTransferQueue_QueueFull(t *testing.T) {
	cfg := TransferQueueConfig{
		QueueSize: 2,
		Workers:   0, // Don't start workers - queue will fill up
	}
	q := NewTransferQueue(nil, cfg)

	// Fill the queue (but don't process)
	entry1 := &mockEntry{payloadID: "1"}
	entry2 := &mockEntry{payloadID: "2"}
	entry3 := &mockEntry{payloadID: "3"}

	if !q.Enqueue(entry1) {
		t.Error("Enqueue(1) should succeed")
	}
	if !q.Enqueue(entry2) {
		t.Error("Enqueue(2) should succeed")
	}
	if q.Enqueue(entry3) {
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
