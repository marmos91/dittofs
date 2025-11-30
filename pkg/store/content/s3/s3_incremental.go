// Package s3 implements S3-based content storage for DittoFS.
//
// This file contains incremental write operations for the S3 content store,
// enabling efficient uploads of large files via multipart uploads during
// partial COMMIT operations.
package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// incrementalWriteSession tracks state for an incremental write.
type incrementalWriteSession struct {
	uploadID          string
	currentPartNumber int
	buffer            *bytes.Buffer
	totalFlushed      int64
	totalReceived     int64 // Total bytes received (including buffered)
	mu                sync.Mutex
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
// This creates an S3 multipart upload session and prepares for incremental
// flushing of cached data. Multiple calls with the same ID are idempotent
// (returns existing session if already started).
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//
// Returns:
//   - string: Upload ID for this multipart upload session
//   - error: Returns error if session cannot be initiated
func (s *S3ContentStore) BeginIncrementalWrite(ctx context.Context, id metadata.ContentID) (string, error) {
	// Check if session already exists
	incrementalSessionsMu.RLock()
	existing, exists := incrementalSessions[id]
	incrementalSessionsMu.RUnlock()

	if exists {
		// Idempotent: return existing upload ID
		return existing.uploadID, nil
	}

	// Create new multipart upload session
	uploadID, err := s.BeginMultipartUpload(ctx, id)
	if err != nil {
		return "", fmt.Errorf("failed to begin multipart upload: %w", err)
	}

	// Create and store session state
	session := &incrementalWriteSession{
		uploadID:          uploadID,
		currentPartNumber: 1,
		buffer:            bytes.NewBuffer(nil),
		totalFlushed:      0,
	}

	incrementalSessionsMu.Lock()
	incrementalSessions[id] = session
	incrementalSessionsMu.Unlock()

	return uploadID, nil
}

// FlushIncremental reads from cache and writes to S3 incrementally.
//
// The store implementation:
//  1. Reads new bytes from cache using cache.GetFlushedOffset() and cache.ReadAt()
//  2. Buffers data until enough for an upload (5MB for S3 multipart)
//  3. Uploads when buffer threshold is reached
//  4. Updates cache.SetFlushedOffset() after successful upload
//
// Returns the number of bytes actually uploaded to S3 (may be 0 if still buffering).
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//   - c: Cache to read data from (store manages offsets and updates FlushedOffset)
//
// Returns:
//   - flushed: Number of bytes actually uploaded to S3 (0 if buffering)
//   - error: Returns error if read or upload fails
func (s *S3ContentStore) FlushIncremental(ctx context.Context, id metadata.ContentID, c cache.Cache) (int64, error) {
	// Get session
	incrementalSessionsMu.RLock()
	session, exists := incrementalSessions[id]
	incrementalSessionsMu.RUnlock()

	if !exists {
		return 0, fmt.Errorf("no incremental write session found for %s (call BeginIncrementalWrite first)", id)
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	// Read new bytes from cache since last flush
	cacheSize := c.Size(id)
	flushedOffset := c.GetFlushedOffset(id)
	bytesToRead := cacheSize - flushedOffset

	if bytesToRead <= 0 {
		// No new data in cache
		return 0, nil
	}

	// Read new bytes from cache
	buf := make([]byte, bytesToRead)
	n, err := c.ReadAt(ctx, id, buf, flushedOffset)
	if err != nil && err != io.EOF {
		return 0, fmt.Errorf("failed to read from cache: %w", err)
	}
	newData := buf[:n]

	// Add new data to internal buffer
	written, err := session.buffer.Write(newData)
	if err != nil {
		return 0, fmt.Errorf("failed to buffer data: %w", err)
	}
	if written != len(newData) {
		return 0, fmt.Errorf("partial buffer write: wrote %d of %d bytes", written, len(newData))
	}

	// Update total received
	session.totalReceived += int64(n)

	// Check if we have enough data to upload a part (5MB minimum for S3)
	const minPartSize = 5 * 1024 * 1024 // 5MB
	bufferSize := session.buffer.Len()

	if bufferSize < minPartSize {
		// Not enough data yet - update flushed offset to mark bytes as "received by store"
		c.SetFlushedOffset(id, flushedOffset+int64(n))
		return 0, nil
	}

	// CRITICAL PERFORMANCE FIX:
	// Upload ONLY ONE part per COMMIT to avoid blocking the NFS client.
	// Uploading all buffered parts at once can take 20+ seconds for large buffers,
	// causing "NFS server not responding" warnings on the client.
	//
	// By uploading one part at a time (~5 seconds per COMMIT), we:
	// - Keep COMMIT operations fast and responsive
	// - Maintain RFC 1813 compliance (data is committed to S3 before returning success)
	// - Allow incremental progress on large file uploads
	// - Let subsequent COMMITs handle remaining buffered data

	// Read exactly one part from buffer
	partData := make([]byte, minPartSize)
	nRead, err := session.buffer.Read(partData)
	if err != nil {
		return 0, fmt.Errorf("failed to read from buffer: %w", err)
	}
	partData = partData[:nRead] // Trim to actual size

	// Upload this single part synchronously (COMMIT waits for this to complete)
	err = s.UploadPart(ctx, id, session.uploadID, session.currentPartNumber, partData)
	if err != nil {
		return 0, fmt.Errorf("failed to upload part %d: %w", session.currentPartNumber, err)
	}

	// Update session state
	partsFlushed := int64(len(partData))
	session.totalFlushed += partsFlushed
	session.currentPartNumber++

	// Update cache flushed offset
	c.SetFlushedOffset(id, flushedOffset+int64(n))

	return partsFlushed, nil
}

// CompleteIncrementalWrite finalizes an incremental write session.
//
// This:
//  1. Uploads any remaining buffered data as the final part
//  2. Completes the S3 multipart upload
//  3. Cleans up session state
//
// After this call, the content is available for reading via ReadContent().
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//
// Returns:
//   - error: Returns error if completion fails
func (s *S3ContentStore) CompleteIncrementalWrite(ctx context.Context, id metadata.ContentID) error {
	// Get and remove session
	incrementalSessionsMu.Lock()
	session, exists := incrementalSessions[id]
	if exists {
		delete(incrementalSessions, id)
	}
	incrementalSessionsMu.Unlock()

	if !exists {
		// Idempotent: no session means already completed or never started
		return nil
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	// Determine how many parts we actually uploaded
	// currentPartNumber always points to the NEXT part to upload
	// If currentPartNumber = 4, we've uploaded parts 1, 2, 3
	actualPartCount := session.currentPartNumber - 1

	// Upload any remaining buffered data as final part
	if session.buffer.Len() > 0 {
		finalPartData := session.buffer.Bytes()
		err := s.UploadPart(ctx, id, session.uploadID, session.currentPartNumber, finalPartData)
		if err != nil {
			return fmt.Errorf("failed to upload final part %d: %w", session.currentPartNumber, err)
		}
		session.totalFlushed += int64(len(finalPartData))
		actualPartCount = session.currentPartNumber // Include the final part we just uploaded
	}

	// Only complete if we uploaded at least one part
	if actualPartCount == 0 {
		// No parts uploaded (empty file) - abort the multipart upload
		return s.AbortMultipartUpload(ctx, id, session.uploadID)
	}

	// Build part numbers list (1, 2, 3, ..., actualPartCount)
	partNumbers := make([]int, actualPartCount)
	for i := 0; i < actualPartCount; i++ {
		partNumbers[i] = i + 1
	}

	// Complete multipart upload
	err := s.CompleteMultipartUpload(ctx, id, session.uploadID, partNumbers)
	if err != nil {
		return fmt.Errorf("failed to complete multipart upload: %w", err)
	}

	return nil
}

// AbortIncrementalWrite cancels an incremental write session.
//
// This:
//  1. Aborts the S3 multipart upload (frees storage)
//  2. Discards any buffered data
//  3. Cleans up session state
//
// This operation is idempotent - aborting a non-existent session succeeds.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - id: Content identifier
//
// Returns:
//   - error: Returns error only for storage failures (idempotent)
func (s *S3ContentStore) AbortIncrementalWrite(ctx context.Context, id metadata.ContentID) error {
	// Get and remove session
	incrementalSessionsMu.Lock()
	session, exists := incrementalSessions[id]
	if exists {
		delete(incrementalSessions, id)
	}
	incrementalSessionsMu.Unlock()

	if !exists {
		// Idempotent: no session to abort
		return nil
	}

	// Abort multipart upload
	err := s.AbortMultipartUpload(ctx, id, session.uploadID)
	if err != nil {
		return fmt.Errorf("failed to abort multipart upload: %w", err)
	}

	return nil
}

// GetIncrementalWriteState returns the current state of an incremental write session.
//
// Returns nil if no incremental write session exists for this content ID.
//
// Parameters:
//   - id: Content identifier
//
// Returns:
//   - *IncrementalWriteState: Current state (nil if no session)
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
		UploadID:          session.uploadID,
		CurrentPartNumber: session.currentPartNumber,
		BufferedSize:      int64(session.buffer.Len()),
		TotalFlushed:      session.totalFlushed,
	}
}
