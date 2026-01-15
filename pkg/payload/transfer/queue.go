package transfer

import (
	"context"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// TransferQueue handles asynchronous block store uploads.
// It processes transfer requests in the background, decoupling block store latency
// from NFS COMMIT operations.
type TransferQueue struct {
	manager *TransferManager

	// Upload queue with bounded capacity
	queue chan TransferRequest

	// Worker management
	workers   int
	wg        sync.WaitGroup
	stopCh    chan struct{}
	stoppedCh chan struct{}
	started   bool // tracks whether Start() was called

	// Metrics
	mu          sync.Mutex
	pending     int
	completed   int
	failed      int
	lastError   error
	lastErrorAt time.Time
}

// TransferQueueConfig holds configuration for the transfer queue.
type TransferQueueConfig struct {
	// QueueSize is the maximum number of pending transfer requests.
	// Default: 1000
	QueueSize int

	// Workers is the number of concurrent upload workers.
	// Default: 4
	Workers int
}

// DefaultTransferQueueConfig returns sensible defaults.
func DefaultTransferQueueConfig() TransferQueueConfig {
	return TransferQueueConfig{
		QueueSize: 1000,
		Workers:   4,
	}
}

// NewTransferQueue creates a new transfer queue.
func NewTransferQueue(m *TransferManager, cfg TransferQueueConfig) *TransferQueue {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1000
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}

	return &TransferQueue{
		manager:   m,
		queue:     make(chan TransferRequest, cfg.QueueSize),
		workers:   cfg.Workers,
		stopCh:    make(chan struct{}),
		stoppedCh: make(chan struct{}),
	}
}

// Start begins processing transfer requests.
func (q *TransferQueue) Start(ctx context.Context) {
	q.mu.Lock()
	if q.started {
		q.mu.Unlock()
		return
	}
	q.started = true
	q.mu.Unlock()

	logger.Info("Starting transfer queue", "workers", q.workers)

	for i := 0; i < q.workers; i++ {
		q.wg.Add(1)
		go q.worker(ctx, i)
	}

	// Monitor goroutine to close stoppedCh when all workers exit
	go func() {
		q.wg.Wait()
		close(q.stoppedCh)
	}()
}

// Stop gracefully shuts down the transfer queue.
// It waits for pending uploads to complete (with timeout).
func (q *TransferQueue) Stop(timeout time.Duration) {
	q.mu.Lock()
	if !q.started {
		q.mu.Unlock()
		// Never started - nothing to stop
		return
	}
	q.mu.Unlock()

	logger.Info("Stopping transfer queue", "pending", q.Pending())

	// Signal workers to stop
	close(q.stopCh)

	// Wait with timeout
	select {
	case <-q.stoppedCh:
		logger.Info("Transfer queue stopped gracefully")
	case <-time.After(timeout):
		logger.Warn("Transfer queue stop timed out", "pending", q.Pending())
	}
}

// Enqueue adds a transfer request to the queue.
// Returns false if the queue is full (non-blocking).
func (q *TransferQueue) Enqueue(req TransferRequest) bool {
	select {
	case q.queue <- req:
		q.mu.Lock()
		q.pending++
		q.mu.Unlock()
		return true
	default:
		// Queue full
		logger.Warn("Transfer queue full, dropping request",
			"payloadID", req.PayloadID)
		return false
	}
}

// Pending returns the number of pending transfer requests.
func (q *TransferQueue) Pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pending
}

// Stats returns transfer statistics.
func (q *TransferQueue) Stats() (pending, completed, failed int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pending, q.completed, q.failed
}

// LastError returns the last error and when it occurred.
func (q *TransferQueue) LastError() (error, time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.lastError, q.lastErrorAt
}

// worker processes transfer requests from the queue.
func (q *TransferQueue) worker(ctx context.Context, _ int) {
	defer q.wg.Done()

	for {
		select {
		case <-q.stopCh:
			// Drain remaining items before exiting
			q.drainQueue(ctx)
			return

		case <-ctx.Done():
			return

		case req, ok := <-q.queue:
			if !ok {
				return
			}
			q.processRequest(ctx, req)
		}
	}
}

// drainQueue processes remaining items in the queue during shutdown.
func (q *TransferQueue) drainQueue(ctx context.Context) {
	for {
		select {
		case req, ok := <-q.queue:
			if !ok {
				return
			}
			q.processRequest(ctx, req)
		default:
			return
		}
	}
}

// processRequest handles a single transfer request.
func (q *TransferQueue) processRequest(_ context.Context, req TransferRequest) {
	// Use a fresh context with timeout for block store operations
	uploadCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Execute the transfer via the manager
	err := q.manager.flushRemainingSyncInternal(uploadCtx, req.ShareName, req.FileHandle, req.PayloadID, true)

	q.mu.Lock()
	q.pending--
	if err != nil {
		q.failed++
		q.lastError = err
		q.lastErrorAt = time.Now()
		logger.Error("Transfer failed",
			"payloadID", req.PayloadID,
			"error", err)
	} else {
		q.completed++
		logger.Debug("Transfer completed", "payloadID", req.PayloadID)
	}
	q.mu.Unlock()
}
