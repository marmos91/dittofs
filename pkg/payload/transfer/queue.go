package transfer

import (
	"context"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// TransferQueue handles asynchronous transfers with priority scheduling.
//
// Priority order (highest to lowest):
//  1. Downloads - User is waiting for data
//  2. Uploads - Data durability
//  3. Prefetch - Speculative optimization
//
// Workers check channels in priority order, ensuring downloads are always
// processed first, even when upload/prefetch queues are full.
type TransferQueue struct {
	manager *TransferManager

	// Priority channels - workers check in priority order
	downloads chan TransferRequest // Highest priority
	uploads   chan TransferRequest // Medium priority
	prefetch  chan TransferRequest // Lowest priority

	// Worker management
	workers   int
	wg        sync.WaitGroup
	stopCh    chan struct{}
	stoppedCh chan struct{}
	started   bool // tracks whether Start() was called

	// Metrics
	mu              sync.Mutex
	pendingDownload int
	pendingUpload   int
	pendingPrefetch int
	completed       int
	failed          int
	lastError       error
	lastErrorAt     time.Time
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
		downloads: make(chan TransferRequest, cfg.QueueSize),
		uploads:   make(chan TransferRequest, cfg.QueueSize),
		prefetch:  make(chan TransferRequest, cfg.QueueSize),
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

// Enqueue adds an upload transfer request to the queue.
// Returns false if the queue is full (non-blocking).
// For backward compatibility - routes to upload channel.
func (q *TransferQueue) Enqueue(req TransferRequest) bool {
	return q.EnqueueUpload(req)
}

// EnqueueDownload adds a download request (highest priority).
// Returns false if the queue is full (non-blocking).
func (q *TransferQueue) EnqueueDownload(req TransferRequest) bool {
	req.Type = TransferDownload
	select {
	case q.downloads <- req:
		q.mu.Lock()
		q.pendingDownload++
		q.mu.Unlock()
		return true
	default:
		logger.Warn("Download queue full, dropping request",
			"payloadID", req.PayloadID)
		return false
	}
}

// EnqueueUpload adds an upload request (medium priority).
// Returns false if the queue is full (non-blocking).
func (q *TransferQueue) EnqueueUpload(req TransferRequest) bool {
	req.Type = TransferUpload
	select {
	case q.uploads <- req:
		q.mu.Lock()
		q.pendingUpload++
		q.mu.Unlock()
		return true
	default:
		logger.Warn("Upload queue full, dropping request",
			"payloadID", req.PayloadID)
		return false
	}
}

// EnqueuePrefetch adds a prefetch request (lowest priority).
// Returns false if the queue is full (non-blocking, best effort).
func (q *TransferQueue) EnqueuePrefetch(req TransferRequest) bool {
	req.Type = TransferPrefetch
	select {
	case q.prefetch <- req:
		q.mu.Lock()
		q.pendingPrefetch++
		q.mu.Unlock()
		return true
	default:
		// Prefetch is best-effort, silently drop if full
		return false
	}
}

// Pending returns the total number of pending transfer requests.
func (q *TransferQueue) Pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pendingDownload + q.pendingUpload + q.pendingPrefetch
}

// PendingByType returns pending counts by transfer type.
func (q *TransferQueue) PendingByType() (download, upload, prefetch int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pendingDownload, q.pendingUpload, q.pendingPrefetch
}

// Stats returns transfer statistics.
func (q *TransferQueue) Stats() (pending, completed, failed int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	pending = q.pendingDownload + q.pendingUpload + q.pendingPrefetch
	return pending, q.completed, q.failed
}

// LastError returns when the last error occurred and the error itself.
func (q *TransferQueue) LastError() (time.Time, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.lastErrorAt, q.lastError
}

// worker processes transfer requests from the queue with priority ordering.
// Priority: downloads > uploads > prefetch
//
// The worker uses a two-phase select to ensure downloads are processed first
// without busy-waiting (CPU spin) when queues are empty.
//
// IMPORTANT: Workers ignore the passed context for lifecycle management and only
// exit when stopCh is closed. This prevents workers from exiting early if the
// initialization context is short-lived or cancelled. Each request gets its own
// fresh context with timeout in processRequest().
func (q *TransferQueue) worker(_ context.Context, id int) {
	defer q.wg.Done()

	logger.Debug("Transfer queue worker started", "workerID", id)

	for {
		// Phase 1: Check for high-priority downloads (non-blocking)
		select {
		case req := <-q.downloads:
			q.processRequest(req)
			continue
		default:
		}

		// Phase 2: Wait for any work (blocking - no CPU spin)
		select {
		case req := <-q.downloads:
			q.processRequest(req)
		case req := <-q.uploads:
			q.processRequest(req)
		case req := <-q.prefetch:
			q.processRequest(req)
		case <-q.stopCh:
			q.drainQueue()
			logger.Debug("Transfer queue worker stopped", "workerID", id)
			return
		}
	}
}

// drainQueue processes remaining items in all queues during shutdown.
func (q *TransferQueue) drainQueue() {
	for {
		select {
		case req := <-q.downloads:
			q.processRequest(req)
		case req := <-q.uploads:
			q.processRequest(req)
		case req := <-q.prefetch:
			q.processRequest(req)
		default:
			return
		}
	}
}

// processRequest handles a single transfer request.
// Each request gets a fresh context with timeout - worker contexts are not used
// to ensure workers don't exit early if the initialization context is cancelled.
func (q *TransferQueue) processRequest(req TransferRequest) {
	// Use a fresh context with timeout for block store operations
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var err error

	switch req.Type {
	case TransferDownload:
		err = q.processDownload(ctx, req)
		q.decrementPending(&q.pendingDownload)

	case TransferUpload:
		err = q.processUpload(ctx, req)
		q.decrementPending(&q.pendingUpload)

	case TransferPrefetch:
		_ = q.processDownload(ctx, req) // Best effort - ignore errors
		q.decrementPending(&q.pendingPrefetch)
		q.signalDone(req.Done, nil) // Don't signal errors for prefetch
		return
	}

	q.recordResult(req, err)
	q.signalDone(req.Done, err)
}

// decrementPending decrements a pending counter under lock.
func (q *TransferQueue) decrementPending(counter *int) {
	q.mu.Lock()
	(*counter)--
	q.mu.Unlock()
}

// recordResult updates metrics after a transfer completes.
func (q *TransferQueue) recordResult(req TransferRequest, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if err != nil {
		q.failed++
		q.lastError = err
		q.lastErrorAt = time.Now()
		logger.Error("Transfer failed",
			"type", req.Type.String(),
			"payloadID", req.PayloadID,
			"error", err)
	} else {
		q.completed++
		logger.Debug("Transfer completed",
			"type", req.Type.String(),
			"payloadID", req.PayloadID)
	}
}

// signalDone sends result on Done channel if present.
func (q *TransferQueue) signalDone(done chan error, err error) {
	if done != nil {
		done <- err
		close(done)
	}
}

// processDownload handles a download request.
func (q *TransferQueue) processDownload(ctx context.Context, req TransferRequest) error {
	if q.manager == nil {
		return nil
	}

	// DEBUG: Log download start for large offsets
	if req.ChunkIdx >= 32 {
		logger.Debug("processDownload: starting",
			"payloadID", req.PayloadID,
			"chunkIdx", req.ChunkIdx,
			"blockIdx", req.BlockIdx)
	}

	// Download the block and cache it
	err := q.manager.downloadBlock(ctx, req.PayloadID, req.ChunkIdx, req.BlockIdx)

	// DEBUG: Log download result for large offsets
	if req.ChunkIdx >= 32 {
		if err != nil {
			logger.Debug("processDownload: failed",
				"payloadID", req.PayloadID,
				"chunkIdx", req.ChunkIdx,
				"blockIdx", req.BlockIdx,
				"error", err)
		} else {
			logger.Debug("processDownload: succeeded",
				"payloadID", req.PayloadID,
				"chunkIdx", req.ChunkIdx,
				"blockIdx", req.BlockIdx)
		}
	}

	return err
}

// processUpload handles an upload request.
func (q *TransferQueue) processUpload(ctx context.Context, req TransferRequest) error {
	if q.manager == nil {
		return nil
	}

	// All uploads are block-level (eager upload)
	return q.manager.uploadBlock(ctx, req.PayloadID, req.ChunkIdx, req.BlockIdx)
}
