package transfer

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/payload/block"
	"github.com/marmos91/dittofs/pkg/payload/chunk"
	"github.com/marmos91/dittofs/pkg/payload/store"
)

// blockPool reuses 4MB buffers for block uploads to reduce GC pressure.
// Uses *[]byte to satisfy staticcheck SA6002 (sync.Pool prefers pointer types).
var blockPool = sync.Pool{
	New: func() any {
		buf := make([]byte, BlockSize)
		return &buf
	},
}

// fileUploadState tracks in-flight uploads for a single file.
type fileUploadState struct {
	inFlight sync.WaitGroup    // Tracks in-flight eager uploads
	flush    sync.WaitGroup    // Tracks in-flight flush operations
	errors   []error           // Accumulated errors
	errorsMu sync.Mutex        // Protects errors
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
//   - Configurable parallelism: Set max concurrent uploads via config
//   - In-flight deduplication: Avoids duplicate downloads for the same block
//   - Non-blocking: All operations return immediately, I/O happens in background
type TransferManager struct {
	cache      *cache.Cache
	blockStore store.BlockStore
	config     Config

	// Per-file upload tracking
	uploads   map[string]*fileUploadState // payloadID -> state
	uploadsMu sync.Mutex

	// Global upload semaphore - limits total concurrent uploads
	uploadSem chan struct{}

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

	// Calculate semaphore size - use MaxParallelUploads if set, otherwise ParallelUploads
	semSize := config.ParallelUploads
	if config.MaxParallelUploads > 0 {
		semSize = config.MaxParallelUploads
	}
	if semSize < 1 {
		semSize = DefaultParallelUploads
	}

	m := &TransferManager{
		cache:      c,
		blockStore: store,
		config:     config,
		uploads:    make(map[string]*fileUploadState),
		ioCond:     sync.NewCond(&sync.Mutex{}),
		inFlight:   make(map[string]chan error),
		uploadSem:  make(chan struct{}, semSize),
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

	logger.Debug("Eager upload triggered",
		"payloadID", payloadID,
		"chunkIdx", chunkIdx,
		"blockIdx", blockIdx)

	// Read block data from cache
	dataPtr := blockPool.Get().(*[]byte)
	data := *dataPtr
	found, err := m.cache.Read(ctx, payloadID, chunkIdx, blockStart, BlockSize, data)
	if err != nil || !found {
		blockPool.Put(dataPtr)
		return
	}

	// Start async upload (takes ownership of data buffer pointer)
	m.startBlockUpload(ctx, payloadID, chunkIdx, blockIdx, dataPtr)
}

// startBlockUpload uploads a block asynchronously with bounded parallelism.
//
// The dataPtr buffer pointer is owned by this function and will be returned to blockPool
// after the upload completes or fails.
//
// Upload goroutines yield to downloads (download priority) before performing I/O.
func (m *TransferManager) startBlockUpload(ctx context.Context,
	payloadID string, chunkIdx, blockIdx uint32, dataPtr *[]byte) {
	state := m.getOrCreateUploadState(payloadID)

	// Check if already uploaded (deduplication)
	key := blockKey{chunkIdx: chunkIdx, blockIdx: blockIdx}
	state.blocksMu.Lock()
	if state.uploaded[key] {
		state.blocksMu.Unlock()
		blockPool.Put(dataPtr) // Return unused buffer
		return
	}
	state.uploaded[key] = true // Mark as in-progress
	state.blocksMu.Unlock()

	// Try to acquire semaphore slot (non-blocking)
	// If all slots are taken, skip eager upload - block will be uploaded during Flush
	select {
	case m.uploadSem <- struct{}{}:
		// Got slot, proceed with upload
	default:
		// All slots taken, skip eager upload
		state.blocksMu.Lock()
		state.uploaded[key] = false // Unmark so Flush will upload it
		state.blocksMu.Unlock()
		blockPool.Put(dataPtr)
		return
	}
	state.inFlight.Add(1)

	data := *dataPtr
	go func() {
		defer func() {
			blockPool.Put(dataPtr) // Return buffer to pool
			<-m.uploadSem          // Release semaphore slot
			state.inFlight.Done()
		}()

		// Yield to any pending downloads (download priority)
		m.waitForDownloads()

		blockKeyStr := FormatBlockKey(payloadID, chunkIdx, blockIdx)
		startTime := time.Now()

		logger.Debug("Eager upload starting",
			"payloadID", payloadID,
			"blockKey", blockKeyStr,
			"activeUploads", len(m.uploadSem),
			"maxUploads", cap(m.uploadSem))

		if err := m.blockStore.WriteBlock(ctx, blockKeyStr, data); err != nil {
			logger.Error("Eager upload failed",
				"payloadID", payloadID,
				"blockKey", blockKeyStr,
				"duration", time.Since(startTime),
				"error", err)

			state.errorsMu.Lock()
			state.errors = append(state.errors, fmt.Errorf("upload block %s: %w", blockKeyStr, err))
			state.errorsMu.Unlock()

			// Mark as not uploaded so it can be retried
			state.blocksMu.Lock()
			state.uploaded[key] = false
			state.blocksMu.Unlock()
			return
		}

		// Mark block as uploaded so it can be evicted.
		// This is critical for memory management during large file writes.
		m.cache.MarkBlockUploaded(ctx, payloadID, chunkIdx, blockIdx)

		logger.Debug("Eager upload complete",
			"payloadID", payloadID,
			"blockKey", blockKeyStr,
			"duration", time.Since(startTime),
			"size", len(data))
	}()
}

// ============================================================================
// Flush API (Returns FlushResult)
// ============================================================================

// Flush enqueues remaining dirty data for background upload and returns immediately.
//
// This method does NOT wait for S3 uploads to complete because:
// 1. Data is already safe in WAL-backed mmap cache (crash-safe via OS page cache)
// 2. Eager upload handles complete 4MB blocks asynchronously
// 3. Remaining partial blocks are enqueued for background upload
//
// Both NFS COMMIT and SMB CLOSE use this method. NFS/SMB semantics only require
// data to be durable on stable storage - the mmap WAL provides this guarantee.
//
// Deduplication: Blocks already uploaded by eager upload are tracked in state.uploaded
// and skipped by uploadRemainingBlocks. No need to wait for eager uploads to complete.
func (m *TransferManager) Flush(ctx context.Context, payloadID string) (*FlushResult, error) {
	if !m.canProcess(ctx) {
		return nil, fmt.Errorf("transfer manager is closed")
	}

	// Get or create upload state for tracking
	state := m.getOrCreateUploadState(payloadID)
	state.flush.Add(1)

	// Upload remaining dirty blocks (partial blocks not covered by eager upload)
	// in background. No blocking - data is safe in mmap cache.
	//
	// IMPORTANT: We use context.Background() here because the request context is
	// cancelled when COMMIT returns. The background upload should continue regardless.
	//
	// Server shutdown is handled separately by TransferManager.Close() which:
	// 1. Stops accepting new work via canProcess() check
	// 2. Drains the transfer queue with a timeout
	// 3. uploadRemainingBlocks checks canProcess() before each block upload
	//
	// Data durability is guaranteed by the mmap WAL cache - uploads are best-effort
	// for performance, not required for durability.
	go func() {
		defer state.flush.Done()
		if err := m.uploadRemainingBlocks(context.Background(), payloadID); err != nil {
			logger.Warn("Failed to upload remaining blocks",
				"payloadID", payloadID,
				"error", err)
		}
	}()

	return &FlushResult{Finalized: true}, nil
}

// WaitForEagerUploads waits for in-flight eager uploads to complete.
// This is useful in tests to ensure uploads complete before checking results.
func (m *TransferManager) WaitForEagerUploads(ctx context.Context, payloadID string) error {
	state := m.getUploadState(payloadID)
	if state == nil {
		return nil
	}

	done := make(chan struct{})
	go func() {
		state.inFlight.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WaitForAllUploads waits for both eager uploads AND flush operations to complete.
// FOR TESTING ONLY - this method is used in integration tests to verify data was uploaded
// before checking block store contents. Production code should NOT call this method;
// production uses non-blocking Flush() which returns immediately (data safety is
// guaranteed by the WAL-backed mmap cache).
func (m *TransferManager) WaitForAllUploads(ctx context.Context, payloadID string) error {
	state := m.getUploadState(payloadID)
	if state == nil {
		return nil
	}

	done := make(chan struct{})
	go func() {
		state.inFlight.Wait() // Wait for eager uploads
		state.flush.Wait()    // Wait for flush operations
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// uploadRemainingBlocks uploads dirty blocks to the block store in parallel.
// This handles blocks that weren't eagerly uploaded (partial blocks or when semaphore was full).
func (m *TransferManager) uploadRemainingBlocks(ctx context.Context, payloadID string) error {
	// Get all pending blocks that need uploading
	pending, err := m.cache.GetDirtyBlocks(ctx, payloadID)
	if err != nil {
		if err == cache.ErrFileNotInCache {
			return nil // No data to flush
		}
		return err
	}

	if len(pending) == 0 {
		return nil
	}

	// Get upload state for deduplication
	state := m.getUploadState(payloadID)

	// Filter out blocks already uploaded by eager upload
	var blocksToUpload []cache.PendingBlock
	for _, blk := range pending {
		// Check if already uploaded by eager upload
		if state != nil {
			key := blockKey{chunkIdx: blk.ChunkIndex, blockIdx: blk.BlockIndex}
			state.blocksMu.Lock()
			alreadyUploaded := state.uploaded[key]
			state.blocksMu.Unlock()
			if alreadyUploaded {
				// Mark as uploaded in cache since eager upload succeeded
				m.cache.MarkBlockUploaded(ctx, payloadID, blk.ChunkIndex, blk.BlockIndex)
				continue
			}
		}
		blocksToUpload = append(blocksToUpload, blk)
	}

	if len(blocksToUpload) == 0 {
		logger.Info("Flush: all blocks already uploaded",
			"payloadID", payloadID,
			"blocks", len(pending))
		return nil
	}

	logger.Info("Flush: uploading remaining blocks",
		"payloadID", payloadID,
		"blocksToUpload", len(blocksToUpload),
		"activeUploads", len(m.uploadSem),
		"maxUploads", cap(m.uploadSem))

	// Upload all blocks in parallel using semaphore
	var wg sync.WaitGroup

	for _, blk := range blocksToUpload {
		chunkIdx := blk.ChunkIndex
		blockIdx := blk.BlockIndex

		// Atomically claim and detach the block buffer for upload.
		// This is a zero-copy operation that transfers ownership of the buffer
		// to the upload goroutine, preventing data corruption from concurrent writes.
		blockData, dataSize, ok := m.cache.DetachBlockForUpload(ctx, payloadID, chunkIdx, blockIdx)
		if !ok {
			logger.Debug("Flush: block already being uploaded or not found, skipping",
				"payloadID", payloadID,
				"chunkIdx", chunkIdx,
				"blockIdx", blockIdx)
			continue
		}

		// Also mark in state.uploaded to prevent future flushes from trying
		if state != nil {
			key := blockKey{chunkIdx: chunkIdx, blockIdx: blockIdx}
			state.blocksMu.Lock()
			state.uploaded[key] = true
			state.blocksMu.Unlock()
		}

		wg.Add(1)

		// Acquire semaphore slot (blocking for flush)
		m.uploadSem <- struct{}{}

		go func(blockData []byte, dataSize, chunkIdx, blockIdx uint32) {
			defer func() {
				<-m.uploadSem // Release semaphore slot
				wg.Done()
			}()

			blockKeyStr := FormatBlockKey(payloadID, chunkIdx, blockIdx)
			startTime := time.Now()

			logger.Debug("Flush upload starting",
				"payloadID", payloadID,
				"blockKey", blockKeyStr,
				"size", dataSize,
				"activeUploads", len(m.uploadSem),
				"maxUploads", cap(m.uploadSem))

			if err := m.blockStore.WriteBlock(ctx, blockKeyStr, blockData[:dataSize]); err != nil {
				logger.Error("Flush upload failed",
					"payloadID", payloadID,
					"blockKey", blockKeyStr,
					"duration", time.Since(startTime),
					"error", err)
				// Restore buffer to cache so it can be retried
				m.cache.RestoreBlockBuffer(ctx, payloadID, chunkIdx, blockIdx, blockData)
				if state != nil {
					key := blockKey{chunkIdx: chunkIdx, blockIdx: blockIdx}
					state.blocksMu.Lock()
					state.uploaded[key] = false
					state.blocksMu.Unlock()
				}
				return
			}

			// Mark block as uploaded. Buffer is discarded - data is now in S3.
			// If needed later, it can be downloaded from S3.
			m.cache.MarkBlockUploaded(ctx, payloadID, chunkIdx, blockIdx)

			logger.Info("Flush upload complete",
				"payloadID", payloadID,
				"blockKey", blockKeyStr,
				"duration", time.Since(startTime),
				"size", dataSize)
		}(blockData, dataSize, chunkIdx, blockIdx)
	}

	wg.Wait()
	return nil
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

	// Write to cache using WriteDownloaded which:
	// - Marks block as Uploaded (evictable) since it's already in S3
	// - Does NOT count against pendingSize
	// - Does NOT write to WAL
	// This allows cache to evict downloaded data under pressure
	blockOffset := blockIdx * BlockSize
	if err := m.cache.WriteDownloaded(ctx, payloadID, chunkIdx, data, blockOffset); err != nil {
		return fmt.Errorf("cache downloaded block %s: %w", blockKeyStr, err)
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
	dataPtr := blockPool.Get().(*[]byte)
	defer blockPool.Put(dataPtr)
	data := *dataPtr

	found, err := m.cache.Read(ctx, payloadID, chunkIdx, blockOffset, BlockSize, data)
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
func (m *TransferManager) GetFileSize(ctx context.Context, _ /* shareName */, payloadID string) (uint64, error) {
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
func (m *TransferManager) Exists(ctx context.Context, _ /* shareName */, payloadID string) (bool, error) {
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
func (m *TransferManager) Truncate(ctx context.Context, _ /* shareName */, payloadID string, newSize uint64) error {
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
func (m *TransferManager) Delete(ctx context.Context, _ /* shareName */, payloadID string) error {
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
