package offloader

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/cache"
)

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
func (m *Offloader) OnWriteComplete(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32) {
	if !m.canProcess(ctx) {
		return
	}

	startBlock, endBlock := blockRange(offset, length)
	for blockIdx := startBlock; blockIdx <= endBlock; blockIdx++ {
		m.tryEagerUpload(ctx, payloadID, chunkIdx, blockIdx)
	}
}

// tryEagerUpload checks if a block is complete and starts an async upload if ready.
// Only complete 4MB blocks are uploaded; partial blocks are flushed during Flush().
//
// PERFORMANCE: This function is called in the NFS WRITE path. It must return quickly
// to avoid blocking writes. Hash computation and dedup checks are done asynchronously
// in the upload goroutine to minimize write latency.
func (m *Offloader) tryEagerUpload(ctx context.Context, payloadID string, chunkIdx, blockIdx uint32) {
	blockStart := blockIdx * BlockSize
	blockEnd := blockStart + BlockSize

	// Skip blocks that extend beyond chunk boundary
	if blockEnd > cache.ChunkSize {
		return
	}

	// Check if fully covered (no zero-filled gaps) - fast bitmap check
	covered, err := m.cache.IsRangeCovered(ctx, payloadID, chunkIdx, blockStart, BlockSize)
	if err != nil || !covered {
		return
	}

	logger.Debug("Eager upload triggered",
		"payloadID", payloadID,
		"chunkIdx", chunkIdx,
		"blockIdx", blockIdx)

	// Read block data from cache into pooled buffer
	dataPtr := blockPool.Get().(*[]byte)
	data := *dataPtr
	found, err := m.cache.ReadAt(ctx, payloadID, chunkIdx, blockStart, BlockSize, data)
	if err != nil || !found {
		blockPool.Put(dataPtr)
		return
	}

	// Start async upload (takes ownership of data buffer pointer)
	// Hash computation and dedup checks happen in the background goroutine
	// to avoid blocking the NFS WRITE path
	m.startBlockUpload(ctx, payloadID, chunkIdx, blockIdx, dataPtr)
}

// startBlockUpload uploads a block asynchronously with bounded parallelism.
//
// The dataPtr buffer pointer is owned by this function and will be returned to blockPool
// after the upload completes or fails.
//
// Upload goroutines yield to downloads (download priority) before performing I/O.
//
// PERFORMANCE: Hash computation and dedup checks happen inside the goroutine
// to avoid blocking the NFS WRITE path. This moves ~15ms of SHA-256 computation
// off the critical path for each 4MB block.
//
// If ObjectStore is configured, content-addressed deduplication is performed:
// 1. Compute SHA-256 hash of block data (async)
// 2. Check if block with same hash already exists
// 3. If exists: increment RefCount, skip upload
// 4. If not: upload and register block
func (m *Offloader) startBlockUpload(ctx context.Context,
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

		// Compute hash for deduplication (done in background to not block writes)
		hash := sha256.Sum256(data)

		// Content-addressed deduplication: check if block already exists
		if m.objectStore != nil {
			existing, err := m.objectStore.FindBlockByHash(ctx, hash)
			if err == nil && existing != nil && existing.IsUploaded() {
				// Block already exists and is uploaded - increment RefCount and skip upload
				_, _ = m.objectStore.IncrementBlockRefCount(ctx, hash)
				m.cache.MarkBlockReadyForUpload(ctx, payloadID, chunkIdx, blockIdx, hash, nil)
				m.cache.MarkBlockUploaded(ctx, payloadID, chunkIdx, blockIdx)

				logger.Debug("Dedup: block already exists, skipping upload",
					"payloadID", payloadID,
					"chunkIdx", chunkIdx,
					"blockIdx", blockIdx,
					"hash", hashB64(hash))
				return
			}
		}

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

		// Handle successful upload (ObjectStore, hash tracking, cache marking)
		m.handleUploadSuccess(ctx, payloadID, chunkIdx, blockIdx, hash, uint32(len(data)))

		logger.Debug("Eager upload complete",
			"payloadID", payloadID,
			"blockKey", blockKeyStr,
			"hash", hashB64(hash),
			"duration", time.Since(startTime),
			"size", len(data))
	}()
}

// uploadRemainingBlocks uploads dirty blocks to the block store in parallel.
// This handles blocks that weren't eagerly uploaded (partial blocks or when semaphore was full).
func (m *Offloader) uploadRemainingBlocks(ctx context.Context, payloadID string) error {
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

		// Use existing hash from ReadyForUpload state, or compute it
		hash := blk.Hash
		if hash == [32]byte{} {
			blockData, dataSize, err := m.cache.GetBlockData(ctx, payloadID, chunkIdx, blockIdx)
			if err != nil {
				continue
			}
			hash = sha256.Sum256(blockData[:dataSize])
		}

		// Content-addressed deduplication: check if block already exists
		if m.objectStore != nil {
			existing, err := m.objectStore.FindBlockByHash(ctx, hash)
			if err == nil && existing != nil && existing.IsUploaded() {
				// Block already exists - increment RefCount and skip upload
				_, _ = m.objectStore.IncrementBlockRefCount(ctx, hash)
				m.cache.MarkBlockUploaded(ctx, payloadID, chunkIdx, blockIdx)

				logger.Debug("Flush dedup: block already exists, skipping upload",
					"payloadID", payloadID,
					"chunkIdx", chunkIdx,
					"blockIdx", blockIdx,
					"hash", hashB64(hash))
				continue
			}
		}

		// Mark the block as Uploading to prevent eviction during upload.
		// Unlike DetachBlockForUpload, we keep the data in cache so concurrent
		// reads can still access it. We copy the data for the upload goroutine.
		//
		// DetachBlockForUpload was previously used here for zero-copy performance,
		// but it caused data corruption: when Flush runs during active writes
		// (e.g., COMMIT during a 100MB write), detaching partial blocks removes
		// data from cache. Subsequent writes re-allocate the block buffer with
		// a fresh coverage bitmap, losing the previously-written data. Reads then
		// return zeros for the lost region instead of the actual data.
		if !m.cache.MarkBlockUploading(ctx, payloadID, chunkIdx, blockIdx) {
			logger.Debug("Flush: block already being uploaded or not found, skipping",
				"payloadID", payloadID,
				"chunkIdx", chunkIdx,
				"blockIdx", blockIdx)
			continue
		}

		// Copy block data for upload (data stays in cache for concurrent reads)
		blockData, dataSize, err := m.cache.GetBlockData(ctx, payloadID, chunkIdx, blockIdx)
		if err != nil {
			// Revert to Pending if we can't get the data
			m.cache.MarkBlockPending(ctx, payloadID, chunkIdx, blockIdx)
			continue
		}
		uploadData := make([]byte, dataSize)
		copy(uploadData, blockData[:dataSize])

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

		go func(uploadData []byte, dataSize, chunkIdx, blockIdx uint32, hash [32]byte) {
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

			if err := m.blockStore.WriteBlock(ctx, blockKeyStr, uploadData); err != nil {
				logger.Error("Flush upload failed",
					"payloadID", payloadID,
					"blockKey", blockKeyStr,
					"duration", time.Since(startTime),
					"error", err)
				// Revert block to Pending so it can be retried
				m.cache.MarkBlockPending(ctx, payloadID, chunkIdx, blockIdx)
				if state != nil {
					key := blockKey{chunkIdx: chunkIdx, blockIdx: blockIdx}
					state.blocksMu.Lock()
					state.uploaded[key] = false
					state.blocksMu.Unlock()
				}
				return
			}

			// Handle successful upload (ObjectStore, hash tracking, cache marking)
			// Data stays in cache for reads, evictable by LRU when marked Uploaded.
			m.handleUploadSuccess(ctx, payloadID, chunkIdx, blockIdx, hash, dataSize)

			logger.Info("Flush upload complete",
				"payloadID", payloadID,
				"blockKey", blockKeyStr,
				"hash", hashB64(hash),
				"duration", time.Since(startTime),
				"size", dataSize)
		}(uploadData, dataSize, chunkIdx, blockIdx, hash)
	}

	wg.Wait()
	return nil
}

// uploadBlock uploads a single block from cache to block store.
// Called by queue workers for block-level upload requests (eager upload).
func (m *Offloader) uploadBlock(ctx context.Context, payloadID string, chunkIdx, blockIdx uint32) error {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return fmt.Errorf("offloader is closed")
	}
	m.mu.RUnlock()

	// Read block data from cache
	blockOffset := blockIdx * BlockSize
	dataPtr := blockPool.Get().(*[]byte)
	defer blockPool.Put(dataPtr)
	data := *dataPtr

	found, err := m.cache.ReadAt(ctx, payloadID, chunkIdx, blockOffset, BlockSize, data)
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
