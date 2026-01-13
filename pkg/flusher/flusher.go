// Package flusher implements eager block upload and parallel download for the cache-to-S3 integration.
//
// The flusher is responsible for:
//   - Eager upload: Upload 4MB blocks as soon as they're ready (don't wait for COMMIT)
//   - Flush: Wait for in-flight uploads and flush remaining partial blocks on COMMIT/CLOSE
//   - Download: Fetch blocks from S3 on cache miss, cache them for future reads
//
// Key Design Principles:
//   - Maximize bandwidth: Upload blocks as soon as 4MB is available
//   - Parallel I/O: Upload/download multiple blocks concurrently
//   - Protocol agnostic: Works with both NFS COMMIT and SMB CLOSE
//   - Share-aware keys: S3 keys include share name for multi-tenant support
package flusher

import (
	"context"
	"fmt"
	"sync"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/store/block"
)

// BlockSize is the size of a single block (4MB).
const BlockSize = 4 * 1024 * 1024

// DefaultParallelUploads is the default number of concurrent uploads per file.
const DefaultParallelUploads = 4

// DefaultParallelDownloads is the default number of concurrent downloads per file.
const DefaultParallelDownloads = 4

// Config holds configuration for the Flusher.
type Config struct {
	// ParallelUploads is the number of concurrent block uploads per file.
	// Default: 4
	ParallelUploads int

	// ParallelDownloads is the number of concurrent block downloads per file.
	// Default: 4
	ParallelDownloads int
}

// DefaultConfig returns the default flusher configuration.
func DefaultConfig() Config {
	return Config{
		ParallelUploads:   DefaultParallelUploads,
		ParallelDownloads: DefaultParallelDownloads,
	}
}

// fileUploadState tracks in-flight uploads for a single file.
type fileUploadState struct {
	inFlight sync.WaitGroup       // Tracks in-flight uploads
	errors   []error              // Accumulated errors
	errorsMu sync.Mutex           // Protects errors
	sem      chan struct{}        // Bounded parallelism
	blocksMu sync.Mutex           // Protects uploadedBlocks
	uploaded map[blockKey]bool    // Tracks which blocks have been uploaded
}

// blockKey uniquely identifies a block within a file.
type blockKey struct {
	chunkIdx uint32
	blockIdx uint32
}

// Flusher handles eager upload and parallel download for cache-to-S3 integration.
type Flusher struct {
	cache      *cache.Cache
	blockStore block.Store
	config     Config

	// Per-file upload tracking
	uploads   map[string]*fileUploadState // contentID -> state
	uploadsMu sync.Mutex

	// Shutdown
	closed bool
	mu     sync.RWMutex
}

// New creates a new Flusher.
//
// Parameters:
//   - c: The cache to flush from/to
//   - store: The block store to flush to (S3)
//   - config: Flusher configuration
func New(c *cache.Cache, store block.Store, config Config) *Flusher {
	if config.ParallelUploads <= 0 {
		config.ParallelUploads = DefaultParallelUploads
	}
	if config.ParallelDownloads <= 0 {
		config.ParallelDownloads = DefaultParallelDownloads
	}

	return &Flusher{
		cache:      c,
		blockStore: store,
		config:     config,
		uploads:    make(map[string]*fileUploadState),
	}
}

// getOrCreateUploadState returns the upload state for a file, creating it if needed.
func (f *Flusher) getOrCreateUploadState(contentID string) *fileUploadState {
	f.uploadsMu.Lock()
	defer f.uploadsMu.Unlock()

	state, exists := f.uploads[contentID]
	if !exists {
		state = &fileUploadState{
			sem:      make(chan struct{}, f.config.ParallelUploads),
			uploaded: make(map[blockKey]bool),
		}
		f.uploads[contentID] = state
	}
	return state
}

// getUploadState returns the upload state for a file, or nil if not found.
func (f *Flusher) getUploadState(contentID string) *fileUploadState {
	f.uploadsMu.Lock()
	defer f.uploadsMu.Unlock()
	return f.uploads[contentID]
}

// cleanupUploadState removes the upload state for a file.
func (f *Flusher) cleanupUploadState(contentID string) {
	f.uploadsMu.Lock()
	defer f.uploadsMu.Unlock()
	delete(f.uploads, contentID)
}

// ============================================================================
// Eager Upload
// ============================================================================

// OnWriteComplete is called after a write completes in the cache.
// It checks if any 4MB blocks are ready for upload and starts async uploads.
//
// Parameters:
//   - shareName: The share name (used in S3 key prefix)
//   - fileHandle: The file handle in the cache
//   - contentID: The content ID for S3 key generation
//   - chunkIdx: The chunk index that was written to
//   - offset: The offset within the chunk
//   - length: The length of data written
func (f *Flusher) OnWriteComplete(ctx context.Context, shareName string, fileHandle []byte,
	contentID string, chunkIdx uint32, offset, length uint32) {
	f.mu.RLock()
	if f.closed {
		f.mu.RUnlock()
		return
	}
	f.mu.RUnlock()

	// Calculate which blocks might be complete after this write
	endOffset := offset + length
	startBlockIdx := offset / BlockSize
	endBlockIdx := (endOffset - 1) / BlockSize

	// Check each potentially affected block
	for blockIdx := startBlockIdx; blockIdx <= endBlockIdx; blockIdx++ {
		// Only upload complete 4MB blocks (not partial blocks)
		blockStart := blockIdx * BlockSize
		blockEnd := blockStart + BlockSize

		// For eager upload, we only upload when we have a complete 4MB block
		// The last partial block is uploaded during Flush()
		if blockEnd > cache.ChunkSize {
			continue // Block extends beyond chunk boundary
		}

		// Check if we have complete data for this block
		data, found, err := f.cache.ReadSlice(ctx, fileHandle, chunkIdx, blockStart, BlockSize)
		if err != nil || !found {
			continue
		}

		// Verify we have a full block worth of data
		if uint32(len(data)) != BlockSize {
			continue
		}

		// Start async upload for this block
		f.startBlockUpload(ctx, shareName, fileHandle, contentID, chunkIdx, blockIdx, data)
	}
}

// startBlockUpload uploads a block asynchronously with bounded parallelism.
func (f *Flusher) startBlockUpload(ctx context.Context, shareName string, _ []byte,
	contentID string, chunkIdx, blockIdx uint32, data []byte) {
	state := f.getOrCreateUploadState(contentID)

	// Check if already uploaded
	key := blockKey{chunkIdx: chunkIdx, blockIdx: blockIdx}
	state.blocksMu.Lock()
	if state.uploaded[key] {
		state.blocksMu.Unlock()
		return
	}
	state.uploaded[key] = true // Mark as in-progress
	state.blocksMu.Unlock()

	// Acquire semaphore slot (blocks if at parallel limit)
	state.sem <- struct{}{}
	state.inFlight.Add(1)

	go func() {
		defer func() {
			<-state.sem
			state.inFlight.Done()
		}()

		// Generate S3 key: {contentID}/chunk-{n}/block-{n}
		// Note: contentID already includes the share name (e.g., "export/path/to/file")
		blockKey := fmt.Sprintf("%s/chunk-%d/block-%d", contentID, chunkIdx, blockIdx)

		if err := f.blockStore.WriteBlock(ctx, blockKey, data); err != nil {
			state.errorsMu.Lock()
			state.errors = append(state.errors, fmt.Errorf("upload block %s: %w", blockKey, err))
			state.errorsMu.Unlock()

			// Mark as not uploaded so it can be retried
			state.blocksMu.Lock()
			state.uploaded[key] = false
			state.blocksMu.Unlock()
			return
		}

		// Block uploaded successfully - the slice will be marked as flushed during Flush()
	}()
}

// ============================================================================
// Flush Operations
// ============================================================================

// WaitForUploads waits for all in-flight uploads for a file to complete.
// Called by ContentService.Flush() and FlushAndFinalize().
func (f *Flusher) WaitForUploads(ctx context.Context, contentID string) error {
	f.mu.RLock()
	if f.closed {
		f.mu.RUnlock()
		return fmt.Errorf("flusher is closed")
	}
	f.mu.RUnlock()

	state := f.getUploadState(contentID)
	if state == nil {
		return nil // No uploads for this file
	}

	// Wait for all in-flight uploads
	done := make(chan struct{})
	go func() {
		state.inFlight.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Check for accumulated errors
		state.errorsMu.Lock()
		errs := state.errors
		state.errorsMu.Unlock()

		if len(errs) > 0 {
			return fmt.Errorf("upload errors: %v", errs[0])
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// FlushRemaining uploads any remaining data that hasn't been uploaded yet.
// This handles partial blocks (< 4MB) that weren't uploaded during eager upload.
// Called on COMMIT/CLOSE to ensure all data is persisted.
func (f *Flusher) FlushRemaining(ctx context.Context, shareName string, fileHandle []byte, contentID string) error {
	f.mu.RLock()
	if f.closed {
		f.mu.RUnlock()
		return fmt.Errorf("flusher is closed")
	}
	f.mu.RUnlock()

	// Get all pending slices
	pending, err := f.cache.GetDirtySlices(ctx, fileHandle)
	if err != nil {
		if err == cache.ErrFileNotInCache {
			return nil // No data to flush
		}
		return err
	}

	if len(pending) == 0 {
		return nil
	}

	// Upload slices as blocks
	var wg sync.WaitGroup
	errCh := make(chan error, len(pending))
	sem := make(chan struct{}, f.config.ParallelUploads)

	for _, slice := range pending {
		wg.Add(1)
		sem <- struct{}{}

		go func(s cache.PendingSlice) {
			defer func() {
				<-sem
				wg.Done()
			}()

			// Upload slice data as blocks
			blockRefs, err := f.uploadSliceAsBlocks(ctx, shareName, contentID, s)
			if err != nil {
				errCh <- err
				return
			}

			// Mark slice as flushed in cache
			if err := f.cache.MarkSliceFlushed(ctx, fileHandle, s.ID, blockRefs); err != nil {
				errCh <- err
			}
		}(slice)
	}

	wg.Wait()
	close(errCh)

	// Collect errors
	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("flush errors: %v", errs[0])
	}

	// Cleanup upload state
	f.cleanupUploadState(contentID)

	return nil
}

// uploadSliceAsBlocks splits a slice into blocks and uploads each.
func (f *Flusher) uploadSliceAsBlocks(ctx context.Context, shareName, contentID string, slice cache.PendingSlice) ([]cache.BlockRef, error) {
	var blockRefs []cache.BlockRef
	data := slice.Data

	// Calculate the starting block index for this slice
	startBlockIdx := slice.Offset / BlockSize

	for blockIdx := startBlockIdx; len(data) > 0; blockIdx++ {
		// Calculate how much data goes into this block
		blockOffset := blockIdx * BlockSize
		var blockData []byte

		if slice.Offset > blockOffset {
			// Slice starts in the middle of this block
			offsetInBlock := slice.Offset - blockOffset
			blockSize := min(uint32(len(data)), BlockSize-offsetInBlock)
			blockData = data[:blockSize]
			data = data[blockSize:]
		} else {
			// Slice starts at or before this block
			blockSize := min(uint32(len(data)), BlockSize)
			blockData = data[:blockSize]
			data = data[blockSize:]
		}

		// Generate S3 key: {contentID}/chunk-{n}/block-{n}
		// Note: contentID already includes the share name
		blockKey := fmt.Sprintf("%s/chunk-%d/block-%d", contentID, slice.ChunkIndex, blockIdx)

		// Upload block
		if err := f.blockStore.WriteBlock(ctx, blockKey, blockData); err != nil {
			return nil, fmt.Errorf("upload block %s: %w", blockKey, err)
		}

		blockRefs = append(blockRefs, cache.BlockRef{
			ID:   blockKey,
			Size: uint32(len(blockData)),
		})
	}

	return blockRefs, nil
}

// ============================================================================
// Parallel Download (Cache Miss)
// ============================================================================

// ReadBlocks fetches blocks from S3 in parallel and caches them.
// Called when ReadAt() encounters a cache miss.
func (f *Flusher) ReadBlocks(ctx context.Context, shareName string, fileHandle []byte,
	contentID string, chunkIdx uint32, offset, length uint32) ([]byte, error) {
	f.mu.RLock()
	if f.closed {
		f.mu.RUnlock()
		return nil, fmt.Errorf("flusher is closed")
	}
	f.mu.RUnlock()

	// Calculate which blocks we need
	startBlockIdx := offset / BlockSize
	endBlockIdx := (offset + length - 1) / BlockSize

	// Fetch blocks in parallel
	numBlocks := endBlockIdx - startBlockIdx + 1
	blocks := make([][]byte, numBlocks)
	var wg sync.WaitGroup
	errCh := make(chan error, numBlocks)
	sem := make(chan struct{}, f.config.ParallelDownloads)

	for i := startBlockIdx; i <= endBlockIdx; i++ {
		wg.Add(1)
		sem <- struct{}{}

		go func(blockIdx uint32, resultIdx int) {
			defer func() {
				<-sem
				wg.Done()
			}()

			// Generate S3 key: {contentID}/chunk-{n}/block-{n}
			// Note: contentID already includes the share name
			blockKey := fmt.Sprintf("%s/chunk-%d/block-%d", contentID, chunkIdx, blockIdx)

			data, err := f.blockStore.ReadBlock(ctx, blockKey)
			if err != nil {
				errCh <- fmt.Errorf("read block %s: %w", blockKey, err)
				return
			}

			blocks[resultIdx] = data

			// Cache the downloaded block as flushed (evictable)
			// We create a new slice for this block
			blockOffset := blockIdx * BlockSize
			if err := f.cache.WriteSlice(ctx, fileHandle, chunkIdx, data, blockOffset); err != nil {
				// Non-fatal: block was read successfully, just not cached
			} else {
				// Mark as flushed since it's already in S3
				// Note: This is a simplification - ideally we'd have a method to add
				// an already-flushed slice to the cache
			}
		}(i, int(i-startBlockIdx))
	}

	wg.Wait()
	close(errCh)

	// Check for errors
	for err := range errCh {
		return nil, err
	}

	// Assemble result from blocks
	return assembleBlocks(blocks, offset, length, startBlockIdx), nil
}

// assembleBlocks combines block data into the requested byte range.
func assembleBlocks(blocks [][]byte, offset, length, startBlockIdx uint32) []byte {
	result := make([]byte, length)
	written := uint32(0)

	for i, blockData := range blocks {
		if blockData == nil {
			continue
		}

		blockIdx := startBlockIdx + uint32(i)
		blockStart := blockIdx * BlockSize
		blockEnd := blockStart + uint32(len(blockData))

		// Calculate overlap with requested range
		overlapStart := max(offset, blockStart)
		overlapEnd := min(offset+length, blockEnd)

		if overlapStart >= overlapEnd {
			continue
		}

		// Copy overlapping data
		srcStart := overlapStart - blockStart
		srcEnd := overlapEnd - blockStart
		dstStart := overlapStart - offset

		copy(result[dstStart:], blockData[srcStart:srcEnd])
		written += overlapEnd - overlapStart
	}

	return result[:written]
}

// ============================================================================
// Lifecycle
// ============================================================================

// Close shuts down the flusher.
func (f *Flusher) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return nil
	}

	f.closed = true
	return nil
}

// HealthCheck verifies the block store is accessible.
func (f *Flusher) HealthCheck(ctx context.Context) error {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if f.closed {
		return fmt.Errorf("flusher is closed")
	}

	if f.blockStore == nil {
		return fmt.Errorf("no block store configured")
	}

	return f.blockStore.HealthCheck(ctx)
}
