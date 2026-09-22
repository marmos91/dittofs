package engine

import (
	"context"
	gosync "sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// SyncQueue runs speculative readahead on a dedicated worker pool. It is the
// only asynchronous fetch path: uploads belong to the carve dispatcher, and a
// demand read fetches inline through fetchGroup/inlineFetchOrWait rather than
// queueing.
//
// decision: the queue carries prefetch only. A foreground arm would need a
// caller that wants blocks staged without waiting for them, and the one such
// caller on the horizon — bulk pre-warm of a subtree — does not exist yet.
// Re-adding a channel, an enqueue method and a worker case is a smaller cost
// than carrying arms no production path reaches; add one back when a pre-warm
// caller lands.
type SyncQueue struct {
	manager *RemoteSync

	prefetch chan TransferRequest // Processed by the prefetch workers

	// Worker management
	downloadWorkers int // Number of prefetch workers
	wg              gosync.WaitGroup
	stopCh          chan struct{}
	stoppedCh       chan struct{}
	stopOnce        gosync.Once // guards close(stopCh) against concurrent/repeated Stop()
	started         bool        // tracks whether Start() was called

	// workerCtx is the parent context for per-request contexts created in
	// processRequest. It is cancelled when stopCh is closed so that in-flight
	// prefetches abort promptly during Stop() instead of blocking on a
	// slow or hung remote (e.g. S3).
	workerCtx    context.Context
	workerCancel context.CancelFunc

	// Metrics
	mu              gosync.Mutex
	pendingPrefetch int
}

// NewSyncQueue creates a new transfer queue with a dedicated worker pool.
func NewSyncQueue(m *RemoteSync, cfg SyncQueueConfig) *SyncQueue {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1000
	}
	if cfg.DownloadWorkers <= 0 {
		cfg.DownloadWorkers = DefaultParallelDownloads
	}

	return &SyncQueue{
		manager:         m,
		prefetch:        make(chan TransferRequest, cfg.QueueSize),
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
	// Derive the worker context from the caller's ctx and arrange for it to
	// be cancelled when stopCh closes. processRequest uses this as parent so
	// in-flight transfers abort during Stop() instead of pinning goroutines
	// on a hung remote.
	workerCtx, workerCancel := context.WithCancel(ctx)
	q.workerCtx = workerCtx
	q.workerCancel = workerCancel
	q.mu.Unlock()

	go func() {
		<-q.stopCh
		workerCancel()
	}()

	logger.Info("Starting transfer queue", "download_workers", q.downloadWorkers)

	for i := 0; i < q.downloadWorkers; i++ {
		q.wg.Add(1)
		go q.downloadWorker(ctx, i)
	}

	go func() {
		q.wg.Wait()
		close(q.stoppedCh)
	}()
}

// Stop gracefully shuts down the transfer queue.
// It waits for pending uploads to complete (with timeout).
// Safe to call multiple times and from concurrent goroutines.
func (q *SyncQueue) Stop(timeout time.Duration) {
	q.mu.Lock()
	if !q.started {
		q.mu.Unlock()
		return
	}
	q.mu.Unlock()

	logger.Info("Stopping transfer queue", "pending", q.Pending())
	// sync.Once guards close(stopCh) so concurrent/repeated Stop() calls
	// cannot double-close the channel (panic). Mirrors HealthMonitor.Stop().
	q.stopOnce.Do(func() {
		close(q.stopCh)
	})

	select {
	case <-q.stoppedCh:
		logger.Info("Transfer queue stopped gracefully")
	case <-time.After(timeout):
		logger.Warn("Transfer queue stop timed out", "pending", q.Pending())
	}
}

// EnqueuePrefetch adds a prefetch request.
// Returns false if the queue is full (non-blocking, best effort).
func (q *SyncQueue) EnqueuePrefetch(req TransferRequest) bool {
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

// Pending returns the number of pending prefetch requests.
func (q *SyncQueue) Pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pendingPrefetch
}

// downloadWorker processes prefetch requests, exiting on stopCh close.
func (q *SyncQueue) downloadWorker(_ context.Context, id int) {
	defer q.wg.Done()

	logger.Debug("Fetch worker started", "workerID", id)

	for {
		select {
		case req := <-q.prefetch:
			q.processRequest(req)
		case <-q.stopCh:
			q.drainPrefetch()
			logger.Debug("Fetch worker stopped", "workerID", id)
			return
		}
	}
}

// drainPrefetch processes the remaining prefetch requests during shutdown.
func (q *SyncQueue) drainPrefetch() {
	for {
		select {
		case req := <-q.prefetch:
			q.processRequest(req)
		default:
			return
		}
	}
}

// processRequest handles a single transfer request with a fresh context.
// The per-request context is derived from workerCtx so that Stop() cancels
// in-flight transfers and they release worker goroutines promptly even when
// the remote (e.g. S3) is slow or hung.
func (q *SyncQueue) processRequest(req TransferRequest) {
	parent := q.workerCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()

	_ = q.processDownload(ctx, req) // Best effort - ignore errors
	q.decrementPending(&q.pendingPrefetch)
	q.signalDone(req.Done, nil) // Don't signal errors for prefetch
}

// decrementPending decrements a pending counter under lock.
func (q *SyncQueue) decrementPending(counter *int) {
	q.mu.Lock()
	(*counter)--
	q.mu.Unlock()
}

// signalDone sends result on Done channel if present.
func (q *SyncQueue) signalDone(done chan error, err error) {
	if done != nil {
		done <- err
		close(done)
	}
}

// processDownload stages one block for a prefetch request via the worker pool.
func (q *SyncQueue) processDownload(ctx context.Context, req TransferRequest) error {
	if q.manager == nil {
		return nil
	}

	return q.manager.fetchBlock(ctx, req.PayloadID, req.BlockIndex)
}
