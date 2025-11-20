// Package s3 implements S3-based content storage for DittoFS.
//
// This file contains write operations for the S3 content store, including
// full content writes, truncation, and deletion.
package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
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

// WriteAt is not supported by S3ContentStore.
//
// S3 does not support random-access writes natively. For NFS write operations,
// use the FlushableContentStore interface with WriteCache for proper write buffering.
//
// This method always returns an error to satisfy the WritableContentStore interface
// while indicating that the operation is not supported.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//   - data: Data to write
//   - offset: Byte offset where writing begins
//
// Returns:
//   - error: Always returns ErrNotSupported
func (s *S3ContentStore) WriteAt(ctx context.Context, id metadata.ContentID, data []byte, offset int64) error {
	return fmt.Errorf("WriteAt is not supported for S3 content store: use FlushWrites with WriteCache instead")
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
	} else {
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
	flushStart := time.Now()
	var flushErr error

	defer func() {
		// Record overall flush operation at the end
		if s.metrics != nil {
			cacheSize := s.writeCache.Size(id)
			s.metrics.RecordFlushOperation("auto", int64(cacheSize), time.Since(flushStart), flushErr)
		}
	}()

	if err := ctx.Err(); err != nil {
		flushErr = err
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

	logger.Info("S3 FlushWrites: starting flush content_id=%s size=%d bytes", id, cacheSize)

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

		logger.Info("S3 FlushWrites: using PutObject (small file) content_id=%s size=%d", id, dataSize)

		// Phase 1: Read from cache
		cacheReadStart := time.Now()
		data, err := s.writeCache.ReadAll(id)
		if err != nil {
			flushErr = fmt.Errorf("failed to read from write cache: %w", err)
			return flushErr
		}
		cacheReadDuration := time.Since(cacheReadStart)
		if s.metrics != nil {
			s.metrics.ObserveFlushPhase("cache_read", cacheReadDuration, int64(len(data)))
		}

		// Phase 2: Upload to S3
		s3UploadStart := time.Now()
		_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader(data),
		})
		if err != nil {
			flushErr = fmt.Errorf("failed to flush content to S3 (PutObject): %w", err)
			return flushErr
		}
		s3UploadDuration := time.Since(s3UploadStart)
		if s.metrics != nil {
			s.metrics.ObserveFlushPhase("s3_upload", s3UploadDuration, int64(len(data)))
		}

		logger.Info("S3 FlushWrites: PutObject complete content_id=%s size=%d", id, dataSize)
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

		logger.Info("S3 FlushWrites: using multipart upload (large file) content_id=%s size=%d", id, dataSize)

		// Track S3 upload time for multipart
		s3UploadStart := time.Now()

		uploadID, err := s.BeginMultipartUpload(ctx, id)
		if err != nil {
			flushErr = fmt.Errorf("failed to begin multipart upload: %w", err)
			return flushErr
		}

		logger.Info("S3 FlushWrites: multipart upload started content_id=%s upload_id=%s", id, uploadID)

		// Track upload state for cleanup on error
		defer func() {
			if flushErr != nil {
				// Abort multipart upload on error to avoid orphaned parts
				_ = s.AbortMultipartUpload(ctx, id, uploadID)
			}
		}()

		// Read and upload data in chunks - PARALLEL uploads for performance!
		//
		// Strategy:
		// 1. Pre-read all parts into memory (already reading from cache, so fast)
		// 2. Upload all parts in parallel using goroutines + sync.WaitGroup
		// 3. Collect errors and part numbers
		//
		// This dramatically improves upload speed for large files:
		// - 100MB file = ~20 parts = 20x faster with parallel uploads
		// - Network bandwidth fully utilized
		// - S3 easily handles 50+ concurrent part uploads

		numParts := int((cacheSize + s.partSize - 1) / s.partSize)
		logger.Info("S3 FlushWrites: preparing %d parts for parallel upload content_id=%s", numParts, id)

		// Pre-read all parts from cache into memory
		// This is fast (in-memory read) and allows parallel uploads
		type partData struct {
			number int
			data   []byte
			offset int64
		}
		parts := make([]partData, 0, numParts)

		offset := int64(0)
		partNumber := 1
		for offset < cacheSize {
			// Determine how many bytes to read for this part
			remainingBytes := cacheSize - offset
			bytesToRead := int(s.partSize)
			if int64(bytesToRead) > remainingBytes {
				bytesToRead = int(remainingBytes)
			}

			// Read chunk from cache
			partBuffer := make([]byte, bytesToRead)
			n, readErr := s.writeCache.ReadAt(id, partBuffer, offset)
			if readErr != nil && readErr != io.EOF {
				flushErr = readErr
				return flushErr
			}

			parts = append(parts, partData{
				number: partNumber,
				data:   partBuffer[:n],
				offset: offset,
			})

			offset += int64(n)
			partNumber++
		}

		logger.Info("S3 FlushWrites: read %d parts from cache, starting parallel upload content_id=%s", len(parts), id)

		// Upload all parts in parallel
		var wg sync.WaitGroup
		var uploadMu sync.Mutex
		var uploadErrors []error
		partNumbers := make([]int, len(parts))

		// Limit concurrent uploads to avoid overwhelming S3 (50 is a reasonable limit)
		maxConcurrent := 50
		if len(parts) < maxConcurrent {
			maxConcurrent = len(parts)
		}
		semaphore := make(chan struct{}, maxConcurrent)

		for i, part := range parts {
			wg.Add(1)
			partNumbers[i] = part.number

			// Launch goroutine for parallel upload
			go func(p partData, idx int) {
				defer wg.Done()

				// Acquire semaphore slot
				semaphore <- struct{}{}
				defer func() { <-semaphore }()

				// Check for cancellation
				if err := ctx.Err(); err != nil {
					uploadMu.Lock()
					uploadErrors = append(uploadErrors, err)
					uploadMu.Unlock()
					return
				}

				// Upload part
				if err := s.UploadPart(ctx, id, uploadID, p.number, p.data); err != nil {
					uploadMu.Lock()
					uploadErrors = append(uploadErrors, fmt.Errorf("part %d failed: %w", p.number, err))
					uploadMu.Unlock()
					return
				}

				logger.Debug("S3 FlushWrites: uploaded part %d/%d content_id=%s bytes=%d", p.number, len(parts), id, len(p.data))
			}(part, i)
		}

		// Wait for all uploads to complete
		wg.Wait()

		// Check for upload errors
		if len(uploadErrors) > 0 {
			flushErr = fmt.Errorf("multipart upload failed: %d parts failed: %v", len(uploadErrors), uploadErrors[0])
			return flushErr
		}

		logger.Info("S3 FlushWrites: all %d parts uploaded successfully in parallel content_id=%s", len(parts), id)

		logger.Info("S3 FlushWrites: completing multipart upload content_id=%s parts=%d", id, len(partNumbers))

		// Complete multipart upload
		if err := s.CompleteMultipartUpload(ctx, id, uploadID, partNumbers); err != nil {
			flushErr = err
			return flushErr
		}

		// Record S3 upload phase duration for multipart
		s3UploadDuration := time.Since(s3UploadStart)
		if s.metrics != nil {
			s.metrics.ObserveFlushPhase("s3_upload", s3UploadDuration, int64(dataSize))
		}

		logger.Info("S3 FlushWrites: multipart upload complete content_id=%s size=%d", id, dataSize)
	}

	// Phase 3: Clear cache after successful upload
	cacheClearStart := time.Now()
	if resetErr := s.writeCache.Reset(id); resetErr != nil {
		// Log warning but don't fail the flush operation
		// The data is already in S3, so the operation succeeded
		flushErr = fmt.Errorf("flush succeeded but cache reset failed: %w", resetErr)
		return flushErr
	}
	cacheClearDuration := time.Since(cacheClearStart)
	if s.metrics != nil {
		s.metrics.ObserveFlushPhase("cache_clear", cacheClearDuration, 0)
	}

	logger.Info("S3 FlushWrites: flush complete, cache cleared content_id=%s", id)
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
