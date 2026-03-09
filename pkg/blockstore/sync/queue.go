package sync

import (
	"context"
	gosync "sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// SyncQueue handles asynchronous transfers with dedicated worker pools.
//
// Download workers process only downloads and prefetch (never uploads).
// Upload workers process only uploads (never downloads).
// This isolation prevents upload bursts from starving latency-sensitive downloads.
type SyncQueue struct {
	manager *Syncer

	// Priority channels
	downloads chan TransferRequest // Processed by download workers
	uploads   chan TransferRequest // Processed by upload workers
	prefetch  chan TransferRequest // Processed by download workers when idle

	// Worker management
	uploadWorkers   int // Number of upload-only workers
	downloadWorkers int // Number of download+prefetch workers
	wg              gosync.WaitGroup
	stopCh          chan struct{}
	stoppedCh       chan struct{}
	started         bool // tracks whether Start() was called

	// Metrics
	mu              gosync.Mutex
	pendingDownload int
	pendingUpload   int
	pendingPrefetch int
	completed       int
	failed          int
	lastError       error
	lastErrorAt     time.Time
}

// NewSyncQueue creates a new transfer queue with dedicated worker pools.
func NewSyncQueue(m *Syncer, cfg SyncQueueConfig) *SyncQueue {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1000
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.DownloadWorkers <= 0 {
		cfg.DownloadWorkers = DefaultParallelDownloads
	}

	return &SyncQueue{
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
func (q *SyncQueue) Start(ctx context.Context) {
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
func (q *SyncQueue) Stop(timeout time.Duration) {
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

// EnqueueDownload adds a download request (highest priority).
// Returns false if the queue is full (non-blocking).
func (q *SyncQueue) EnqueueDownload(req TransferRequest) bool {
	req.Type = TransferDownload
	select {
	case q.downloads <- req:
		q.mu.Lock()
		q.pendingDownload++
		q.mu.Unlock()
		return true
	default:
		logger.Warn("Fetch queue full, dropping request",
			"payloadID", req.PayloadID)
		return false
	}
}

// EnqueueUpload adds an upload request (medium priority).
// Returns false if the queue is full (non-blocking).
func (q *SyncQueue) EnqueueUpload(req TransferRequest) bool {
	req.Type = TransferUpload
	select {
	case q.uploads <- req:
		q.mu.Lock()
		q.pendingUpload++
		q.mu.Unlock()
		return true
	default:
		logger.Warn("Sync queue full, dropping request",
			"payloadID", req.PayloadID)
		return false
	}
}

// EnqueuePrefetch adds a prefetch request (lowest priority).
// Returns false if the queue is full (non-blocking, best effort).
func (q *SyncQueue) EnqueuePrefetch(req TransferRequest) bool {
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
func (q *SyncQueue) Pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pendingDownload + q.pendingUpload + q.pendingPrefetch
}

// PendingByType returns pending counts by transfer type.
func (q *SyncQueue) PendingByType() (download, upload, prefetch int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pendingDownload, q.pendingUpload, q.pendingPrefetch
}

// Stats returns transfer statistics.
func (q *SyncQueue) Stats() (pending, completed, failed int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	pending = q.pendingDownload + q.pendingUpload + q.pendingPrefetch
	return pending, q.completed, q.failed
}

// LastError returns when the last error occurred and the error itself.
func (q *SyncQueue) LastError() (time.Time, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.lastErrorAt, q.lastError
}

// downloadWorker processes download and prefetch requests, exiting on stopCh close.
func (q *SyncQueue) downloadWorker(_ context.Context, id int) {
	defer q.wg.Done()

	logger.Debug("Fetch worker started", "workerID", id)

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
			logger.Debug("Fetch worker stopped", "workerID", id)
			return
		}
	}
}

// uploadWorker processes upload requests, exiting on stopCh close.
func (q *SyncQueue) uploadWorker(_ context.Context, id int) {
	defer q.wg.Done()

	logger.Debug("Sync worker started", "workerID", id)

	for {
		select {
		case req := <-q.uploads:
			q.processRequest(req)
		case <-q.stopCh:
			q.drainUploads()
			logger.Debug("Sync worker stopped", "workerID", id)
			return
		}
	}
}

// drainDownloads processes remaining downloads and prefetch during shutdown.
func (q *SyncQueue) drainDownloads() {
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
func (q *SyncQueue) drainUploads() {
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
func (q *SyncQueue) processRequest(req TransferRequest) {
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
func (q *SyncQueue) decrementPending(counter *int) {
	q.mu.Lock()
	(*counter)--
	q.mu.Unlock()
}

// recordResult updates metrics after a transfer completes.
func (q *SyncQueue) recordResult(req TransferRequest, err error) {
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
func (q *SyncQueue) signalDone(done chan error, err error) {
	if done != nil {
		done <- err
		close(done)
	}
}

// processDownload handles a download or prefetch request via the worker pool.
func (q *SyncQueue) processDownload(ctx context.Context, req TransferRequest) error {
	if q.manager == nil {
		return nil
	}

	_, err := q.manager.fetchBlock(ctx, req.PayloadID, req.BlockIdx)
	return err
}

// processUpload handles an upload request.
func (q *SyncQueue) processUpload(ctx context.Context, req TransferRequest) error {
	if q.manager == nil {
		return nil
	}
	return q.manager.uploadBlock(ctx, req.PayloadID, req.BlockIdx)
}
