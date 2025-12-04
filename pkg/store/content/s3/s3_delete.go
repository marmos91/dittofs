// Package s3 implements S3-based content storage for DittoFS.
//
// This file contains delete operations for the S3 content store,
// including single deletes, batch deletes, and buffered deletion logic.
package s3

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// Delete removes content from S3.
//
// Buffered Deletion (Asynchronous Mode):
// When buffered deletion is enabled, this method returns nil immediately after
// queuing the deletion. The actual S3 deletion happens asynchronously via a
// background worker that batches deletions every 2 seconds or when 100+ items
// are queued (using S3's DeleteObjects API for efficiency).
//
// IMPORTANT: In buffered mode, returning nil does NOT guarantee the deletion
// has completed or succeeded. Callers must be aware:
//   - The content may still exist in S3 after this method returns
//   - Server crashes before flush will lose queued deletions
//   - Use Close() or TriggerFlush() before shutdown to ensure deletions complete
//
// When buffered deletion is disabled, deletions happen immediately (synchronous)
// with retry logic for transient errors.
//
// This operation is idempotent - deleting non-existent content returns nil.
//
// Retry Behavior:
// Synchronous deletions retry transient errors with exponential backoff.
//
// Context Cancellation:
// The S3 DeleteObject operation respects context cancellation.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier to delete
//
// Returns:
//   - error: Returns error for S3 failures or context cancellation (not for non-existent objects)
func (s *S3ContentStore) Delete(ctx context.Context, id metadata.ContentID) error {
	start := time.Now()
	var err error
	defer func() {
		if s.metrics != nil && !s.deletionQueue.enabled {
			// Only record metrics for synchronous deletes
			s.metrics.ObserveOperation("Delete", time.Since(start), err)
		}
	}()

	if err = ctx.Err(); err != nil {
		return err
	}

	// If buffered deletion is enabled, queue it
	if s.deletionQueue.enabled {
		s.deletionQueue.mu.Lock()
		s.deletionQueue.queue = append(s.deletionQueue.queue, id)
		queueLen := len(s.deletionQueue.queue)
		s.deletionQueue.mu.Unlock()

		// Trigger immediate flush if batch size threshold reached
		if uint(queueLen) >= s.deletionQueue.batchSize {
			select {
			case s.deletionQueue.flushCh <- struct{}{}:
			default:
				// Channel already has signal, skip
			}
		}

		return nil
	}

	// Buffering disabled - execute immediately with retry
	key := s.getObjectKey(id)
	var lastErr error

	for attempt := 0; attempt <= int(s.retry.maxRetries); attempt++ {
		if attempt > 0 {
			backoff := s.calculateBackoff(attempt - 1)
			logger.Debug("Delete: retrying after %v (attempt %d/%d): key=%s",
				backoff, attempt, s.retry.maxRetries, key)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		_, lastErr = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
		})

		if lastErr == nil {
			return nil
		}

		// Not found is not an error for delete (idempotent)
		if isNotFoundError(lastErr) {
			return nil
		}

		if !isRetryableError(lastErr) {
			break
		}

		logger.Debug("Delete: transient error (attempt %d/%d): key=%s error=%v",
			attempt+1, s.retry.maxRetries+1, key, lastErr)
	}

	err = fmt.Errorf("failed to delete object from S3 after %d attempts: %w", s.retry.maxRetries+1, lastErr)
	return err
}

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

// TriggerFlush signals the deletion worker to flush pending deletions.
//
// IMPORTANT: This is an asynchronous, non-blocking operation that only signals
// the worker thread. It does NOT wait for the flush to complete and does NOT
// guarantee the flush has occurred when it returns. The method name uses "Trigger"
// rather than "Flush" to emphasize the non-blocking behavior.
//
// This method is suitable for:
//   - Manual flush triggers (e.g., admin API endpoint)
//   - Opportunistic flushing when convenient
//
// This method is NOT suitable for:
//   - Testing (use Close() which provides synchronous guarantee)
//   - Shutdown sequences (use Close() which waits for completion)
//   - Any scenario requiring confirmation that deletions completed
//
// For guaranteed, synchronous flushing, use Close() instead, which waits for
// the worker to finish and ensures all queued deletions are processed.
//
// Parameters:
//   - ctx: Context for cancellation and timeout
//
// Returns:
//   - error: Returns error if context is cancelled
func (s *S3ContentStore) TriggerFlush(ctx context.Context) error {
	if !s.deletionQueue.enabled {
		return nil // Nothing to flush
	}

	// Check context
	if err := ctx.Err(); err != nil {
		return err
	}

	// Signal the worker to flush (non-blocking)
	select {
	case s.deletionQueue.flushCh <- struct{}{}:
		// Signal sent successfully
	case <-ctx.Done():
		return ctx.Err()
	default:
		// Channel already has signal, skip
	}

	return nil
}

// Close stops the deletion worker and flushes pending deletions.
//
// This ensures graceful shutdown with no data loss. The method:
//  1. Signals the worker to stop
//  2. Waits for the worker to finish flushing
//  3. Returns when shutdown is complete
//
// Safe to call multiple times (subsequent calls are no-ops).
// Call this during server shutdown to ensure all deletions complete.
func (s *S3ContentStore) Close() error {
	if !s.deletionQueue.enabled {
		return nil
	}

	s.deletionQueue.closeOnce.Do(func() {
		logger.Info("S3 content store closing, stopping deletion worker...")

		// Signal stop
		close(s.deletionQueue.stopCh)

		// Wait for worker to finish (with configurable timeout)
		timeout := s.deletionQueue.shutdownTimeout
		select {
		case <-s.deletionQueue.doneCh:
			logger.Info("S3 deletion worker stopped successfully")
		case <-time.After(timeout):
			logger.Warn("S3 deletion worker shutdown timeout after %s", timeout)
		}
	})

	return nil
}

// DeleteBatch removes multiple content items in one operation.
//
// S3 supports batch deletes of up to 1000 objects at a time. This implementation
// automatically chunks larger batches.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - ids: Content identifiers to delete
//
// Returns:
//   - map[metadata.ContentID]error: Map of failed deletions (empty = all succeeded)
//   - error: Returns error for catastrophic failures or context cancellation
func (s *S3ContentStore) DeleteBatch(ctx context.Context, ids []metadata.ContentID) (map[metadata.ContentID]error, error) {
	failures := make(map[metadata.ContentID]error)

	// S3 allows max 1000 objects per delete request
	const maxBatchSize = 1000

	for i := 0; i < len(ids); i += maxBatchSize {
		if err := ctx.Err(); err != nil {
			for j := i; j < len(ids); j++ {
				failures[ids[j]] = ctx.Err()
			}
			return failures, ctx.Err()
		}

		end := i + maxBatchSize
		if end > len(ids) {
			end = len(ids)
		}

		batch := ids[i:end]

		// Build delete objects input
		objects := make([]types.ObjectIdentifier, len(batch))
		for j, id := range batch {
			key := s.getObjectKey(id)
			objects[j] = types.ObjectIdentifier{
				Key: aws.String(key),
			}
		}

		// Execute batch delete
		start := time.Now()
		result, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &types.Delete{
				Objects: objects,
				Quiet:   aws.Bool(false),
			},
		})

		// Record metrics
		if s.metrics != nil {
			s.metrics.ObserveOperation("DeleteObjects", time.Since(start), err)
		}

		if err != nil {
			for _, id := range batch {
				failures[id] = err
			}
			continue
		}

		// Check for individual errors
		for _, deleteErr := range result.Errors {
			if deleteErr.Key == nil {
				continue
			}

			// Find the ContentID for this key (remove prefix to get path)
			key := *deleteErr.Key
			if s.keyPrefix != "" && len(key) > len(s.keyPrefix) {
				key = key[len(s.keyPrefix):]
			}

			id := metadata.ContentID(key)
			errMsg := "unknown error"
			if deleteErr.Code != nil && deleteErr.Message != nil {
				errMsg = fmt.Sprintf("%s: %s", *deleteErr.Code, *deleteErr.Message)
			}
			failures[id] = errors.New(errMsg)
		}
	}

	return failures, nil
}
