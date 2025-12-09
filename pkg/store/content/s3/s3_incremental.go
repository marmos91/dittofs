// Package s3 implements S3-based content storage for DittoFS.
//
// This file contains incremental write operations for the S3 content store,
// enabling efficient parallel uploads of large files via multipart uploads.
//
// Architecture:
//   - incrementalWriteSession tracks per-file upload state
//   - Sessions are stored per S3ContentStore instance (not global)
//   - FlushIncremental uploads complete parts during cache flush
//   - CompleteIncrementalWrite finalizes the multipart upload
//
// Called by:
//   - Cache flusher (pkg/cache/flusher) calls FlushIncremental during periodic sweeps
//   - Cache flusher calls CompleteIncrementalWrite when finalizing a file
package s3

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/telemetry"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// incrementalWriteSession tracks state for an incremental write.
// Designed for parallel part uploads based on offset ranges.
type incrementalWriteSession struct {
	uploadID       string
	uploadedParts  map[int]bool // Parts that completed successfully
	uploadingParts map[int]bool // Parts currently being uploaded
	totalFlushed   uint64
	mu             sync.Mutex
}

// Compile-time check that S3ContentStore implements IncrementalWriteStore
var _ content.IncrementalWriteStore = (*S3ContentStore)(nil)

// getOrCreateSession returns an existing session or creates a new one.
// Thread-safe.
func (s *S3ContentStore) getOrCreateSession(id metadata.ContentID) *incrementalWriteSession {
	s.incrementalSessionsMu.RLock()
	session, exists := s.incrementalSessions[id]
	s.incrementalSessionsMu.RUnlock()

	if exists {
		return session
	}

	// Create new session
	s.incrementalSessionsMu.Lock()
	defer s.incrementalSessionsMu.Unlock()

	// Double-check after acquiring write lock
	if existing, ok := s.incrementalSessions[id]; ok {
		return existing
	}

	session = &incrementalWriteSession{
		uploadID:       "",
		uploadedParts:  make(map[int]bool),
		uploadingParts: make(map[int]bool),
		totalFlushed:   0,
	}
	s.incrementalSessions[id] = session
	return session
}

// deleteSession removes a session from the store.
// Thread-safe.
func (s *S3ContentStore) deleteSession(id metadata.ContentID) {
	s.incrementalSessionsMu.Lock()
	delete(s.incrementalSessions, id)
	s.incrementalSessionsMu.Unlock()
}

// BeginIncrementalWrite initiates an incremental write session.
//
// This is mostly a no-op since we lazily create multipart uploads
// only when we have enough data. Kept for interface compatibility.
func (s *S3ContentStore) BeginIncrementalWrite(ctx context.Context, id metadata.ContentID) (string, error) {
	s.incrementalSessionsMu.RLock()
	existing, exists := s.incrementalSessions[id]
	s.incrementalSessionsMu.RUnlock()

	if exists && existing.uploadID != "" {
		return existing.uploadID, nil
	}

	// Ensure session exists (lazy creation)
	s.getOrCreateSession(id)
	return "", nil
}

// uploadPartResult contains the result of uploading a single part.
type uploadPartResult struct {
	partNum int
	size    uint64
	err     error
}

// uploadPartsInParallel uploads multiple parts concurrently.
// Returns total bytes uploaded and any error encountered.
func (s *S3ContentStore) uploadPartsInParallel(
	ctx context.Context,
	id metadata.ContentID,
	uploadID string,
	c cache.Cache,
	cacheSize uint64,
	partsToUpload []int,
	session *incrementalWriteSession,
) (uint64, error) {
	if len(partsToUpload) == 0 {
		return 0, nil
	}

	maxConcurrent := s.maxParallelUploads
	if maxConcurrent == 0 {
		maxConcurrent = 4
	}

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, maxConcurrent)
	results := make(chan uploadPartResult, len(partsToUpload))

	for _, partNum := range partsToUpload {
		wg.Add(1)
		semaphore <- struct{}{} // Acquire slot

		go func(pn int) {
			defer wg.Done()
			defer func() { <-semaphore }() // Release slot

			// Calculate offset range for this part
			startOffset := uint64(pn-1) * s.partSize
			endOffset := startOffset + s.partSize
			if endOffset > cacheSize {
				endOffset = cacheSize
			}
			partSize := endOffset - startOffset

			if partSize == 0 {
				results <- uploadPartResult{pn, 0, nil}
				return
			}

			// Read part data from cache
			partData := make([]byte, partSize)
			n, err := c.ReadAt(ctx, id, partData, startOffset)
			if err != nil {
				results <- uploadPartResult{pn, 0, fmt.Errorf("failed to read part %d from cache: %w", pn, err)}
				return
			}
			partData = partData[:n]

			// Upload with retry
			err = s.uploadPartWithRetry(ctx, id, uploadID, pn, partData)
			if err != nil {
				results <- uploadPartResult{pn, 0, err}
				return
			}

			logger.Debug("uploadPartsInParallel: uploaded part", "part", pn, "bytes", n, "content_id", id)
			results <- uploadPartResult{pn, uint64(n), nil}
		}(partNum)
	}

	wg.Wait()
	close(results)

	// Process results
	session.mu.Lock()
	defer session.mu.Unlock()

	var totalUploaded uint64
	var firstErr error

	for result := range results {
		// Remove from uploading regardless of success/failure
		delete(session.uploadingParts, result.partNum)

		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			logger.Warn("uploadPartsInParallel: part failed", "part", result.partNum, "error", result.err)
			continue
		}

		// Mark as uploaded on success
		session.uploadedParts[result.partNum] = true
		session.totalFlushed += result.size
		totalUploaded += result.size
	}

	return totalUploaded, firstErr
}

// uploadPartWithRetry uploads a single part with retry logic for transient errors.
func (s *S3ContentStore) uploadPartWithRetry(
	ctx context.Context,
	id metadata.ContentID,
	uploadID string,
	partNum int,
	data []byte,
) error {
	var lastErr error

	for attempt := 0; attempt <= int(s.retry.maxRetries); attempt++ {
		if attempt > 0 {
			backoff := s.calculateBackoff(attempt - 1)
			logger.Debug("uploadPartWithRetry: retrying after backoff", "backoff", backoff, "attempt", attempt, "max_retries", s.retry.maxRetries, "part", partNum)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		lastErr = s.UploadPart(ctx, id, uploadID, partNum, data)
		if lastErr == nil {
			return nil
		}

		if !isRetryableError(lastErr) {
			return fmt.Errorf("failed to upload part %d: %w", partNum, lastErr)
		}

		logger.Debug("uploadPartWithRetry: transient error", "attempt", attempt+1, "max_retries", s.retry.maxRetries+1, "part", partNum, "error", lastErr)
	}

	return fmt.Errorf("failed to upload part %d after %d attempts: %w", partNum, s.retry.maxRetries+1, lastErr)
}

// FlushIncremental uploads complete parts from cache to S3 in parallel.
//
// This implementation:
//   - Returns 0 if cacheSize < partSize (small file, wait for finalization)
//   - Calculates which complete parts can be uploaded
//   - Finds parts not yet uploaded and not currently uploading
//   - Uploads selected parts in parallel (up to maxParallelUploads)
//   - Updates cache flushedOffset to highest contiguous uploaded position
//
// Returns the number of bytes flushed and any error.
func (s *S3ContentStore) FlushIncremental(
	ctx context.Context,
	id metadata.ContentID,
	c cache.Cache,
) (uint64, error) {
	ctx, span := telemetry.StartContentSpan(ctx, "flush_incremental", string(id),
		telemetry.StoreName("s3"),
		telemetry.StoreType("content"))
	defer span.End()

	cacheSize := c.Size(id)
	if cacheSize == 0 {
		return 0, nil
	}

	// Small files: don't use multipart, wait for finalization to use PutObject
	if cacheSize < s.partSize {
		return 0, nil
	}

	session := s.getOrCreateSession(id)

	// Calculate how many COMPLETE parts we can upload
	// Only upload complete parts - final partial part is handled by CompleteIncrementalWrite
	numCompleteParts := cacheSize / s.partSize
	if numCompleteParts == 0 {
		return 0, nil
	}

	// Lock briefly to find parts to upload and mark them as uploading
	session.mu.Lock()

	// Lazily create multipart upload on first actual part upload
	if session.uploadID == "" {
		uploadID, err := s.BeginMultipartUpload(ctx, id)
		if err != nil {
			session.mu.Unlock()
			return 0, fmt.Errorf("failed to begin multipart upload: %w", err)
		}
		session.uploadID = uploadID
		logger.Info("FlushIncremental: started multipart upload", "content_id", id, "upload_id", uploadID)
	}
	uploadID := session.uploadID

	// Find parts that need uploading (not uploaded and not currently uploading)
	var partsToUpload []int
	for partNum := 1; partNum <= int(numCompleteParts); partNum++ {
		if !session.uploadedParts[partNum] && !session.uploadingParts[partNum] {
			partsToUpload = append(partsToUpload, partNum)
		}
	}

	if len(partsToUpload) == 0 {
		// Calculate flushed offset while still holding the lock
		flushedOffset := s.calculateFlushedOffset(session, cacheSize)
		session.mu.Unlock()
		// All parts already uploaded or uploading, update flushed offset
		c.SetFlushedOffset(id, flushedOffset)
		return 0, nil
	}

	// Limit concurrent uploads
	maxConcurrent := s.maxParallelUploads
	if maxConcurrent == 0 {
		maxConcurrent = 4
	}
	if uint(len(partsToUpload)) > maxConcurrent {
		partsToUpload = partsToUpload[:maxConcurrent]
	}

	// Mark selected parts as uploading
	for _, partNum := range partsToUpload {
		session.uploadingParts[partNum] = true
	}
	session.mu.Unlock()

	// Upload parts in parallel (blocking - wait for all to complete)
	// NFS COMMIT semantics require data to be on stable storage when we return
	totalUploaded, err := s.uploadPartsInParallel(ctx, id, uploadID, c, cacheSize, partsToUpload, session)

	if err != nil {
		return totalUploaded, err
	}

	if totalUploaded > 0 {
		logger.Info("FlushIncremental: uploaded parts", "parts", len(partsToUpload), "bytes", totalUploaded, "content_id", id)
	}

	return totalUploaded, nil
}

// calculateFlushedOffset returns the highest contiguous uploaded byte position.
// Must be called with session.mu held.
func (s *S3ContentStore) calculateFlushedOffset(session *incrementalWriteSession, cacheSize uint64) uint64 {
	if len(session.uploadedParts) == 0 {
		return 0
	}

	// Find highest contiguous part number starting from 1
	highestContiguous := 0
	for partNum := 1; ; partNum++ {
		if !session.uploadedParts[partNum] {
			break
		}
		highestContiguous = partNum
	}

	if highestContiguous == 0 {
		return 0
	}

	// Calculate byte offset (capped at cacheSize)
	return min(uint64(highestContiguous)*s.partSize, cacheSize)
}

// CompleteIncrementalWrite finalizes an incremental write session.
//
// For small files (< partSize): uses simple PutObject from cache
// For large files: uploads remaining parts + CompleteMultipartUpload
//
// This method is called by the cache flusher when a file write is finalized
// (detected via inactivity timeout).
func (s *S3ContentStore) CompleteIncrementalWrite(ctx context.Context, id metadata.ContentID, c cache.Cache) error {
	ctx, span := telemetry.StartContentSpan(ctx, "complete_incremental_write", string(id),
		telemetry.StoreName("s3"),
		telemetry.StoreType("content"))
	defer span.End()

	cacheSize := c.Size(id)

	// Get session (but don't delete yet - only delete after successful completion)
	s.incrementalSessionsMu.RLock()
	session, exists := s.incrementalSessions[id]
	s.incrementalSessionsMu.RUnlock()

	// Small file or no session: use simple PutObject
	if !exists || session.uploadID == "" {
		if cacheSize > 0 {
			data := make([]byte, cacheSize)
			n, err := c.ReadAt(ctx, id, data, 0)
			if err != nil {
				return fmt.Errorf("failed to read small file from cache: %w", err)
			}
			if err := s.WriteContent(ctx, id, data[:n]); err != nil {
				return fmt.Errorf("failed to write small file via PutObject: %w", err)
			}
			logger.Info("CompleteIncrementalWrite: used PutObject for small file", "content_id", id, "size", n)
		}
		// Delete session if it exists (for small files that had an empty session)
		if exists {
			s.deleteSession(id)
		}
		return nil
	}

	session.mu.Lock()

	// Calculate total parts needed (including final partial part)
	// Use ceiling division: (cacheSize + partSize - 1) / partSize
	totalParts := (cacheSize + s.partSize - 1) / s.partSize

	// Find parts that still need uploading
	var remainingParts []int
	for partNum := 1; partNum <= int(totalParts); partNum++ {
		if !session.uploadedParts[partNum] {
			remainingParts = append(remainingParts, partNum)
		}
	}
	uploadID := session.uploadID
	session.mu.Unlock()

	// Upload remaining parts in parallel for better performance
	if len(remainingParts) > 0 {
		logger.Info("CompleteIncrementalWrite: uploading remaining parts", "parts", len(remainingParts), "content_id", id)

		_, err := s.uploadPartsInParallel(ctx, id, uploadID, c, cacheSize, remainingParts, session)
		if err != nil {
			return err
		}
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	// Build sorted part numbers list
	if len(session.uploadedParts) == 0 {
		// No parts uploaded - abort the multipart upload
		if err := s.AbortMultipartUpload(ctx, id, session.uploadID); err != nil {
			logger.Warn("CompleteIncrementalWrite: failed to abort empty upload", "error", err)
		}
		s.deleteSession(id)
		return nil
	}

	partNumbers := make([]int, 0, len(session.uploadedParts))
	for partNum := range session.uploadedParts {
		partNumbers = append(partNumbers, partNum)
	}
	sort.Ints(partNumbers)

	// Complete multipart upload
	if err := s.CompleteMultipartUpload(ctx, id, session.uploadID, partNumbers); err != nil {
		return fmt.Errorf("failed to complete multipart upload: %w", err)
	}

	// Only delete session after successful completion to prevent race conditions
	// where a new COMMIT creates a fresh session and re-uploads everything
	s.deleteSession(id)

	logger.Info("CompleteIncrementalWrite: completed multipart upload", "content_id", id, "parts", len(partNumbers))
	return nil
}

// AbortIncrementalWrite cancels an incremental write session.
func (s *S3ContentStore) AbortIncrementalWrite(ctx context.Context, id metadata.ContentID) error {
	s.incrementalSessionsMu.Lock()
	session, exists := s.incrementalSessions[id]
	if exists {
		delete(s.incrementalSessions, id)
	}
	s.incrementalSessionsMu.Unlock()

	if !exists || session.uploadID == "" {
		return nil
	}

	if err := s.AbortMultipartUpload(ctx, id, session.uploadID); err != nil {
		return fmt.Errorf("failed to abort multipart upload: %w", err)
	}

	return nil
}

// GetIncrementalWriteState returns the current state of an incremental write session.
func (s *S3ContentStore) GetIncrementalWriteState(id metadata.ContentID) *content.IncrementalWriteState {
	s.incrementalSessionsMu.RLock()
	session, exists := s.incrementalSessions[id]
	s.incrementalSessionsMu.RUnlock()

	if !exists {
		return nil
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	return &content.IncrementalWriteState{
		UploadID:     session.uploadID,
		PartsWritten: len(session.uploadedParts),
		PartsWriting: len(session.uploadingParts),
		TotalFlushed: int64(session.totalFlushed),
	}
}
