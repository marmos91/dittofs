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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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
// DEPRECATED: This method is inefficient for S3 (requires read-modify-write).
// Use FlushableContentStore interface with WriteCache instead for proper buffering.
//
// This implementation is kept for compatibility but should not be used in production.
// The NFS handlers should accumulate writes in a cache and call FlushWrites() when ready.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//   - data: Data to write
//   - offset: Byte offset where writing begins
//
// Returns:
//   - error: Returns error indicating this method should not be used
func (s *S3ContentStore) WriteAt(ctx context.Context, id metadata.ContentID, data []byte, offset int64) error {
	return fmt.Errorf("WriteAt is not supported for S3ContentStore - use FlushableContentStore interface with WriteCache instead")
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

// FlushWrites flushes cached writes for a specific content ID to S3.
//
// This method reads buffered writes from the WriteCache (if available) and
// uploads them to S3. The upload strategy depends on the data size:
//
//   - Small files (< multipartThreshold): Single PutObject operation
//   - Large files (>= multipartThreshold): Multipart upload for efficiency
//
// This implements the FlushableContentStore interface and is called by:
//   - NFS handlers when stable write is requested (DataSyncWrite/FileSyncWrite)
//   - NFS handlers when cache size exceeds FlushThreshold()
//   - COMMIT procedure to ensure data durability
//   - Auto-flush timeout worker (for macOS compatibility)
//
// After successful upload, the cache entry is cleared to free memory.
//
// Context Cancellation:
// S3 operations respect context cancellation.
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

	// Check if WriteCache is available
	if s.writeCache == nil {
		// No cache configured - nothing to flush
		// This can happen if:
		// - S3ContentStore is used directly without NFS handler
		// - Cache was not injected during initialization
		return nil
	}

	// Get cache size first to determine upload strategy
	cacheSize := s.writeCache.Size(id)
	if cacheSize == 0 {
		// No data in cache, nothing to flush
		return nil
	}

	key := s.getObjectKey(id)
	dataSize := uint64(cacheSize)

	// Choose upload strategy based on data size
	if dataSize < s.multipartThreshold {
		// ====================================================================
		// SMALL FILE PATH: Simple PutObject (< multipartThreshold)
		// ====================================================================
		// For files smaller than the multipart threshold, use a single
		// PutObject operation. This is simpler and more efficient than
		// multipart upload for small files.
		//
		// For small files, it's acceptable to load entire content into memory.

		data, err := s.writeCache.ReadAll(id)
		if err != nil {
			return fmt.Errorf("failed to read from write cache: %w", err)
		}

		_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader(data),
		})
		if err != nil {
			return fmt.Errorf("failed to flush content to S3 (PutObject): %w", err)
		}
	} else {
		// ====================================================================
		// LARGE FILE PATH: Multipart upload (>= multipartThreshold)
		// ====================================================================
		// For files at or above the multipart threshold, use multipart upload.
		// This is more efficient for large files and required for files > 5GB.
		//
		// IMPORTANT: For large files, we read from cache in chunks (partSize)
		// to avoid loading the entire file into memory. This is critical for
		// handling 100MB+ files without memory exhaustion.
		//
		// Process:
		// 1. Begin multipart upload
		// 2. Read cache in partSize chunks using ReadAt()
		// 3. Upload each chunk as a part
		// 4. Complete multipart upload
		// 5. Abort on any error

		uploadID, err := s.BeginMultipartUpload(ctx, id)
		if err != nil {
			return fmt.Errorf("failed to begin multipart upload: %w", err)
		}

		// Track upload state for cleanup on error
		var uploadErr error
		defer func() {
			if uploadErr != nil {
				// Abort multipart upload on error to avoid orphaned parts
				_ = s.AbortMultipartUpload(ctx, id, uploadID)
			}
		}()

		// Read and upload data in chunks to avoid memory exhaustion
		offset := int64(0)
		partNumber := 1
		partNumbers := make([]int, 0)

		// Allocate a single buffer for reading parts (reused for each part)
		partBuffer := make([]byte, s.partSize)

		for offset < cacheSize {
			// Check for cancellation before each part
			if err := ctx.Err(); err != nil {
				uploadErr = err
				return fmt.Errorf("flush cancelled during multipart upload: %w", err)
			}

			// Determine how many bytes to read for this part
			remainingBytes := cacheSize - offset
			bytesToRead := int(s.partSize)
			if int64(bytesToRead) > remainingBytes {
				bytesToRead = int(remainingBytes)
			}

			// Read chunk from cache
			partData := partBuffer[:bytesToRead]
			n, readErr := s.writeCache.ReadAt(id, partData, offset)
			if readErr != nil && readErr != io.EOF {
				uploadErr = readErr
				return fmt.Errorf("failed to read chunk from cache at offset %d: %w", offset, readErr)
			}

			// Upload part (only the bytes actually read)
			if err := s.UploadPart(ctx, id, uploadID, partNumber, partData[:n]); err != nil {
				uploadErr = err
				return fmt.Errorf("failed to upload part %d: %w", partNumber, err)
			}

			partNumbers = append(partNumbers, partNumber)
			offset += int64(n)
			partNumber++
		}

		// Complete multipart upload
		if err := s.CompleteMultipartUpload(ctx, id, uploadID, partNumbers); err != nil {
			uploadErr = err
			return fmt.Errorf("failed to complete multipart upload: %w", err)
		}
	}

	// Clear cache after successful upload
	if resetErr := s.writeCache.Reset(id); resetErr != nil {
		// Log warning but don't fail the flush operation
		// The data is already in S3, so the operation succeeded
		return fmt.Errorf("flush succeeded but cache reset failed: %w", resetErr)
	}

	return nil
}

// FlushThreshold returns the recommended size threshold for flushing writes.
//
// For S3, this returns the configured multipart threshold, which is the minimum
// size at which multipart uploads become beneficial. This is typically set to
// the S3 multipart minimum (5MB) but can be configured during store creation.
//
// The threshold serves multiple purposes:
//   - Triggers automatic flush when cache size exceeds this value
//   - Determines upload strategy (PutObject vs multipart)
//   - Keeps memory usage bounded for cached writes
//   - Balances API call frequency with memory efficiency
//
// The NFS handlers use this threshold to decide when to automatically flush
// cached writes to S3, even without an explicit COMMIT or stable write request.
//
// This implements the FlushableContentStore interface.
//
// Returns:
//   - uint64: Flush threshold in bytes (typically 5MB = 5 * 1024 * 1024)
func (s *S3ContentStore) FlushThreshold() uint64 {
	// Return the configured multipart threshold
	// This was set during NewS3ContentStore() initialization
	if s.multipartThreshold > 0 {
		return s.multipartThreshold
	}

	// Default to 5MB if not set (should not happen in practice)
	// 5MB is the minimum size for S3 multipart upload parts
	return 5 * 1024 * 1024
}

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
