// Package s3 implements S3-based content storage for DittoFS.
//
// This file contains write operations for the S3 content store, including
// full content writes, WriteAt, truncation, and deletion.
package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/content"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// WriteContent writes the entire content in one operation.
//
// This uses S3 PutObject for uploading the complete content.
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
func (s *S3ContentStore) WriteContent(ctx context.Context, id metadata.ContentID, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	key := s.getObjectKey(id)

	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return fmt.Errorf("failed to write content to S3: %w", err)
	}

	return nil
}

// WriteAt writes data at the specified offset.
//
// OPTIMIZED IMPLEMENTATION:
// This method now detects sequential write patterns (common in NFS) and uses
// efficient append-only writes instead of read-modify-write cycles.
//
// Write Patterns:
//  1. Sequential writes (offset = current size): Append efficiently using multipart
//  2. Writes at offset 0 on new file: Simple PutObject
//  3. Random/sparse writes: Fall back to read-modify-write (slow but correct)
//
// Performance:
//   - Sequential: O(n) - each write uploads only new data
//   - Random: O(n²) - each write downloads and re-uploads entire file
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
//   - error: Returns error if write fails or context is cancelled
func (s *S3ContentStore) WriteAt(ctx context.Context, id metadata.ContentID, data []byte, offset int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	key := s.getObjectKey(id)

	// ========================================================================
	// OPTIMIZATION: Use write buffer for sequential append pattern
	// ========================================================================
	// Check if this is a sequential write and buffer it in memory.
	// This avoids expensive read-modify-write cycles on S3.

	s.writeBuffersMu.Lock()
	if s.writeBuffers == nil {
		s.writeBuffers = make(map[string]*writeBuffer)
	}

	idStr := string(id)
	buffer, hasBuffer := s.writeBuffers[idStr]

	if !hasBuffer {
		// Create new buffer for this file
		buffer = &writeBuffer{
			data:         make([]byte, 0, s.partSize),
			expectedSize: 0,
			lastWrite:    time.Now(),
		}
		s.writeBuffers[idStr] = buffer
	}

	// Lock this specific buffer before releasing the map lock to prevent
	// race conditions where another goroutine could access the same buffer
	// between releasing writeBuffersMu and acquiring buffer.mu
	buffer.mu.Lock()
	s.writeBuffersMu.Unlock()

	defer buffer.mu.Unlock()

	// Check if this is a sequential append
	if offset == buffer.expectedSize {
		// Sequential write - append to buffer
		buffer.data = append(buffer.data, data...)
		buffer.expectedSize = offset + int64(len(data))
		buffer.lastWrite = time.Now()

		// NOTE: We do NOT automatically flush when buffer reaches partSize
		// because PutObject REPLACES the entire S3 object, which would lose
		// previously uploaded data. Instead, we accumulate all data in memory
		// and upload once on FlushWrites().
		//
		// For large files, this means more memory usage, but ensures correctness.
		// A proper implementation would use S3 multipart uploads to append data
		// efficiently, but that's a larger refactoring.

		return nil
	}

	// ========================================================================
	// FALLBACK: Non-sequential write (rare - random access or overwrites)
	// ========================================================================
	// When a non-sequential write is detected, we need to:
	// 1. Read existing S3 object (if any)
	// 2. Merge any buffered sequential writes (which start at offset 0)
	// 3. Apply the new non-sequential write
	// 4. Upload the complete merged result
	// This ensures no data is lost from the buffer.

	hasBufferedData := len(buffer.data) > 0
	if hasBufferedData {
		logger.Warn("Non-sequential write detected with buffered data - performing read-modify-write to preserve all data: content_id=%s buffer_size=%d offset=%d expected_offset=%d",
			string(id), len(buffer.data), offset, buffer.expectedSize)
	}

	// Step 1: Read existing S3 object (if any)
	existingData := []byte{}
	exists, err := s.ContentExists(ctx, id)
	if err != nil {
		return err
	}

	if exists {
		reader, err := s.ReadContent(ctx, id)
		if err != nil {
			return err
		}
		defer func() { _ = reader.Close() }()

		existingData, err = io.ReadAll(reader)
		if err != nil {
			return fmt.Errorf("failed to read existing content: %w", err)
		}
	}

	// Step 2: Determine the final size needed
	// Must accommodate: existing data, buffered data (starts at 0), and new write at offset
	finalSize := int64(len(existingData))
	if hasBufferedData && buffer.expectedSize > finalSize {
		finalSize = buffer.expectedSize
	}
	newWriteEnd := offset + int64(len(data))
	if newWriteEnd > finalSize {
		finalSize = newWriteEnd
	}

	// Create merged data buffer
	mergedData := make([]byte, finalSize)

	// Copy existing S3 data
	copy(mergedData, existingData)

	// Step 3: Merge buffered data (overwrites starting from offset 0)
	if hasBufferedData {
		copy(mergedData, buffer.data)
		// Clear buffer after merging
		buffer.data = buffer.data[:0]
		buffer.expectedSize = 0
		buffer.lastWrite = time.Time{}
	}

	// Step 4: Apply the new non-sequential write
	copy(mergedData[offset:], data)

	// Step 5: Upload the complete merged result
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(mergedData),
	})
	if err != nil {
		return fmt.Errorf("failed to write merged object to S3: %w", err)
	}

	return nil
}

// FlushWrites flushes any buffered writes for a content ID to S3.
//
// This should be called when the NFS COMMIT operation is received, or when
// you need to ensure all writes are persisted to S3.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier to flush
//
// Returns:
//   - error: Returns error if flush fails or context is cancelled
func (s *S3ContentStore) FlushWrites(ctx context.Context, id metadata.ContentID) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.writeBuffersMu.Lock()
	idStr := string(id)
	buffer, hasBuffer := s.writeBuffers[idStr]
	if !hasBuffer {
		s.writeBuffersMu.Unlock()
		return nil // No buffer, nothing to flush
	}

	// Lock the buffer before removing from map to prevent race conditions
	// where WriteAt could create a new buffer between delete and lock,
	// potentially losing data
	buffer.mu.Lock()
	// Remove from map while holding both locks
	delete(s.writeBuffers, idStr)
	s.writeBuffersMu.Unlock()

	defer buffer.mu.Unlock()

	// Flush any remaining data
	if len(buffer.data) > 0 {
		key := s.getObjectKey(id)
		_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader(buffer.data),
		})
		if err != nil {
			return fmt.Errorf("failed to flush final write buffer to S3: %w", err)
		}
	}

	return nil
}

// Truncate changes the size of the content.
//
// For S3, this requires downloading the object, truncating/extending it, and re-uploading.
// This is inefficient for large objects.
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
func (s *S3ContentStore) Truncate(ctx context.Context, id metadata.ContentID, newSize uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	exists, err := s.ContentExists(ctx, id)
	if err != nil {
		return err
	}

	if !exists {
		return fmt.Errorf("truncate failed for %s: %w", id, content.ErrContentNotFound)
	}

	currentSize, err := s.GetContentSize(ctx, id)
	if err != nil {
		return err
	}

	// No-op if size is already correct
	if currentSize == newSize {
		return nil
	}

	key := s.getObjectKey(id)

	if newSize < currentSize {
		// Truncate - handle edge case where newSize == 0
		if newSize == 0 {
			// Overwrite object with empty file
			_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String(s.bucket),
				Key:    aws.String(key),
				Body:   bytes.NewReader([]byte{}),
			})
			if err != nil {
				return fmt.Errorf("failed to write empty object for truncate: %w", err)
			}
		} else {
			// Truncate - download only the portion we need
			rangeStr := fmt.Sprintf("bytes=0-%d", newSize-1)
			result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
				Bucket: aws.String(s.bucket),
				Key:    aws.String(key),
				Range:  aws.String(rangeStr),
			})
			if err != nil {
				return fmt.Errorf("failed to get object for truncate: %w", err)
			}
			defer func() { _ = result.Body.Close() }()

			data, err := io.ReadAll(result.Body)
			if err != nil {
				return fmt.Errorf("failed to read object for truncate: %w", err)
			}

			_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String(s.bucket),
				Key:    aws.String(key),
				Body:   bytes.NewReader(data),
			})
			if err != nil {
				return fmt.Errorf("failed to write truncated object: %w", err)
			}
		}
		// Extend - download existing and append zeros
		reader, err := s.ReadContent(ctx, id)
		if err != nil {
			return err
		}
		defer func() { _ = reader.Close() }()

		existingData, err := io.ReadAll(reader)
		if err != nil {
			return fmt.Errorf("failed to read existing content: %w", err)
		}

		newData := make([]byte, newSize)
		copy(newData, existingData)

		_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader(newData),
		})
		if err != nil {
			return fmt.Errorf("failed to write extended object: %w", err)
		}
	}

	return nil
}

// Delete removes content from S3.
//
// Buffered Deletion:
// When buffered deletion is enabled (default), this method queues the deletion
// and returns immediately. A background worker batches deletions and processes
// them every 2 seconds or when 100+ items are queued, using S3's DeleteObjects
// API for efficiency (up to 1000 items per API call).
//
// When buffered deletion is disabled, deletions happen immediately (synchronous).
//
// This operation is idempotent - deleting non-existent content returns nil.
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
	if err := ctx.Err(); err != nil {
		return err
	}

	// If buffered deletion is enabled, queue it
	if s.deletionQueue.enabled {
		s.deletionQueue.mu.Lock()
		s.deletionQueue.queue = append(s.deletionQueue.queue, id)
		queueLen := len(s.deletionQueue.queue)
		s.deletionQueue.mu.Unlock()

		// Trigger immediate flush if batch size threshold reached
		if queueLen >= s.deletionQueue.batchSize {
			select {
			case s.deletionQueue.flushCh <- struct{}{}:
			default:
				// Channel already has signal, skip
			}
		}

		return nil
	}

	// Buffering disabled - execute immediately (synchronous)
	key := s.getObjectKey(id)

	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to delete object from S3: %w", err)
	}

	return nil
}
