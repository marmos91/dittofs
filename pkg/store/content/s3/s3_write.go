// Package s3 implements S3-based content storage for DittoFS.
//
// This file contains write operations for the S3 content store, including
// full content writes and truncation.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/telemetry"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/store/content"
)

// WriteContent writes the entire content in one operation.
//
// This uses S3 PutObject for uploading the complete content.
//
// Retry Behavior:
// Transient errors (network issues, throttling, 5xx errors) are retried
// with exponential backoff.
//
// Context Cancellation:
// The S3 PutObject operation respects context cancellation.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//   - data: Complete content data
//
// Returns:
//   - error: Returns error if write fails or context is cancelled
func (s *S3ContentStore) WriteContent(
	ctx context.Context,
	id metadata.ContentID,
	data []byte,
) error {
	ctx, span := telemetry.StartContentSpan(ctx, "write_content", string(id),
		telemetry.FSCount(uint32(len(data))),
		telemetry.StoreName("s3"),
		telemetry.StoreType("content"))
	defer span.End()

	start := time.Now()
	var err error
	defer func() {
		if s.metrics != nil {
			s.metrics.ObserveOperation("WriteContent", time.Since(start), err)
			if err == nil {
				s.metrics.RecordBytes("write", int64(len(data)))
			}
		}
		if err != nil {
			telemetry.RecordError(ctx, err)
		}
	}()

	if err = ctx.Err(); err != nil {
		return err
	}

	key := s.getObjectKey(id)
	err = s.writeContentWithRetry(ctx, key, data)
	return err
}

// WriteAt writes data at a specific offset within the content.
//
// WARNING: This operation is INEFFICIENT for S3. It requires downloading
// the entire object, modifying it in memory, and re-uploading. For NFS
// write operations, prefer using cache + IncrementalWriteStore instead.
//
// This fallback implementation exists for compatibility but should be
// avoided in performance-critical paths. Consider using:
//   - Cache buffering for NFS writes (recommended)
//   - WriteContent for full file replacement
//   - IncrementalWriteStore for streaming uploads
//
// Thread Safety:
// Concurrent WriteAt calls on the same object are serialized using per-object
// locks to prevent race conditions. Without this, concurrent writes would race:
// each goroutine reads the current state, modifies it, and uploads - the last
// upload wins, losing changes from other goroutines.
//
// Retry Behavior:
// Transient errors are retried with exponential backoff.
//
// Context Cancellation:
// S3 operations respect context cancellation.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//   - data: Data to write
//   - offset: Byte offset where writing begins
//
// Returns:
//   - error: Returns error if operation fails or context is cancelled
func (s *S3ContentStore) WriteAt(
	ctx context.Context,
	id metadata.ContentID,
	data []byte,
	offset uint64,
) error {
	ctx, span := telemetry.StartContentSpan(ctx, "write", string(id),
		telemetry.FSOffset(offset),
		telemetry.FSCount(uint32(len(data))),
		telemetry.StoreName("s3"),
		telemetry.StoreType("content"))
	defer span.End()

	start := time.Now()
	var err error
	defer func() {
		if s.metrics != nil {
			s.metrics.ObserveOperation("WriteAt", time.Since(start), err)
		}
		if err != nil {
			telemetry.RecordError(ctx, err)
		}
	}()

	if err = ctx.Err(); err != nil {
		return err
	}

	// Empty write is a no-op
	if len(data) == 0 {
		return nil
	}

	// Acquire per-object lock to serialize concurrent writes to the same object.
	// This prevents the read-modify-write race condition where concurrent writers
	// would each read the same initial state, modify it independently, and then
	// upload - with the last upload overwriting all previous changes.
	lock := s.getObjectLock(id)
	lock.Lock()
	defer lock.Unlock()

	key := s.getObjectKey(id)

	// Check if object exists and get current size
	currentSize, err := s.GetContentSize(ctx, id)
	if err != nil {
		// Check for both S3-specific not found errors and the wrapped content.ErrContentNotFound
		if isNotFoundError(err) || errors.Is(err, content.ErrContentNotFound) {
			// Object doesn't exist - create new content with data at offset
			// Fill with zeros up to offset, then append data
			newSize := offset + uint64(len(data))
			newData := make([]byte, newSize)
			copy(newData[offset:], data)

			return s.writeContentWithRetry(ctx, key, newData)
		}
		return fmt.Errorf("failed to get current size for WriteAt: %w", err)
	}

	// Calculate new size (may extend beyond current size)
	newSize := currentSize
	endOffset := offset + uint64(len(data))
	if endOffset > newSize {
		newSize = endOffset
	}

	// Download existing content with retry
	var existingData []byte
	var lastErr error

	for attempt := 0; attempt <= int(s.retry.maxRetries); attempt++ {
		if attempt > 0 {
			backoff := s.calculateBackoff(attempt - 1)
			logger.Debug("WriteAt: retrying download", "backoff", backoff, "attempt", attempt, "max_retries", s.retry.maxRetries, "key", key)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		reader, lastErr := s.ReadContent(ctx, id)
		if lastErr != nil {
			if !isRetryableError(lastErr) {
				return fmt.Errorf("failed to read existing content for WriteAt: %w", lastErr)
			}
			continue
		}

		existingData, lastErr = io.ReadAll(reader)
		_ = reader.Close()

		if lastErr == nil {
			break
		}

		if !isRetryableError(lastErr) {
			return fmt.Errorf("failed to read existing content for WriteAt: %w", lastErr)
		}
	}

	if lastErr != nil {
		return fmt.Errorf("failed to read existing content after %d attempts: %w", s.retry.maxRetries+1, lastErr)
	}

	// Create new buffer with modified content
	newData := make([]byte, newSize)
	copy(newData, existingData)
	copy(newData[offset:], data)

	// Upload modified content with retry
	return s.writeContentWithRetry(ctx, key, newData)
}

// writeContentWithRetry uploads content to S3 with retry logic.
func (s *S3ContentStore) writeContentWithRetry(ctx context.Context, key string, data []byte) error {
	var lastErr error

	for attempt := 0; attempt <= int(s.retry.maxRetries); attempt++ {
		if attempt > 0 {
			backoff := s.calculateBackoff(attempt - 1)
			logger.Debug("writeContentWithRetry: retrying", "backoff", backoff, "attempt", attempt, "max_retries", s.retry.maxRetries, "key", key)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		_, lastErr = s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader(data),
		})

		if lastErr == nil {
			if s.metrics != nil {
				s.metrics.RecordBytes("write", int64(len(data)))
			}
			return nil
		}

		if !isRetryableError(lastErr) {
			break
		}

		logger.Debug("writeContentWithRetry: transient error", "attempt", attempt+1, "max_retries", s.retry.maxRetries+1, "key", key, "error", lastErr)
	}

	return fmt.Errorf("failed to write content to S3 after %d attempts: %w", s.retry.maxRetries+1, lastErr)
}

// Truncate changes the size of the content.
//
// S3 Implementation Notes:
//   - Shrinking: Uses range GET to download only needed bytes, then PUT
//   - Extending: Downloads full object, extends with zeros, then PUT
//   - Both operations require re-uploading the object (S3 limitation)
//
// WARNING: Extending large files is memory-intensive as the entire new
// content must be held in memory. Consider using sparse file semantics
// in metadata for very large extensions.
//
// Thread Safety:
// Concurrent Truncate calls on the same object are serialized using per-object
// locks to prevent race conditions during read-modify-write operations.
//
// Retry Behavior:
// Transient errors are retried with exponential backoff.
//
// Context Cancellation:
// S3 operations respect context cancellation.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//   - newSize: New size in bytes
//
// Returns:
//   - error: Returns error if truncate fails or context is cancelled
func (s *S3ContentStore) Truncate(
	ctx context.Context,
	id metadata.ContentID, newSize uint64,
) error {
	start := time.Now()
	var err error
	defer func() {
		if s.metrics != nil {
			s.metrics.ObserveOperation("Truncate", time.Since(start), err)
		}
	}()

	if err = ctx.Err(); err != nil {
		return err
	}

	// Acquire per-object lock to serialize concurrent truncate operations.
	// This prevents race conditions during read-modify-write.
	lock := s.getObjectLock(id)
	lock.Lock()
	defer lock.Unlock()

	key := s.getObjectKey(id)

	// Get current size (also verifies object exists)
	currentSize, err := s.GetContentSize(ctx, id)
	if err != nil {
		if isNotFoundError(err) {
			return fmt.Errorf("truncate failed for %s: %w", id, content.ErrContentNotFound)
		}
		return err
	}

	// No-op if size is already correct
	if currentSize == newSize {
		return nil
	}

	if newSize < currentSize {
		// Shrinking
		if newSize == 0 {
			// Truncate to empty - just PUT an empty object
			return s.writeContentWithRetry(ctx, key, []byte{})
		}

		// Shrinking to non-zero size - download range and re-upload
		rangeStr := fmt.Sprintf("bytes=0-%d", newSize-1)
		var data []byte
		var lastErr error

		for attempt := 0; attempt <= int(s.retry.maxRetries); attempt++ {
			if attempt > 0 {
				backoff := s.calculateBackoff(attempt - 1)
				logger.Debug("Truncate: retrying range GET", "backoff", backoff, "attempt", attempt, "max_retries", s.retry.maxRetries, "key", key)

				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(backoff):
				}
			}

			result, lastErr := s.client.GetObject(ctx, &s3.GetObjectInput{
				Bucket: aws.String(s.bucket),
				Key:    aws.String(key),
				Range:  aws.String(rangeStr),
			})
			if lastErr != nil {
				if !isRetryableError(lastErr) {
					return fmt.Errorf("failed to get object for truncate: %w", lastErr)
				}
				continue
			}

			data, lastErr = io.ReadAll(result.Body)
			_ = result.Body.Close()

			if lastErr == nil {
				break
			}

			if !isRetryableError(lastErr) {
				return fmt.Errorf("failed to read object for truncate: %w", lastErr)
			}
		}

		if lastErr != nil {
			return fmt.Errorf("failed to get object for truncate after %d attempts: %w",
				s.retry.maxRetries+1, lastErr)
		}

		return s.writeContentWithRetry(ctx, key, data)
	}

	// Extending - download existing and append zeros
	var existingData []byte
	var lastErr error

	for attempt := 0; attempt <= int(s.retry.maxRetries); attempt++ {
		if attempt > 0 {
			backoff := s.calculateBackoff(attempt - 1)
			logger.Debug("Truncate: retrying GET", "backoff", backoff, "attempt", attempt, "max_retries", s.retry.maxRetries, "key", key)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		reader, lastErr := s.ReadContent(ctx, id)
		if lastErr != nil {
			if !isRetryableError(lastErr) {
				return fmt.Errorf("failed to read existing content for extend: %w", lastErr)
			}
			continue
		}

		existingData, lastErr = io.ReadAll(reader)
		_ = reader.Close()

		if lastErr == nil {
			break
		}

		if !isRetryableError(lastErr) {
			return fmt.Errorf("failed to read existing content for extend: %w", lastErr)
		}
	}

	if lastErr != nil {
		return fmt.Errorf("failed to read existing content after %d attempts: %w",
			s.retry.maxRetries+1, lastErr)
	}

	// Create extended buffer (zeros are default in Go)
	newData := make([]byte, newSize)
	copy(newData, existingData)

	return s.writeContentWithRetry(ctx, key, newData)
}
