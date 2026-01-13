// Package flusher implements background upload for cache-to-block-store persistence.
package flusher

import (
	"context"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// uploadRequest represents a pending upload request.
type uploadRequest struct {
	shareName  string
	fileHandle []byte
	contentID  string
}

// BackgroundUploader handles asynchronous block store uploads.
// It processes upload requests in the background, decoupling block store latency
// from NFS COMMIT operations.
type BackgroundUploader struct {
	flusher *Flusher

	// Upload queue with bounded capacity
	queue chan uploadRequest

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

// BackgroundUploaderConfig holds configuration for the background uploader.
type BackgroundUploaderConfig struct {
	// QueueSize is the maximum number of pending upload requests.
	// Default: 1000
	QueueSize int

	// Workers is the number of concurrent upload workers.
	// Default: 4
	Workers int
}

// DefaultBackgroundUploaderConfig returns sensible defaults.
func DefaultBackgroundUploaderConfig() BackgroundUploaderConfig {
	return BackgroundUploaderConfig{
		QueueSize: 1000,
		Workers:   4,
	}
}

// NewBackgroundUploader creates a new background uploader.
func NewBackgroundUploader(f *Flusher, cfg BackgroundUploaderConfig) *BackgroundUploader {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1000
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}

	return &BackgroundUploader{
		flusher:   f,
		queue:     make(chan uploadRequest, cfg.QueueSize),
		workers:   cfg.Workers,
		stopCh:    make(chan struct{}),
		stoppedCh: make(chan struct{}),
	}
}

// Start begins processing upload requests.
func (b *BackgroundUploader) Start(ctx context.Context) {
	b.mu.Lock()
	if b.started {
		b.mu.Unlock()
		return
	}
	b.started = true
	b.mu.Unlock()

	logger.Info("Starting background uploader", "workers", b.workers)

	for i := 0; i < b.workers; i++ {
		b.wg.Add(1)
		go b.worker(ctx, i)
	}

	// Monitor goroutine to close stoppedCh when all workers exit
	go func() {
		b.wg.Wait()
		close(b.stoppedCh)
	}()
}

// Stop gracefully shuts down the background uploader.
// It waits for pending uploads to complete (with timeout).
func (b *BackgroundUploader) Stop(timeout time.Duration) {
	b.mu.Lock()
	if !b.started {
		b.mu.Unlock()
		// Never started - nothing to stop
		return
	}
	b.mu.Unlock()

	logger.Info("Stopping background uploader", "pending", b.Pending())

	// Signal workers to stop
	close(b.stopCh)

	// Wait with timeout
	select {
	case <-b.stoppedCh:
		logger.Info("Background uploader stopped gracefully")
	case <-time.After(timeout):
		logger.Warn("Background uploader stop timed out", "pending", b.Pending())
	}
}

// Enqueue adds an upload request to the queue.
// Returns false if the queue is full (non-blocking).
func (b *BackgroundUploader) Enqueue(shareName string, fileHandle []byte, contentID string) bool {
	req := uploadRequest{
		shareName:  shareName,
		fileHandle: fileHandle,
		contentID:  contentID,
	}

	select {
	case b.queue <- req:
		b.mu.Lock()
		b.pending++
		b.mu.Unlock()
		return true
	default:
		// Queue full
		logger.Warn("Background upload queue full, dropping request",
			"contentID", contentID)
		return false
	}
}

// Pending returns the number of pending upload requests.
func (b *BackgroundUploader) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pending
}

// Stats returns upload statistics.
func (b *BackgroundUploader) Stats() (pending, completed, failed int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pending, b.completed, b.failed
}

// worker processes upload requests from the queue.
func (b *BackgroundUploader) worker(ctx context.Context, id int) {
	defer b.wg.Done()

	for {
		select {
		case <-b.stopCh:
			// Drain remaining items before exiting
			b.drainQueue(ctx)
			return

		case <-ctx.Done():
			return

		case req, ok := <-b.queue:
			if !ok {
				return
			}
			b.processRequest(ctx, req)
		}
	}
}

// drainQueue processes remaining items in the queue during shutdown.
func (b *BackgroundUploader) drainQueue(ctx context.Context) {
	for {
		select {
		case req, ok := <-b.queue:
			if !ok {
				return
			}
			b.processRequest(ctx, req)
		default:
			return
		}
	}
}

// processRequest handles a single upload request.
func (b *BackgroundUploader) processRequest(ctx context.Context, req uploadRequest) {
	// Use a fresh context with timeout for block store operations
	uploadCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	err := b.flusher.flushRemainingSync(uploadCtx, req.shareName, req.fileHandle, req.contentID)

	b.mu.Lock()
	b.pending--
	if err != nil {
		b.failed++
		b.lastError = err
		b.lastErrorAt = time.Now()
		logger.Error("Background upload failed",
			"contentID", req.contentID,
			"error", err)
	} else {
		b.completed++
		logger.Debug("Background upload completed", "contentID", req.contentID)
	}
	b.mu.Unlock()
}
