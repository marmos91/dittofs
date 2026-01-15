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

// blockPool reuses 4MB buffers for block uploads to reduce GC pressure.
var blockPool = sync.Pool{
	New: func() interface{} { return make([]byte, BlockSize) },
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
//
// Key features:
//   - Eager upload: Uploads complete 4MB blocks immediately in background goroutines
//   - Download priority: Downloads pause uploads to minimize read latency
//   - Prefetch: Speculatively fetches upcoming blocks for sequential reads
//   - Parallel I/O: Configurable concurrency for both uploads and downloads
//   - In-flight deduplication: Avoids duplicate downloads for the same block
//   - Non-blocking: All operations return immediately, I/O happens in background
type TransferManager struct {
	cache      *cache.Cache
	blockStore store.BlockStore
	config     Config

	// Per-file upload tracking
	uploads   map[string]*fileUploadState // payloadID -> state
	uploadsMu sync.Mutex

	// Transfer queue for non-blocking operations
	queue *TransferQueue

	// Download priority: uploads pause when downloads are active
	ioCond           *sync.Cond // Condition variable for upload/download coordination
	downloadsPending int        // Count of active downloads (protected by ioCond.L)

	// In-flight download tracking: prevents duplicate downloads
	inFlight   map[string]chan error // blockKey -> completion channel
	inFlightMu sync.Mutex

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
		ioCond:     sync.NewCond(&sync.Mutex{}),
		inFlight:   make(map[string]chan error),
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
// Download Priority
// ============================================================================

// waitForDownloads blocks until no downloads are pending.
// Called by upload goroutines to yield to downloads.
func (m *TransferManager) waitForDownloads() {
	m.ioCond.L.Lock()
	for m.downloadsPending > 0 {
		m.ioCond.Wait()
	}
	m.ioCond.L.Unlock()
}

// ============================================================================
// Eager Upload
// ============================================================================

// OnWriteComplete is called after a write completes in the cache.
// It checks if any 4MB blocks are ready for upload and starts async uploads.
//
// Parameters:
//   - payloadID: The content ID (used for cache key and block key generation)
//   - chunkIdx: The chunk index that was written to
//   - offset: The offset within the chunk
//   - length: The length of data written
func (m *TransferManager) OnWriteComplete(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32) {
	if !m.canProcess(ctx) {
		return
	}

	startBlock, endBlock := blockRange(offset, length)
	for blockIdx := startBlock; blockIdx <= endBlock; blockIdx++ {
		m.tryEagerUpload(ctx, payloadID, chunkIdx, blockIdx)
	}
}

// canProcess returns false if the manager is closed or context is cancelled.
func (m *TransferManager) canProcess(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.closed
}

// blockRange returns the range of block indices that overlap with [offset, offset+length).
func blockRange(offset, length uint32) (start, end uint32) {
	start = offset / BlockSize
	end = (offset + length - 1) / BlockSize
	return
}

// tryEagerUpload checks if a block is complete and starts an async upload if ready.
// Only complete 4MB blocks are uploaded; partial blocks are flushed during Flush().
func (m *TransferManager) tryEagerUpload(ctx context.Context, payloadID string, chunkIdx, blockIdx uint32) {
	blockStart := blockIdx * BlockSize
	blockEnd := blockStart + BlockSize

	// Skip blocks that extend beyond chunk boundary
	if blockEnd > cache.ChunkSize {
		return
	}

	// Check if fully covered (no zero-filled gaps)
	covered, err := m.cache.IsRangeCovered(ctx, payloadID, chunkIdx, blockStart, BlockSize)
	if err != nil || !covered {
		return
	}

	// Read block data from cache
	data := blockPool.Get().([]byte)
	found, err := m.cache.ReadSlice(ctx, payloadID, chunkIdx, blockStart, BlockSize, data)
	if err != nil || !found {
		blockPool.Put(data)
		return
	}

	// Start async upload (takes ownership of data buffer)
	m.startBlockUpload(ctx, payloadID, chunkIdx, blockIdx, data)
}

// startBlockUpload uploads a block asynchronously with bounded parallelism.
//
// The data buffer is owned by this function and will be returned to blockPool
// after the upload completes or fails.
//
// Upload goroutines yield to downloads (download priority) before performing I/O.
func (m *TransferManager) startBlockUpload(ctx context.Context,
	payloadID string, chunkIdx, blockIdx uint32, data []byte) {
	state := m.getOrCreateUploadState(payloadID)

	// Check if already uploaded (deduplication)
	key := blockKey{chunkIdx: chunkIdx, blockIdx: blockIdx}
	state.blocksMu.Lock()
	if state.uploaded[key] {
		state.blocksMu.Unlock()
		blockPool.Put(data) // Return unused buffer
		return
	}
	state.uploaded[key] = true // Mark as in-progress
	state.blocksMu.Unlock()

	// Acquire semaphore slot (blocks if at parallel limit)
	state.sem <- struct{}{}
	state.inFlight.Add(1)

	go func() {
		defer func() {
			blockPool.Put(data) // Return buffer to pool
			<-state.sem
			state.inFlight.Done()
		}()

		// Yield to any pending downloads (download priority)
		m.waitForDownloads()

		blockKeyStr := FormatBlockKey(payloadID, chunkIdx, blockIdx)

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

// ============================================================================
// Flush API (Returns FlushResult)
// ============================================================================

// FlushAsync flushes all pending data synchronously.
// Despite the name, this is now synchronous - the "Async" refers to the
// NFS protocol semantics where COMMIT doesn't wait for disk sync.
// Called by NFS COMMIT.
func (m *TransferManager) FlushAsync(ctx context.Context, payloadID string) (*FlushResult, error) {
	if err := m.flushRemaining(ctx, payloadID); err != nil {
		return nil, err
	}
	return &FlushResult{Finalized: true}, nil
}

// Flush waits for all pending data to be uploaded (blocking).
// Used for SMB CLOSE which requires full durability before returning.
func (m *TransferManager) Flush(ctx context.Context, payloadID string) (*FlushResult, error) {
	// Wait for eager uploads
	if err := m.WaitForUploads(ctx, payloadID); err != nil {
		return nil, err
	}

	// Flush remaining partial blocks
	if err := m.flushRemaining(ctx, payloadID); err != nil {
		return nil, err
	}

	return &FlushResult{Finalized: true}, nil
}

// flushRemaining uploads any remaining dirty data to the block store.
func (m *TransferManager) flushRemaining(ctx context.Context, payloadID string) error {
	if !m.canProcess(ctx) {
		return fmt.Errorf("transfer manager is closed")
	}

	// Get all pending slices
	pending, err := m.cache.GetDirtySlices(ctx, payloadID)
	if err != nil {
		if err == cache.ErrFileNotInCache {
			return nil // No data to flush
		}
		return err
	}

	if len(pending) == 0 {
		return nil
	}

	// Upload slices in parallel
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

			blockRefs, err := m.uploadSliceAsBlocks(ctx, payloadID, s)
			if err != nil {
				errCh <- err
				return
			}

			// Mark slice as flushed (ErrSliceNotFound is OK - may have been evicted)
			if err := m.cache.MarkSliceFlushed(ctx, payloadID, s.ID, blockRefs); err != nil {
				if err != cache.ErrSliceNotFound {
					errCh <- err
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

	m.cleanupUploadState(payloadID)
	return nil
}

// uploadSliceAsBlocks splits a slice into blocks and uploads each.
func (m *TransferManager) uploadSliceAsBlocks(ctx context.Context, payloadID string, slice cache.PendingSlice) ([]cache.BlockRef, error) {
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

		blockKeyStr := FormatBlockKey(payloadID, slice.ChunkIndex, blockIdx)

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

// ReadSlice fetches blocks from the block store in parallel and caches them.
// Called when ReadAt() encounters a cache miss. Data is written directly into dest.
//
// Downloads have priority over uploads - when this function runs, any pending
// upload goroutines will pause until the download completes.
//
// Deprecated: Use EnsureAvailable instead, which provides better priority scheduling
// and prefetch support.
func (m *TransferManager) ReadSlice(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32, dest []byte) error {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return fmt.Errorf("transfer manager is closed")
	}
	m.mu.RUnlock()

	// Signal that a download is active (pauses uploads)
	m.ioCond.L.Lock()
	m.downloadsPending++
	m.ioCond.L.Unlock()

	defer func() {
		m.ioCond.L.Lock()
		m.downloadsPending--
		if m.downloadsPending == 0 {
			m.ioCond.Broadcast() // Wake up waiting uploads
		}
		m.ioCond.L.Unlock()
	}()

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

			blockKeyStr := FormatBlockKey(payloadID, chunkIdx, blockIdx)
			data, err := m.blockStore.ReadBlock(ctx, blockKeyStr)
			if err != nil {
				errCh <- fmt.Errorf("read block %s: %w", blockKeyStr, err)
				return
			}

			blocks[resultIdx] = data

			// Cache the downloaded block as flushed (evictable)
			// We create a new slice for this block
			blockOffset := blockIdx * BlockSize
			if err := m.cache.WriteSlice(ctx, payloadID, chunkIdx, data, blockOffset); err != nil {
				// Non-fatal: block was read successfully, just not cached
			}
		}(i, int(i-startBlockIdx))
	}

	wg.Wait()
	close(errCh)

	// Check for errors
	for err := range errCh {
		return err
	}

	// Assemble result from blocks directly into dest
	assembleBlocks(blocks, offset, length, startBlockIdx, dest)
	return nil
}

// assembleBlocks combines block data into the destination buffer.
func assembleBlocks(blocks [][]byte, offset, length, startBlockIdx uint32, dest []byte) {
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

		// Copy overlapping data directly into dest
		srcStart := overlapStart - blockStart
		srcEnd := overlapEnd - blockStart
		dstStart := overlapStart - offset

		copy(dest[dstStart:], blockData[srcStart:srcEnd])
	}
}

// ============================================================================
// Block-Level Operations (called by queue workers)
// ============================================================================

// downloadBlock downloads a single block from the block store and caches it.
// Called by queue workers for download and prefetch requests.
func (m *TransferManager) downloadBlock(ctx context.Context, payloadID string, chunkIdx, blockIdx uint32) error {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return fmt.Errorf("transfer manager is closed")
	}
	m.mu.RUnlock()

	blockKeyStr := FormatBlockKey(payloadID, chunkIdx, blockIdx)

	// Download from block store
	data, err := m.blockStore.ReadBlock(ctx, blockKeyStr)
	if err != nil {
		return fmt.Errorf("download block %s: %w", blockKeyStr, err)
	}

	// Write to cache as a flushed slice (evictable)
	// PayloadID is the sole identifier for file content
	blockOffset := blockIdx * BlockSize
	if err := m.cache.WriteSlice(ctx, payloadID, chunkIdx, data, blockOffset); err != nil {
		// Non-fatal: block was read successfully, just not cached
	}

	return nil
}

// uploadBlock uploads a single block from cache to block store.
// Called by queue workers for block-level upload requests (eager upload).
func (m *TransferManager) uploadBlock(ctx context.Context, payloadID string, chunkIdx, blockIdx uint32) error {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return fmt.Errorf("transfer manager is closed")
	}
	m.mu.RUnlock()

	// Read block data from cache
	blockOffset := blockIdx * BlockSize
	data := blockPool.Get().([]byte)
	defer blockPool.Put(data)

	found, err := m.cache.ReadSlice(ctx, payloadID, chunkIdx, blockOffset, BlockSize, data)
	if err != nil || !found {
		return fmt.Errorf("block not in cache: chunk=%d block=%d", chunkIdx, blockIdx)
	}

	// Upload to block store
	blockKeyStr := FormatBlockKey(payloadID, chunkIdx, blockIdx)
	if err := m.blockStore.WriteBlock(ctx, blockKeyStr, data); err != nil {
		return fmt.Errorf("upload block %s: %w", blockKeyStr, err)
	}

	return nil
}

// ============================================================================
// EnsureAvailable
// ============================================================================

// EnsureAvailable ensures the requested data range is in cache, downloading if needed.
// Blocks until data is available. Also triggers prefetch for upcoming blocks.
//
// This is the preferred method for handling cache misses - it uses the queue
// for downloads with proper priority scheduling and prefetch support.
func (m *TransferManager) EnsureAvailable(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32) error {
	if !m.canProcess(ctx) {
		return fmt.Errorf("transfer manager is closed")
	}

	// Check if range is already in cache
	if m.isRangeInCache(ctx, payloadID, chunkIdx, offset, length) {
		return nil
	}

	// Calculate which blocks we need
	startBlockIdx := offset / BlockSize
	endBlockIdx := (offset + length - 1) / BlockSize

	// Enqueue ALL requests at once: downloads + prefetch (parallel)
	var doneChannels []chan error

	// 1. Enqueue requested blocks (with Done channels to wait on)
	for blockIdx := startBlockIdx; blockIdx <= endBlockIdx; blockIdx++ {
		done := m.enqueueDownload(payloadID, chunkIdx, blockIdx)
		if done != nil {
			doneChannels = append(doneChannels, done)
		}
	}

	// 2. Enqueue prefetch blocks (no Done channel, fire-and-forget)
	//    This happens IN PARALLEL with the downloads above
	if m.config.PrefetchBlocks > 0 {
		blocksPerChunk := uint32(cache.ChunkSize / BlockSize)
		for i := 0; i < m.config.PrefetchBlocks; i++ {
			prefetchBlockIdx := endBlockIdx + 1 + uint32(i)
			// Calculate actual chunk/block for blocks that span chunk boundaries
			actualChunk := chunkIdx + prefetchBlockIdx/blocksPerChunk
			actualBlock := prefetchBlockIdx % blocksPerChunk
			m.enqueuePrefetch(payloadID, actualChunk, actualBlock)
		}
	}

	// 3. Wait for all requested blocks to complete
	for _, done := range doneChannels {
		select {
		case err := <-done:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

// enqueueDownload enqueues a download, handling in-flight deduplication.
// Returns channel to wait on, or nil if already in cache.
func (m *TransferManager) enqueueDownload(payloadID string, chunkIdx, blockIdx uint32) chan error {
	// Check cache first (fast path)
	if m.isBlockInCache(payloadID, chunkIdx, blockIdx) {
		return nil
	}

	key := FormatBlockKey(payloadID, chunkIdx, blockIdx)

	m.inFlightMu.Lock()
	defer m.inFlightMu.Unlock()

	// Check if already in-flight (wait on existing channel)
	if existing, ok := m.inFlight[key]; ok {
		// Create waiter that receives from existing download
		waiter := make(chan error, 1)
		go func() {
			waiter <- <-existing
		}()
		return waiter
	}

	// Create new completion channel and enqueue
	done := make(chan error, 1)
	m.inFlight[key] = done

	req := NewDownloadRequest(payloadID, chunkIdx, blockIdx, nil)
	req.Done = m.wrapDoneChannel(key, done)
	m.queue.EnqueueDownload(req)

	return done
}

// wrapDoneChannel creates a channel that cleans up in-flight tracking when signaled.
func (m *TransferManager) wrapDoneChannel(key string, original chan error) chan error {
	wrapped := make(chan error, 1)
	go func() {
		err := <-wrapped
		// Cleanup in-flight tracking
		m.inFlightMu.Lock()
		delete(m.inFlight, key)
		m.inFlightMu.Unlock()
		// Forward to original
		original <- err
		close(original)
	}()
	return wrapped
}

// enqueuePrefetch enqueues a prefetch request (non-blocking, best effort).
func (m *TransferManager) enqueuePrefetch(payloadID string, chunkIdx, blockIdx uint32) {
	// Skip if in cache
	if m.isBlockInCache(payloadID, chunkIdx, blockIdx) {
		return
	}

	// Skip if already in-flight
	key := FormatBlockKey(payloadID, chunkIdx, blockIdx)
	m.inFlightMu.Lock()
	if _, ok := m.inFlight[key]; ok {
		m.inFlightMu.Unlock()
		return
	}
	m.inFlightMu.Unlock()

	// Non-blocking enqueue (drop if full - prefetch is best effort)
	m.queue.EnqueuePrefetch(NewPrefetchRequest(payloadID, chunkIdx, blockIdx))
}

// isBlockInCache checks if a block is fully in cache.
func (m *TransferManager) isBlockInCache(payloadID string, chunkIdx, blockIdx uint32) bool {
	blockOffset := blockIdx * BlockSize
	covered, err := m.cache.IsRangeCovered(context.Background(), payloadID, chunkIdx, blockOffset, BlockSize)
	return err == nil && covered
}

// isRangeInCache checks if a range is fully in cache.
func (m *TransferManager) isRangeInCache(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32) bool {
	covered, err := m.cache.IsRangeCovered(ctx, payloadID, chunkIdx, offset, length)
	return err == nil && covered
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

	// List all blocks to find the highest chunk/block indices
	prefix := payloadID + "/"
	blocks, err := blockStore.ListByPrefix(ctx, prefix)
	if err != nil {
		return 0, fmt.Errorf("list blocks: %w", err)
	}

	if len(blocks) == 0 {
		return 0, nil
	}

	// Find the last block (highest chunk/block indices)
	var maxChunkIdx, maxBlockIdx uint32
	for _, blockKey := range blocks {
		var chunkIdx, blockIdx uint32
		if _, err := fmt.Sscanf(blockKey, payloadID+"/chunk-%d/block-%d", &chunkIdx, &blockIdx); err != nil {
			continue
		}
		if chunkIdx > maxChunkIdx || (chunkIdx == maxChunkIdx && blockIdx > maxBlockIdx) {
			maxChunkIdx = chunkIdx
			maxBlockIdx = blockIdx
		}
	}

	// Only read the last block to get its size (may be partial)
	lastBlockKey := FormatBlockKey(payloadID, maxChunkIdx, maxBlockIdx)
	lastBlockData, err := blockStore.ReadBlock(ctx, lastBlockKey)
	lastBlockSize := uint64(BlockSize)
	if err == nil {
		lastBlockSize = uint64(len(lastBlockData))
	}

	// Total = full chunks + full blocks in last chunk + last block size
	totalSize := uint64(maxChunkIdx)*uint64(chunk.Size) +
		uint64(maxBlockIdx)*uint64(BlockSize) +
		lastBlockSize

	return totalSize, nil
}

// Exists checks if any blocks exist for a file in the block store.
func (m *TransferManager) Exists(ctx context.Context, shareName, payloadID string) (bool, error) {
	if !m.canProcess(ctx) {
		return false, fmt.Errorf("transfer manager is closed")
	}

	if m.blockStore == nil {
		return false, fmt.Errorf("no block store configured")
	}

	// Check if the first block exists (fast path)
	firstBlockKey := FormatBlockKey(payloadID, 0, 0)
	_, err := m.blockStore.ReadBlock(ctx, firstBlockKey)
	if err == nil {
		return true, nil
	}
	if err == store.ErrBlockNotFound {
		return false, nil
	}
	return false, fmt.Errorf("check block: %w", err)
}

// Truncate removes blocks beyond the new size from the block store.
// Note: This deletes whole blocks only. Partial block truncation (e.g., truncating
// to middle of a block) is not supported - the last block retains its original size.
// Future optimization: Add TruncateBlock to BlockStore interface using S3 CopyObjectWithRange.
func (m *TransferManager) Truncate(ctx context.Context, shareName, payloadID string, newSize uint64) error {
	if !m.canProcess(ctx) {
		return fmt.Errorf("transfer manager is closed")
	}

	if m.blockStore == nil {
		return fmt.Errorf("no block store configured")
	}

	// Calculate which chunk/block the new size falls into
	newChunkIdx := chunk.IndexForOffset(newSize)
	offsetInChunk := chunk.OffsetInChunk(newSize)
	newBlockIdx := block.IndexForOffset(offsetInChunk)

	// List and delete blocks beyond the new size
	prefix := payloadID + "/"
	blocks, err := m.blockStore.ListByPrefix(ctx, prefix)
	if err != nil {
		return fmt.Errorf("list blocks: %w", err)
	}

	for _, blockKey := range blocks {
		var chunkIdx, blockIdx uint32
		if _, err := fmt.Sscanf(blockKey, payloadID+"/chunk-%d/block-%d", &chunkIdx, &blockIdx); err != nil {
			continue
		}
		if chunkIdx > newChunkIdx || (chunkIdx == newChunkIdx && blockIdx > newBlockIdx) {
			if err := m.blockStore.DeleteBlock(ctx, blockKey); err != nil {
				return fmt.Errorf("delete block %s: %w", blockKey, err)
			}
		}
	}

	return nil
}

// Delete removes all blocks for a file from the block store.
func (m *TransferManager) Delete(ctx context.Context, shareName, payloadID string) error {
	if !m.canProcess(ctx) {
		return fmt.Errorf("transfer manager is closed")
	}

	if m.blockStore == nil {
		return fmt.Errorf("no block store configured")
	}

	return m.blockStore.DeleteByPrefix(ctx, payloadID+"/")
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
