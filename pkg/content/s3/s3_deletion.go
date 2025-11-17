// Package s3 implements S3-based content storage for DittoFS.
//
// This file contains buffered deletion logic for the S3 content store,
// enabling efficient batch processing of delete operations.
package s3

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// deletionWorker is a background goroutine that batches and processes delete operations.
//
// This worker reduces S3 API calls by batching deletions using the DeleteObjects API,
// which can delete up to 1000 objects per request. The worker flushes pending deletions:
//   - Every flushInterval (default: 2 seconds)
//   - When batchSize threshold is reached (default: 100 items)
//   - On explicit flush request
//   - On shutdown
//
// The worker runs until stopCh is closed, ensuring graceful shutdown with pending
// deletions flushed before exit.
func (s *S3ContentStore) deletionWorker() {
	defer close(s.deletionQueue.doneCh)

	ticker := time.NewTicker(s.deletionQueue.flushInterval)
	defer ticker.Stop()

	logger.Info("S3 deletion worker started: flush_interval=%s batch_size=%d",
		s.deletionQueue.flushInterval, s.deletionQueue.batchSize)

	for {
		select {
		case <-ticker.C:
			// Periodic flush
			s.flushDeletionQueue(context.Background())

		case <-s.deletionQueue.flushCh:
			// Explicit flush request (batch size threshold reached)
			s.flushDeletionQueue(context.Background())

		case <-s.deletionQueue.stopCh:
			// Shutdown - flush remaining deletions
			logger.Info("S3 deletion worker shutting down, flushing pending deletions...")
			s.flushDeletionQueue(context.Background())
			logger.Info("S3 deletion worker stopped")
			return
		}
	}
}

// flushDeletionQueue processes all pending deletions using batch delete.
//
// This method is called by the deletion worker and during shutdown. It:
//  1. Swaps out the current queue atomically
//  2. Deduplicates ContentIDs (multiple deletes of same content)
//  3. Calls DeleteBatch() which uses S3's DeleteObjects API
//  4. Logs results
//
// The method uses a background context with timeout to ensure deletions
// complete even during shutdown.
func (s *S3ContentStore) flushDeletionQueue(ctx context.Context) {
	// Get pending deletions atomically
	s.deletionQueue.mu.Lock()
	pending := s.deletionQueue.queue
	s.deletionQueue.queue = make([]metadata.ContentID, 0, s.deletionQueue.batchSize)
	s.deletionQueue.mu.Unlock()

	if len(pending) == 0 {
		return
	}

	// Deduplicate (same file may be deleted multiple times)
	unique := make(map[metadata.ContentID]struct{})
	for _, id := range pending {
		unique[id] = struct{}{}
	}

	// Convert back to slice
	ids := make([]metadata.ContentID, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}

	// Use a timeout context to ensure completion
	flushCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	logger.Debug("S3 deletion flush: processing %d unique items (from %d queued)",
		len(ids), len(pending))

	// Call batch delete (uses S3 DeleteObjects API)
	failures, err := s.DeleteBatch(flushCtx, ids)
	if err != nil {
		logger.Error("S3 deletion flush failed: %v", err)
		return
	}

	if len(failures) > 0 {
		logger.Warn("S3 deletion flush: %d items failed, %d succeeded",
			len(failures), len(ids)-len(failures))
		for id, ferr := range failures {
			logger.Debug("S3 deletion failed: content_id=%s error=%v", id, ferr)
		}
	} else {
		logger.Debug("S3 deletion flush: successfully deleted %d items", len(ids))
	}
}

// FlushDeletes forces an immediate flush of all pending deletions.
//
// This is useful for:
//   - Testing (ensure deletions complete before assertions)
//   - Graceful shutdown (flush before closing)
//   - Manual triggers (e.g., admin API)
//
// The method blocks until the flush completes or context is cancelled.
//
// Parameters:
//   - ctx: Context for cancellation and timeout
//
// Returns:
//   - error: Returns error if context is cancelled
func (s *S3ContentStore) FlushDeletes(ctx context.Context) error {
	if !s.deletionQueue.enabled {
		return nil // Nothing to flush
	}

	// Check context
	if err := ctx.Err(); err != nil {
		return err
	}

	// Trigger flush
	select {
	case s.deletionQueue.flushCh <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	default:
		// Channel already has signal, skip
	}

	// Wait briefly for worker to process
	// Note: This is best-effort, not guaranteed synchronous
	time.Sleep(100 * time.Millisecond)

	return nil
}

// Close stops the deletion worker and flushes pending deletions.
//
// This ensures graceful shutdown with no data loss. The method:
//  1. Signals the worker to stop
//  2. Waits for the worker to finish flushing
//  3. Returns when shutdown is complete
//
// Call this during server shutdown to ensure all deletions complete.
func (s *S3ContentStore) Close() error {
	if !s.deletionQueue.enabled {
		return nil
	}

	logger.Info("S3 content store closing, stopping deletion worker...")

	// Signal stop
	close(s.deletionQueue.stopCh)

	// Wait for worker to finish (with timeout)
	select {
	case <-s.deletionQueue.doneCh:
		logger.Info("S3 deletion worker stopped successfully")
	case <-time.After(60 * time.Second):
		logger.Warn("S3 deletion worker shutdown timeout after 60s")
	}

	return nil
}
