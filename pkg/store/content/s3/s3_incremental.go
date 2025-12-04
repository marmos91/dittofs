// Package s3 implements S3-based content storage for DittoFS.
//
// This file contains incremental write operations for the S3 content store,
// enabling efficient parallel uploads of large files via multipart uploads.
package s3

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/marmos91/dittofs/internal/logger"
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
	totalFlushed   int64
	mu             sync.Mutex
}

// Incremental write sessions map (content ID → session state)
var (
	incrementalSessions   = make(map[metadata.ContentID]*incrementalWriteSession)
	incrementalSessionsMu sync.RWMutex
)

// Compile-time check that S3ContentStore implements IncrementalWriteStore
var _ content.IncrementalWriteStore = (*S3ContentStore)(nil)

// BeginIncrementalWrite initiates an incremental write session.
//
// This is now mostly a no-op since we lazily create multipart uploads
// only when we have enough data. Kept for interface compatibility.
func (s *S3ContentStore) BeginIncrementalWrite(ctx context.Context, id metadata.ContentID) (string, error) {
	incrementalSessionsMu.RLock()
	existing, exists := incrementalSessions[id]
	incrementalSessionsMu.RUnlock()

	if exists && existing.uploadID != "" {
		return existing.uploadID, nil
	}

	// Don't create multipart upload yet - wait until we have enough data
	// Just ensure session exists
	incrementalSessionsMu.Lock()
	if _, ok := incrementalSessions[id]; !ok {
		incrementalSessions[id] = &incrementalWriteSession{
			uploadID:       "",
			uploadedParts:  make(map[int]bool),
			uploadingParts: make(map[int]bool),
			totalFlushed:   0,
		}
	}
	incrementalSessionsMu.Unlock()

	return "", nil
}

// FlushIncremental uploads complete parts from cache to S3 in parallel.
//
// This implementation:
//   - Returns 0 if cacheSize < partSize (small file, wait for finalization)
//   - Calculates which complete parts can be uploaded
//   - Finds parts not yet uploaded and not currently uploading
//   - Uploads selected parts in parallel (up to maxParallelUploads)
//   - Updates cache flushedOffset to highest contiguous uploaded position
func (s *S3ContentStore) FlushIncremental(ctx context.Context, id metadata.ContentID, c cache.Cache) (int64, error) {
	cacheSize := c.Size(id)
	if cacheSize == 0 {
		return 0, nil
	}

	// Small files: don't use multipart, wait for finalization to use PutObject
	if cacheSize < s.partSize {
		return 0, nil
	}

	// Get or create session
	incrementalSessionsMu.RLock()
	session, exists := incrementalSessions[id]
	incrementalSessionsMu.RUnlock()

	if !exists {
		session = &incrementalWriteSession{
			uploadID:       "",
			uploadedParts:  make(map[int]bool),
			uploadingParts: make(map[int]bool),
			totalFlushed:   0,
		}
		incrementalSessionsMu.Lock()
		if existing, ok := incrementalSessions[id]; ok {
			session = existing
		} else {
			incrementalSessions[id] = session
		}
		incrementalSessionsMu.Unlock()
	}

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
		logger.Info("FlushIncremental: started multipart upload: content_id=%s upload_id=%s", id, uploadID)
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
	var wg sync.WaitGroup
	type uploadResult struct {
		partNum int
		size    int64
		err     error
	}
	results := make(chan uploadResult, len(partsToUpload))

	for _, partNum := range partsToUpload {
		wg.Add(1)
		go func(pn int) {
			defer wg.Done()

			// Calculate offset range for this part
			startOffset := uint64(pn-1) * s.partSize
			partSize := s.partSize

			// Read part data directly from cache
			partData := make([]byte, partSize)
			n, err := c.ReadAt(ctx, id, partData, startOffset)
			if err != nil {
				results <- uploadResult{pn, 0, fmt.Errorf("failed to read part %d from cache: %w", pn, err)}
				return
			}
			partData = partData[:n]

			// Upload part
			if err := s.UploadPart(ctx, id, uploadID, pn, partData); err != nil {
				results <- uploadResult{pn, 0, fmt.Errorf("failed to upload part %d: %w", pn, err)}
				return
			}

			logger.Debug("FlushIncremental: uploaded part %d (%d bytes): content_id=%s", pn, n, id)
			results <- uploadResult{pn, int64(n), nil}
		}(partNum)
	}

	wg.Wait()
	close(results)

	// Process results - lock briefly to update state
	session.mu.Lock()
	var totalUploaded int64
	var firstErr error

	for result := range results {
		// Remove from uploading regardless of success/failure
		delete(session.uploadingParts, result.partNum)

		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			logger.Warn("FlushIncremental: part %d failed: %v", result.partNum, result.err)
			continue
		}

		// Mark as uploaded on success
		session.uploadedParts[result.partNum] = true
		session.totalFlushed += result.size
		totalUploaded += result.size
	}
	session.mu.Unlock()

	if firstErr != nil {
		return totalUploaded, firstErr
	}

	if totalUploaded > 0 {
		logger.Info("FlushIncremental: uploaded %d parts (%d bytes): content_id=%s", len(partsToUpload), totalUploaded, id)
	}

	return totalUploaded, nil
}

// calculateFlushedOffset returns the highest contiguous uploaded byte position.
// Must be called with session.mu held.
func (s *S3ContentStore) calculateFlushedOffset(session *incrementalWriteSession, cacheSize uint64) uint64 {
	if len(session.uploadedParts) == 0 {
		return 0
	}

	// Find highest contiguous part number
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

	// Calculate byte offset
	offset := uint64(highestContiguous) * s.partSize
	if offset > cacheSize {
		offset = cacheSize
	}
	return offset
}

// CompleteIncrementalWrite finalizes an incremental write session.
//
// For small files (< partSize): uses simple PutObject from cache
// For large files: uploads remaining parts + CompleteMultipartUpload
func (s *S3ContentStore) CompleteIncrementalWrite(ctx context.Context, id metadata.ContentID, c cache.Cache) error {
	cacheSize := c.Size(id)

	// Get session (but don't delete yet - only delete after successful completion)
	incrementalSessionsMu.RLock()
	session, exists := incrementalSessions[id]
	incrementalSessionsMu.RUnlock()

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
			logger.Info("CompleteIncrementalWrite: used PutObject for small file: content_id=%s size=%d", id, n)
		}
		// Delete session if it exists (for small files that had an empty session)
		if exists {
			incrementalSessionsMu.Lock()
			delete(incrementalSessions, id)
			incrementalSessionsMu.Unlock()
		}
		return nil
	}

	session.mu.Lock()

	// Calculate total parts needed (including final partial part)
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
		logger.Info("CompleteIncrementalWrite: uploading %d remaining parts in parallel: content_id=%s", len(remainingParts), id)

		// Use same parallelism as FlushIncremental
		maxConcurrent := s.maxParallelUploads
		if maxConcurrent == 0 {
			maxConcurrent = 4
		}

		var wg sync.WaitGroup
		semaphore := make(chan struct{}, maxConcurrent)
		errChan := make(chan error, len(remainingParts))

		for _, partNum := range remainingParts {
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
					return
				}

				// Read and upload
				partData := make([]byte, partSize)
				n, err := c.ReadAt(ctx, id, partData, startOffset)
				if err != nil {
					errChan <- fmt.Errorf("failed to read part %d from cache: %w", pn, err)
					return
				}

				if err := s.UploadPart(ctx, id, uploadID, pn, partData[:n]); err != nil {
					errChan <- fmt.Errorf("failed to upload part %d: %w", pn, err)
					return
				}

				session.mu.Lock()
				session.uploadedParts[pn] = true
				session.mu.Unlock()

				logger.Debug("CompleteIncrementalWrite: uploaded part %d (%d bytes): content_id=%s", pn, n, id)
			}(partNum)
		}

		wg.Wait()
		close(errChan)

		// Check for any errors
		for err := range errChan {
			return err
		}
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	// Build sorted part numbers list
	if len(session.uploadedParts) == 0 {
		// No parts uploaded - abort
		return s.AbortMultipartUpload(ctx, id, session.uploadID)
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
	incrementalSessionsMu.Lock()
	delete(incrementalSessions, id)
	incrementalSessionsMu.Unlock()

	logger.Info("CompleteIncrementalWrite: completed multipart upload: content_id=%s parts=%d", id, len(partNumbers))
	return nil
}

// AbortIncrementalWrite cancels an incremental write session.
func (s *S3ContentStore) AbortIncrementalWrite(ctx context.Context, id metadata.ContentID) error {
	incrementalSessionsMu.Lock()
	session, exists := incrementalSessions[id]
	if exists {
		delete(incrementalSessions, id)
	}
	incrementalSessionsMu.Unlock()

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
	incrementalSessionsMu.RLock()
	session, exists := incrementalSessions[id]
	incrementalSessionsMu.RUnlock()

	if !exists {
		return nil
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	return &content.IncrementalWriteState{
		UploadID:     session.uploadID,
		PartsWritten: len(session.uploadedParts),
		PartsWriting: len(session.uploadingParts),
		TotalFlushed: session.totalFlushed,
	}
}
