package cache

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/cache/wal"
	"github.com/marmos91/dittofs/pkg/payload/block"
)

// WaitForPendingDrain blocks until pendingSize decreases or the deadline expires.
// Returns true if a drain occurred (pendingSize decreased), false on timeout or context cancellation.
func (c *Cache) WaitForPendingDrain(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	// Early exit if already expired or cancelled.
	if ctx.Err() != nil || !time.Now().Before(deadline) {
		return false
	}

	done := make(chan struct{})

	// Bridge context cancellation to Cond.Broadcast so Wait unblocks.
	go func() {
		select {
		case <-ctx.Done():
			c.pendingCond.L.Lock()
			c.pendingCond.Broadcast()
			c.pendingCond.L.Unlock()
		case <-done:
		}
	}()

	c.pendingCond.L.Lock()

	// Snapshot pending size so we can detect when it actually decreases.
	initialPending := c.pendingSize.Load()

	timer := time.AfterFunc(time.Until(deadline), func() {
		c.pendingCond.L.Lock()
		c.pendingCond.Broadcast()
		c.pendingCond.L.Unlock()
	})

	// Loop to handle spurious wakeups — only exit when pending size decreases
	// or we hit the deadline / context cancellation.
	for ctx.Err() == nil && time.Now().Before(deadline) && c.pendingSize.Load() >= initialPending {
		c.pendingCond.Wait()
	}

	drained := c.pendingSize.Load() < initialPending
	c.pendingCond.L.Unlock()

	timer.Stop()
	close(done)

	return drained && ctx.Err() == nil && time.Now().Before(deadline)
}

// ============================================================================
// Write Operations
// ============================================================================

// writeOptions controls the behavior of writeAtInternal.
type writeOptions struct {
	// isDownloaded indicates data is from block store (already persisted).
	// When true:
	//   - Skip pending size backpressure check
	//   - New blocks start as Uploaded (evictable)
	//   - Skip WAL write (data already safe in block store)
	//   - Preserve existing block state (don't re-dirty)
	isDownloaded bool
}

// WriteAt writes data to the cache at the specified chunk and offset.
//
// This is the primary write path for all file data. Data is written directly
// into 4MB block buffers at the correct position. The coverage bitmap tracks
// which bytes have been written for sparse file support.
//
// Block Buffer Model:
// Data is written directly to the target position in the block buffer.
// Overlapping writes simply overwrite previous data (newest-wins semantics).
//
// Memory Tracking:
// The cache tracks actual memory allocation (BlockSize per block buffer), not
// just bytes written. This ensures accurate backpressure for OOM prevention.
//
// Parameters:
//   - payloadID: Unique identifier for the file content
//   - chunkIdx: Which 64MB chunk this write belongs to
//   - data: The bytes to write (copied into cache, safe to modify after call)
//   - offset: Byte offset within the chunk (0 to ChunkSize-1)
//
// Errors:
//   - ErrInvalidOffset: offset + len(data) exceeds ChunkSize
//   - ErrCacheClosed: cache has been closed
//   - ErrCacheFull: cache is full of pending data that cannot be evicted
//   - context.Canceled/DeadlineExceeded: context was cancelled
func (c *Cache) WriteAt(ctx context.Context, payloadID string, chunkIdx uint32, data []byte, offset uint32) error {
	return c.writeAtInternal(ctx, payloadID, chunkIdx, data, offset, writeOptions{isDownloaded: false})
}

// WriteDownloaded writes data that was downloaded from the block store.
//
// Unlike WriteAt(), this method:
//   - Marks blocks as Uploaded (evictable), not Pending
//   - Does NOT count against pendingSize (it's not dirty data)
//   - Does NOT write to WAL (data is already safe in block store)
//   - CAN evict other uploaded blocks to make room (downloaded data is evictable)
//
// This is used by the TransferManager when downloading blocks from S3 on cache miss.
// Downloaded data is already persisted in the block store, so it can be evicted
// immediately if cache pressure requires it.
//
// Parameters:
//   - payloadID: Unique identifier for the file content
//   - chunkIdx: Which 64MB chunk this write belongs to
//   - data: The bytes to write (copied into cache, safe to modify after call)
//   - offset: Byte offset within the chunk (0 to ChunkSize-1)
//
// Errors:
//   - ErrInvalidOffset: offset + len(data) exceeds ChunkSize
//   - ErrCacheClosed: cache has been closed
//   - ErrCacheFull: cache is completely full even after eviction
//   - context.Canceled/DeadlineExceeded: context was cancelled
func (c *Cache) WriteDownloaded(ctx context.Context, payloadID string, chunkIdx uint32, data []byte, offset uint32) error {
	return c.writeAtInternal(ctx, payloadID, chunkIdx, data, offset, writeOptions{isDownloaded: true})
}

// writeAtInternal is the shared implementation for WriteAt and WriteDownloaded.
func (c *Cache) writeAtInternal(ctx context.Context, payloadID string, chunkIdx uint32, data []byte, offset uint32, opts writeOptions) error {
	if err := c.checkClosed(ctx); err != nil {
		return err
	}

	// Fast path: nothing to write
	if len(data) == 0 {
		return nil
	}

	// Validate parameters
	if offset+uint32(len(data)) > ChunkSize {
		return ErrInvalidOffset
	}

	// Calculate which blocks this write spans
	startBlock := block.IndexForOffset(offset)
	endBlock := block.IndexForOffset(offset + uint32(len(data)) - 1)

	entry := c.getFileEntry(payloadID)
	entry.mu.Lock()

	// Calculate how many NEW blocks will be created (for memory tracking).
	newMemory := c.countNewBlockMemory(entry, chunkIdx, startBlock, endBlock)

	// Enforce maxSize by evicting LRU uploaded blocks if needed.
	if c.maxSize > 0 && newMemory > 0 {
		if c.totalSize.Load()+newMemory > c.maxSize {
			// Release lock to evict (eviction needs to lock other entries)
			entry.mu.Unlock()
			c.evictLRUUntilFits(ctx, newMemory)
			entry.mu.Lock()

			// Re-check after eviction (someone else might have created the blocks)
			newMemory = c.countNewBlockMemory(entry, chunkIdx, startBlock, endBlock)

			// Check if we have enough space after eviction.
			if c.totalSize.Load()+newMemory > c.maxSize {
				entry.mu.Unlock()
				return ErrCacheFull
			}
		}
	}

	// Backpressure on pending data (applies even when maxSize=0).
	// This prevents OOM when uploads can't keep up with writes.
	// Skip for downloaded data - it's already in block store, not pending.
	//
	// When pending data exceeds the limit, the write BLOCKS until the offloader
	// drains enough data (signaled via pendingCond). This provides proper
	// backpressure for slow backends like S3, instead of returning ErrCacheFull.
	if !opts.isDownloaded {
		maxPending := c.maxPendingSize
		if maxPending == 0 {
			maxPending = DefaultMaxPendingSize
		}
		if newMemory > 0 && c.pendingSize.Load()+newMemory > maxPending {
			// Release file entry lock before blocking - other files can proceed
			entry.mu.Unlock()

			if err := c.waitForPendingDrain(ctx, newMemory, maxPending); err != nil {
				return err
			}

			// Re-acquire file entry lock after waking up
			entry.mu.Lock()
		}
	}

	defer entry.mu.Unlock()

	// Update LRU access time
	c.touchFile(entry)

	// Write data to each block buffer it spans
	for blockIdx := startBlock; blockIdx <= endBlock; blockIdx++ {
		if err := c.writeToBlock(ctx, entry, payloadID, chunkIdx, blockIdx, data, offset, opts); err != nil {
			return err
		}
	}

	return nil
}

// waitForPendingDrain blocks until pendingSize drops below the threshold or
// the context/cache is cancelled. This is the core backpressure mechanism
// that prevents ErrCacheFull by waiting for the offloader to drain data.
//
// The method uses sync.Cond for efficient signaling: writers sleep here and
// are woken up by MarkBlockUploaded when pending data decreases.
//
// Returns nil when there is room to write, or an error if:
//   - BackpressureTimeout is exceeded (returns ErrCacheFull)
//   - Context is cancelled (returns context error)
//   - Cache is closed (returns ErrCacheClosed)
func (c *Cache) waitForPendingDrain(ctx context.Context, newMemory, maxPending uint64) error {
	deadline := time.Now().Add(BackpressureTimeout)
	logged := false

	c.pendingCond.L.Lock()
	defer c.pendingCond.L.Unlock()

	for {
		// Check if there's room now
		if c.pendingSize.Load()+newMemory <= maxPending {
			return nil
		}

		// Check context cancellation
		if err := ctx.Err(); err != nil {
			return err
		}

		// Check if cache is closed
		if err := c.checkClosed(ctx); err != nil {
			return err
		}

		// Check timeout
		if time.Now().After(deadline) {
			logger.Error("Backpressure timeout exceeded, cache still full",
				"pendingSize", c.pendingSize.Load(),
				"maxPending", maxPending,
				"timeout", BackpressureTimeout)
			return ErrCacheFull
		}

		if !logged {
			logger.Warn("Write blocked by cache backpressure, waiting for offloader to drain",
				"pendingSize", c.pendingSize.Load(),
				"maxPending", maxPending,
				"newMemory", newMemory)
			logged = true
		}

		// Wait with a periodic wake-up to check context/timeout.
		// We use a goroutine to wake us up after 500ms so we can re-check
		// conditions even if no signal arrives. This prevents indefinite
		// blocking when the offloader is making slow progress.
		waitDone := make(chan struct{})
		go func() {
			timer := time.NewTimer(500 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
				c.pendingCond.Broadcast()
			case <-waitDone:
			}
		}()

		c.pendingCond.Wait()
		close(waitDone)
	}
}

// countNewBlockMemory calculates memory needed for new blocks in the given range.
func (c *Cache) countNewBlockMemory(entry *fileEntry, chunkIdx, startBlock, endBlock uint32) uint64 {
	var newBlockCount uint64
	for blockIdx := startBlock; blockIdx <= endBlock; blockIdx++ {
		if !c.blockExists(entry, chunkIdx, blockIdx) {
			newBlockCount++
		}
	}
	return newBlockCount * BlockSize
}

// writeToBlock writes data to a single block buffer.
// Caller must hold entry.mu.Lock().
func (c *Cache) writeToBlock(ctx context.Context, entry *fileEntry, payloadID string, chunkIdx, blockIdx uint32, data []byte, offset uint32, opts writeOptions) error {
	// Get or create block buffer
	blk, isNew := c.getOrCreateBlock(entry, chunkIdx, blockIdx)

	// For normal writes (not downloaded), handle state transitions
	if !opts.isDownloaded {
		// Write to a block that is currently being uploaded.
		// Since flush upload uses a copy of the data (not detach), it's safe to
		// write new data here. The in-flight upload will complete with the old data,
		// and this block will need to be re-uploaded with the new data.
		// We revert to Pending so the block will be picked up by the next flush.
		if !isNew && blk.state == BlockStateUploading {
			blk.state = BlockStatePending
			blk.hash = [32]byte{} // Invalidate hash - data has changed
		}

		// Handle write to ReadyForUpload block - cancel pending upload and revert to Pending.
		if !isNew && blk.state == BlockStateReadyForUpload {
			if blk.uploadCancel != nil {
				blk.uploadCancel()
				blk.uploadCancel = nil
			}
			blk.state = BlockStatePending
			blk.hash = [32]byte{} // Invalidate hash - will be recomputed on completion
		}
	}

	// Track memory and set initial state for new block buffers
	if isNew {
		c.totalSize.Add(BlockSize)
		if opts.isDownloaded {
			// Downloaded blocks start as Uploaded (evictable)
			blk.state = BlockStateUploaded
		} else {
			// Normal writes start as Pending
			c.pendingSize.Add(BlockSize)
		}
	}

	// Calculate offsets within this block
	blockStart := blockIdx * BlockSize
	blockEnd := blockStart + BlockSize

	// Calculate overlap with write range
	writeStart := max(offset, blockStart)
	writeEnd := min(offset+uint32(len(data)), blockEnd)

	// Calculate positions
	offsetInBlock := writeStart - blockStart
	dataStart := writeStart - offset
	dataEnd := writeEnd - offset
	writeLen := dataEnd - dataStart

	// Copy data directly to block buffer
	copy(blk.data[offsetInBlock:], data[dataStart:dataEnd])

	// Update coverage bitmap
	markCoverage(blk.coverage, offsetInBlock, writeLen)

	// Update block data size
	if end := offsetInBlock + writeLen; end > blk.dataSize {
		blk.dataSize = end
	}

	// For normal writes, mark block as dirty if it was uploaded (re-dirty)
	if !opts.isDownloaded && blk.state == BlockStateUploaded {
		blk.state = BlockStatePending
		c.pendingSize.Add(BlockSize) // Re-dirtied block becomes pending again
	}

	// Persist to WAL if enabled (only for normal writes)
	if !opts.isDownloaded && c.persister != nil {
		walEntry := &wal.BlockWriteEntry{
			PayloadID:     payloadID,
			ChunkIdx:      chunkIdx,
			BlockIdx:      blockIdx,
			OffsetInBlock: offsetInBlock,
			Data:          data[dataStart:dataEnd],
		}
		// Release lock during WAL write to avoid deadlock
		entry.mu.Unlock()
		err := c.persister.AppendBlockWrite(walEntry)
		entry.mu.Lock()
		if err != nil {
			return err
		}
	}

	return nil
}
