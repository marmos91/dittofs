package transfer

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/payload/block"
	"github.com/marmos91/dittofs/pkg/payload/chunk"
	"github.com/marmos91/dittofs/pkg/payload/store"
)

// BlockSize is the size of a single block (4MB).
// Re-exported from block package for convenience.
const BlockSize = block.Size

// DefaultParallelUploads is the default number of concurrent uploads per file.
const DefaultParallelUploads = 4

// DefaultParallelDownloads is the default number of concurrent downloads per file.
const DefaultParallelDownloads = 4

// Config holds configuration for the TransferManager.
type Config struct {
	// ParallelUploads is the number of concurrent block uploads per file.
	// Default: 4
	ParallelUploads int

	// ParallelDownloads is the number of concurrent block downloads per file.
	// Default: 4
	ParallelDownloads int
}

// DefaultConfig returns the default transfer manager configuration.
func DefaultConfig() Config {
	return Config{
		ParallelUploads:   DefaultParallelUploads,
		ParallelDownloads: DefaultParallelDownloads,
	}
}

// fileUploadState tracks in-flight uploads for a single file.
type fileUploadState struct {
	inFlight sync.WaitGroup    // Tracks in-flight uploads
	errors   []error           // Accumulated errors
	errorsMu sync.Mutex        // Protects errors
	sem      chan struct{}     // Bounded parallelism
	blocksMu sync.Mutex        // Protects uploadedBlocks
	uploaded map[blockKey]bool // Tracks which blocks have been uploaded
}

// blockKey uniquely identifies a block within a file.
type blockKey struct {
	chunkIdx uint32
	blockIdx uint32
}

// TransferManager handles eager upload and parallel download for cache-to-block-store integration.
type TransferManager struct {
	cache      *cache.Cache
	blockStore store.BlockStore
	config     Config

	// Per-file upload tracking
	uploads   map[string]*fileUploadState // payloadID -> state
	uploadsMu sync.Mutex

	// Transfer queue for non-blocking COMMIT
	queue *TransferQueue

	// Shutdown
	closed bool
	mu     sync.RWMutex
}

// New creates a new TransferManager.
//
// Parameters:
//   - c: The cache to transfer from/to
//   - store: The block store to transfer to
//   - config: TransferManager configuration
func New(c *cache.Cache, store store.BlockStore, config Config) *TransferManager {
	if config.ParallelUploads <= 0 {
		config.ParallelUploads = DefaultParallelUploads
	}
	if config.ParallelDownloads <= 0 {
		config.ParallelDownloads = DefaultParallelDownloads
	}

	m := &TransferManager{
		cache:      c,
		blockStore: store,
		config:     config,
		uploads:    make(map[string]*fileUploadState),
	}

	// Initialize transfer queue
	queueConfig := DefaultTransferQueueConfig()
	queueConfig.Workers = config.ParallelUploads
	m.queue = NewTransferQueue(m, queueConfig)

	return m
}

// getOrCreateUploadState returns the upload state for a file, creating it if needed.
func (m *TransferManager) getOrCreateUploadState(payloadID string) *fileUploadState {
	m.uploadsMu.Lock()
	defer m.uploadsMu.Unlock()

	state, exists := m.uploads[payloadID]
	if !exists {
		state = &fileUploadState{
			sem:      make(chan struct{}, m.config.ParallelUploads),
			uploaded: make(map[blockKey]bool),
		}
		m.uploads[payloadID] = state
	}
	return state
}

// getUploadState returns the upload state for a file, or nil if not found.
func (m *TransferManager) getUploadState(payloadID string) *fileUploadState {
	m.uploadsMu.Lock()
	defer m.uploadsMu.Unlock()
	return m.uploads[payloadID]
}

// cleanupUploadState removes the upload state for a file.
func (m *TransferManager) cleanupUploadState(payloadID string) {
	m.uploadsMu.Lock()
	defer m.uploadsMu.Unlock()
	delete(m.uploads, payloadID)
}

// ============================================================================
// Eager Upload
// ============================================================================

// OnWriteComplete is called after a write completes in the cache.
// It checks if any 4MB blocks are ready for upload and starts async uploads.
//
// Parameters:
//   - shareName: The share name (used in block key prefix)
//   - fileHandle: The file handle in the cache
//   - payloadID: The content ID for block key generation
//   - chunkIdx: The chunk index that was written to
//   - offset: The offset within the chunk
//   - length: The length of data written
func (m *TransferManager) OnWriteComplete(ctx context.Context, shareName string, fileHandle []byte,
	payloadID string, chunkIdx uint32, offset, length uint32) {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return
	}
	m.mu.RUnlock()

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

		// Check if the block is fully covered by cached data
		// This prevents uploading blocks with zero-filled gaps
		covered, err := m.cache.IsRangeCovered(ctx, fileHandle, chunkIdx, blockStart, BlockSize)
		if err != nil || !covered {
			continue
		}

		// Read the complete block data
		data, found, err := m.cache.ReadSlice(ctx, fileHandle, chunkIdx, blockStart, BlockSize)
		if err != nil || !found {
			continue
		}

		// Start async upload for this block
		m.startBlockUpload(ctx, shareName, fileHandle, payloadID, chunkIdx, blockIdx, data)
	}
}

// startBlockUpload uploads a block asynchronously with bounded parallelism.
func (m *TransferManager) startBlockUpload(ctx context.Context, _ string, _ []byte,
	payloadID string, chunkIdx, blockIdx uint32, data []byte) {
	state := m.getOrCreateUploadState(payloadID)

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

		// Generate block key: {payloadID}/chunk-{n}/block-{n}
		// Note: payloadID already includes the share name (e.g., "export/path/to/file")
		blockKeyStr := fmt.Sprintf("%s/chunk-%d/block-%d", payloadID, chunkIdx, blockIdx)

		if err := m.blockStore.WriteBlock(ctx, blockKeyStr, data); err != nil {
			state.errorsMu.Lock()
			state.errors = append(state.errors, fmt.Errorf("upload block %s: %w", blockKeyStr, err))
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
// Called by BlockService.FlushAndFinalize().
func (m *TransferManager) WaitForUploads(ctx context.Context, payloadID string) error {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return fmt.Errorf("transfer manager is closed")
	}
	m.mu.RUnlock()

	state := m.getUploadState(payloadID)
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
// This is the BLOCKING version - waits for block store uploads to complete.
// Use FlushRemainingAsync for non-blocking behavior.
//
// Deprecated: Use FlushRemainingAsync for better performance.
func (m *TransferManager) FlushRemaining(ctx context.Context, shareName string, fileHandle []byte, payloadID string) error {
	return m.flushRemainingSync(ctx, shareName, fileHandle, payloadID)
}

// FlushRemainingAsync enqueues remaining data for background upload.
// Returns immediately after enqueuing - does NOT wait for block store uploads.
// The cache should be synced before calling this to ensure crash recovery.
func (m *TransferManager) FlushRemainingAsync(ctx context.Context, shareName string, fileHandle []byte, payloadID string) error {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return fmt.Errorf("transfer manager is closed")
	}
	m.mu.RUnlock()

	// Create entry and enqueue for background upload (non-blocking)
	entry := NewDefaultEntry(shareName, fileHandle, payloadID)
	if !m.queue.Enqueue(entry) {
		// Queue full - fall back to sync upload
		return m.flushRemainingSync(ctx, shareName, fileHandle, payloadID)
	}

	return nil
}

// flushRemainingSync is the internal synchronous implementation.
// Called by FlushRemaining (blocking) and by background uploader.
func (m *TransferManager) flushRemainingSync(ctx context.Context, shareName string, fileHandle []byte, payloadID string) error {
	return m.flushRemainingSyncInternal(ctx, shareName, fileHandle, payloadID, true)
}

// flushRemainingSyncInternal is the internal implementation with markFlushed option.
func (m *TransferManager) flushRemainingSyncInternal(ctx context.Context, shareName string, fileHandle []byte, payloadID string, markFlushed bool) error {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return fmt.Errorf("transfer manager is closed")
	}
	m.mu.RUnlock()

	// Get all pending slices
	pending, err := m.cache.GetDirtySlices(ctx, fileHandle)
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
	sem := make(chan struct{}, m.config.ParallelUploads)

	for _, slice := range pending {
		wg.Add(1)
		sem <- struct{}{}

		go func(s cache.PendingSlice) {
			defer func() {
				<-sem
				wg.Done()
			}()

			// Upload slice data as blocks
			blockRefs, err := m.uploadSliceAsBlocks(ctx, shareName, payloadID, s)
			if err != nil {
				errCh <- err
				return
			}

			// Mark slice as flushed in cache (optional - skip during active writes to avoid lock contention)
			if markFlushed {
				// Note: ErrSliceNotFound is OK - slice may have been flushed by another
				// worker and then evicted by LRU. The data is safely in the block store.
				if err := m.cache.MarkSliceFlushed(ctx, fileHandle, s.ID, blockRefs); err != nil {
					if err != cache.ErrSliceNotFound {
						errCh <- err
					}
					// ErrSliceNotFound is expected in race conditions - ignore it
				}
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
	m.cleanupUploadState(payloadID)

	return nil
}

// uploadSliceAsBlocks splits a slice into blocks and uploads each.
func (m *TransferManager) uploadSliceAsBlocks(ctx context.Context, _, payloadID string, slice cache.PendingSlice) ([]cache.BlockRef, error) {
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

		// Generate block key: {payloadID}/chunk-{n}/block-{n}
		// Note: payloadID already includes the share name
		blockKeyStr := fmt.Sprintf("%s/chunk-%d/block-%d", payloadID, slice.ChunkIndex, blockIdx)

		// Upload block
		if err := m.blockStore.WriteBlock(ctx, blockKeyStr, blockData); err != nil {
			return nil, fmt.Errorf("upload block %s: %w", blockKeyStr, err)
		}

		blockRefs = append(blockRefs, cache.BlockRef{
			ID:   blockKeyStr,
			Size: uint32(len(blockData)),
		})
	}

	return blockRefs, nil
}

// ============================================================================
// Parallel Download (Cache Miss)
// ============================================================================

// ReadBlocks fetches blocks from the block store in parallel and caches them.
// Called when ReadAt() encounters a cache miss.
func (m *TransferManager) ReadBlocks(ctx context.Context, shareName string, fileHandle []byte,
	payloadID string, chunkIdx uint32, offset, length uint32) ([]byte, error) {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return nil, fmt.Errorf("transfer manager is closed")
	}
	m.mu.RUnlock()

	// Calculate which blocks we need
	startBlockIdx := offset / BlockSize
	endBlockIdx := (offset + length - 1) / BlockSize

	// Fetch blocks in parallel
	numBlocks := endBlockIdx - startBlockIdx + 1
	blocks := make([][]byte, numBlocks)
	var wg sync.WaitGroup
	errCh := make(chan error, numBlocks)
	sem := make(chan struct{}, m.config.ParallelDownloads)

	for i := startBlockIdx; i <= endBlockIdx; i++ {
		wg.Add(1)
		sem <- struct{}{}

		go func(blockIdx uint32, resultIdx int) {
			defer func() {
				<-sem
				wg.Done()
			}()

			// Generate block key: {payloadID}/chunk-{n}/block-{n}
			// Note: payloadID already includes the share name
			blockKeyStr := fmt.Sprintf("%s/chunk-%d/block-%d", payloadID, chunkIdx, blockIdx)

			data, err := m.blockStore.ReadBlock(ctx, blockKeyStr)
			if err != nil {
				errCh <- fmt.Errorf("read block %s: %w", blockKeyStr, err)
				return
			}

			blocks[resultIdx] = data

			// Cache the downloaded block as flushed (evictable)
			// We create a new slice for this block
			blockOffset := blockIdx * BlockSize
			if err := m.cache.WriteSlice(ctx, fileHandle, chunkIdx, data, blockOffset); err != nil {
				// Non-fatal: block was read successfully, just not cached
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
// Block Store Queries
// ============================================================================

// GetFileSize returns the total size of a file from the block store.
// This is used as a fallback when the cache doesn't have the file.
func (m *TransferManager) GetFileSize(ctx context.Context, shareName, payloadID string) (uint64, error) {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return 0, fmt.Errorf("transfer manager is closed")
	}
	blockStore := m.blockStore
	m.mu.RUnlock()

	if blockStore == nil {
		return 0, fmt.Errorf("no block store configured")
	}

	// List all blocks for this file
	prefix := payloadID + "/"
	blocks, err := blockStore.ListByPrefix(ctx, prefix)
	if err != nil {
		return 0, fmt.Errorf("list blocks: %w", err)
	}

	if len(blocks) == 0 {
		return 0, nil
	}

	// Calculate total size from blocks
	// Block key format: {payloadID}/chunk-{chunkIdx}/block-{blockIdx}
	var maxChunkIdx, maxBlockIdx uint32
	blockSizes := make(map[string]uint32) // blockKey -> size

	for _, blockKey := range blocks {
		var chunkIdx, blockIdx uint32
		// Parse chunk and block index from key
		_, err := fmt.Sscanf(blockKey, payloadID+"/chunk-%d/block-%d", &chunkIdx, &blockIdx)
		if err != nil {
			continue
		}

		// Track highest chunk/block for size calculation
		if chunkIdx > maxChunkIdx || (chunkIdx == maxChunkIdx && blockIdx > maxBlockIdx) {
			maxChunkIdx = chunkIdx
			maxBlockIdx = blockIdx
		}

		// Read block to get actual size (last block may be partial)
		data, err := blockStore.ReadBlock(ctx, blockKey)
		if err != nil {
			continue
		}
		blockSizes[blockKey] = uint32(len(data))
	}

	// Calculate total size
	// Full chunks: maxChunkIdx * ChunkSize
	// Last chunk: (maxBlockIdx * BlockSize) + last block size
	lastBlockKey := fmt.Sprintf("%s/chunk-%d/block-%d", payloadID, maxChunkIdx, maxBlockIdx)
	lastBlockSize := blockSizes[lastBlockKey]
	if lastBlockSize == 0 {
		lastBlockSize = BlockSize // Assume full block if we couldn't read it
	}

	totalSize := uint64(maxChunkIdx)*uint64(chunk.Size) +
		uint64(maxBlockIdx)*uint64(BlockSize) +
		uint64(lastBlockSize)

	return totalSize, nil
}

// Exists checks if any blocks exist for a file in the block store.
func (m *TransferManager) Exists(ctx context.Context, shareName, payloadID string) (bool, error) {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return false, fmt.Errorf("transfer manager is closed")
	}
	blockStore := m.blockStore
	m.mu.RUnlock()

	if blockStore == nil {
		return false, fmt.Errorf("no block store configured")
	}

	// List blocks for this file
	prefix := payloadID + "/"
	blocks, err := blockStore.ListByPrefix(ctx, prefix)
	if err != nil {
		return false, fmt.Errorf("list blocks: %w", err)
	}

	return len(blocks) > 0, nil
}

// Truncate removes blocks beyond the new size from the block store.
func (m *TransferManager) Truncate(ctx context.Context, shareName, payloadID string, newSize uint64) error {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return fmt.Errorf("transfer manager is closed")
	}
	blockStore := m.blockStore
	m.mu.RUnlock()

	if blockStore == nil {
		return fmt.Errorf("no block store configured")
	}

	// Calculate which chunk/block the new size falls into
	newChunkIdx := chunk.IndexForOffset(newSize)
	offsetInChunk := chunk.OffsetInChunk(newSize)
	newBlockIdx := block.IndexForOffset(offsetInChunk)

	// List all blocks for this file
	prefix := payloadID + "/"
	blocks, err := blockStore.ListByPrefix(ctx, prefix)
	if err != nil {
		return fmt.Errorf("list blocks: %w", err)
	}

	// Delete blocks beyond the new size
	for _, blockKey := range blocks {
		var chunkIdx, blockIdx uint32
		_, err := fmt.Sscanf(blockKey, payloadID+"/chunk-%d/block-%d", &chunkIdx, &blockIdx)
		if err != nil {
			continue
		}

		// Delete if beyond new size
		if chunkIdx > newChunkIdx || (chunkIdx == newChunkIdx && blockIdx > newBlockIdx) {
			if err := blockStore.DeleteBlock(ctx, blockKey); err != nil {
				return fmt.Errorf("delete block %s: %w", blockKey, err)
			}
		}
	}

	return nil
}

// Delete removes all blocks for a file from the block store.
func (m *TransferManager) Delete(ctx context.Context, shareName, payloadID string) error {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return fmt.Errorf("transfer manager is closed")
	}
	blockStore := m.blockStore
	m.mu.RUnlock()

	if blockStore == nil {
		return fmt.Errorf("no block store configured")
	}

	// Delete all blocks with this prefix
	prefix := payloadID + "/"
	if err := blockStore.DeleteByPrefix(ctx, prefix); err != nil {
		return fmt.Errorf("delete blocks: %w", err)
	}

	return nil
}

// ============================================================================
// Lifecycle
// ============================================================================

// Start begins background upload processing.
// Must be called after New() to enable async uploads.
func (m *TransferManager) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.queue != nil {
		m.queue.Start(ctx)
	}
}

// Close shuts down the transfer manager and waits for pending uploads.
func (m *TransferManager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()

	// Stop transfer queue with 30 second timeout
	if m.queue != nil {
		m.queue.Stop(30 * time.Second)
	}

	return nil
}

// HealthCheck verifies the block store is accessible.
func (m *TransferManager) HealthCheck(ctx context.Context) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.closed {
		return fmt.Errorf("transfer manager is closed")
	}

	if m.blockStore == nil {
		return fmt.Errorf("no block store configured")
	}

	return m.blockStore.HealthCheck(ctx)
}
