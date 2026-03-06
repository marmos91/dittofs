package offloader

import (
	"context"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// TransferQueue handles asynchronous transfers with dedicated worker pools.
//
// Download workers process only downloads and prefetch (never uploads).
// Upload workers process only uploads (never downloads).
// This isolation prevents upload bursts from starving latency-sensitive downloads.
type TransferQueue struct {
	manager *Offloader

	// Priority channels
	downloads chan TransferRequest // Processed by download workers
	uploads   chan TransferRequest // Processed by upload workers
	prefetch  chan TransferRequest // Processed by download workers when idle

	// Worker management
	uploadWorkers   int // Number of upload-only workers
	downloadWorkers int // Number of download+prefetch workers
	wg              sync.WaitGroup
	stopCh          chan struct{}
	stoppedCh       chan struct{}
	started         bool // tracks whether Start() was called

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

// NewTransferQueue creates a new transfer queue with dedicated worker pools.
func NewTransferQueue(m *Offloader, cfg TransferQueueConfig) *TransferQueue {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1000
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.DownloadWorkers <= 0 {
		cfg.DownloadWorkers = DefaultParallelDownloads
	}

	return &TransferQueue{
		manager:         m,
		downloads:       make(chan TransferRequest, cfg.QueueSize),
		uploads:         make(chan TransferRequest, cfg.QueueSize),
		prefetch:        make(chan TransferRequest, cfg.QueueSize),
		uploadWorkers:   cfg.Workers,
		downloadWorkers: cfg.DownloadWorkers,
		stopCh:          make(chan struct{}),
		stoppedCh:       make(chan struct{}),
	}
}

// Start begins processing transfer requests with dedicated worker pools.
func (q *TransferQueue) Start(ctx context.Context) {
	q.mu.Lock()
	if q.started {
		q.mu.Unlock()
		return
	}
	q.started = true
	q.mu.Unlock()

	logger.Info("Starting transfer queue",
		"download_workers", q.downloadWorkers, "upload_workers", q.uploadWorkers)

	for i := 0; i < q.downloadWorkers; i++ {
		q.wg.Add(1)
		go q.downloadWorker(ctx, i)
	}
	for i := 0; i < q.uploadWorkers; i++ {
		q.wg.Add(1)
		go q.uploadWorker(ctx, i)
	}

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
		return
	}
	q.mu.Unlock()

	logger.Info("Stopping transfer queue", "pending", q.Pending())
	close(q.stopCh)

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

// downloadWorker processes download and prefetch requests, exiting on stopCh close.
func (q *TransferQueue) downloadWorker(_ context.Context, id int) {
	defer q.wg.Done()

	logger.Debug("Download worker started", "workerID", id)

	for {
		select {
		case req := <-q.downloads:
			q.processRequest(req)
			continue
		default:
		}

		select {
		case req := <-q.downloads:
			q.processRequest(req)
		case req := <-q.prefetch:
			q.processRequest(req)
		case <-q.stopCh:
			q.drainDownloads()
			logger.Debug("Download worker stopped", "workerID", id)
			return
		}
	}
}

// uploadWorker processes upload requests, exiting on stopCh close.
func (q *TransferQueue) uploadWorker(_ context.Context, id int) {
	defer q.wg.Done()

	logger.Debug("Upload worker started", "workerID", id)

	for {
		select {
		case req := <-q.uploads:
			q.processRequest(req)
		case <-q.stopCh:
			q.drainUploads()
			logger.Debug("Upload worker stopped", "workerID", id)
			return
		}
	}
}

// drainDownloads processes remaining downloads and prefetch during shutdown.
func (q *TransferQueue) drainDownloads() {
	for {
		select {
		case req := <-q.downloads:
			q.processRequest(req)
		case req := <-q.prefetch:
			q.processRequest(req)
		default:
			return
		}
	}
}

// drainUploads processes remaining uploads during shutdown.
func (q *TransferQueue) drainUploads() {
	for {
		select {
		case req := <-q.uploads:
			q.processRequest(req)
		default:
			return
		}
	}
}

// processRequest handles a single transfer request with a fresh context.
func (q *TransferQueue) processRequest(req TransferRequest) {
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

// processDownload handles a download or prefetch request via the worker pool.
func (q *TransferQueue) processDownload(ctx context.Context, req TransferRequest) error {
	if q.manager == nil {
		return nil
	}

	_, err := q.manager.downloadBlock(ctx, req.PayloadID, req.BlockIdx)
	return err
}

// processUpload handles an upload request.
func (q *TransferQueue) processUpload(ctx context.Context, req TransferRequest) error {
	if q.manager == nil {
		return nil
	}
	return q.manager.uploadBlock(ctx, req.PayloadID, req.BlockIdx)
}
