package offloader

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/cache"
)

// ============================================================================
// Download Priority
// ============================================================================

// waitForDownloads blocks until no downloads are pending.
// Called by upload goroutines to yield to downloads.
func (m *Offloader) waitForDownloads() {
	m.ioCond.L.Lock()
	for m.downloadsPending > 0 {
		m.ioCond.Wait()
	}
	m.ioCond.L.Unlock()
}

// ============================================================================
// Block-Level Download (called by queue workers)
// ============================================================================

// downloadBlock downloads a single block from the block store and caches it.
// Called by queue workers for download and prefetch requests.
func (m *Offloader) downloadBlock(ctx context.Context, payloadID string, chunkIdx, blockIdx uint32) error {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return fmt.Errorf("offloader is closed")
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

// ============================================================================
// EnsureAvailable
// ============================================================================

// EnsureAvailable ensures the requested data range is in cache, downloading if needed.
// Blocks until data is available. Also triggers prefetch for upcoming blocks.
//
// This is the preferred method for handling cache misses - it uses the queue
// for downloads with proper priority scheduling and prefetch support.
func (m *Offloader) EnsureAvailable(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32) error {
	if !m.canProcess(ctx) {
		return fmt.Errorf("offloader is closed")
	}

	// Check if range is already in cache
	if m.isRangeInCache(ctx, payloadID, chunkIdx, offset, length) {
		return nil
	}

	// DEBUG: Log cache miss for large offsets (2GB+ files)
	if chunkIdx >= 32 {
		logger.Debug("EnsureAvailable: cache miss, will download",
			"payloadID", payloadID,
			"chunkIdx", chunkIdx,
			"offset", offset,
			"length", length)
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

	// DEBUG: Log number of downloads enqueued
	if chunkIdx >= 32 && len(doneChannels) > 0 {
		logger.Debug("EnsureAvailable: downloads enqueued",
			"payloadID", payloadID,
			"chunkIdx", chunkIdx,
			"downloadsCount", len(doneChannels))
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
	for i, done := range doneChannels {
		// DEBUG: Log waiting for download
		if chunkIdx >= 32 {
			logger.Debug("EnsureAvailable: waiting for download",
				"payloadID", payloadID,
				"chunkIdx", chunkIdx,
				"downloadIndex", i,
				"totalDownloads", len(doneChannels))
		}
		select {
		case err := <-done:
			if err != nil {
				// DEBUG: Log download error
				if chunkIdx >= 32 {
					logger.Debug("EnsureAvailable: download error",
						"payloadID", payloadID,
						"chunkIdx", chunkIdx,
						"downloadIndex", i,
						"error", err)
				}
				return err
			}
			// DEBUG: Log download complete
			if chunkIdx >= 32 {
				logger.Debug("EnsureAvailable: download complete",
					"payloadID", payloadID,
					"chunkIdx", chunkIdx,
					"downloadIndex", i)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

// enqueueDownload enqueues a download, handling in-flight deduplication.
// Returns channel to wait on, or nil if already in cache.
//
// Uses a broadcast pattern: multiple callers requesting the same block will all
// wait on the same downloadResult. When the download completes, the done channel
// is CLOSED (not written to), which notifies ALL waiters simultaneously.
func (m *Offloader) enqueueDownload(payloadID string, chunkIdx, blockIdx uint32) chan error {
	// Check cache first (fast path)
	if m.isBlockInCache(payloadID, chunkIdx, blockIdx) {
		return nil
	}

	key := FormatBlockKey(payloadID, chunkIdx, blockIdx)

	m.inFlightMu.Lock()

	// Check if already in-flight - use broadcast pattern to notify ALL waiters
	if existing, ok := m.inFlight[key]; ok {
		m.inFlightMu.Unlock()
		// Create waiter that receives broadcast from existing download
		waiter := make(chan error, 1)
		go func() {
			<-existing.done // Wait for broadcast (channel close)
			existing.mu.Lock()
			err := existing.err
			existing.mu.Unlock()
			waiter <- err
		}()
		return waiter
	}

	// Create new broadcast result and enqueue
	result := &downloadResult{
		done: make(chan struct{}),
	}
	m.inFlight[key] = result
	m.inFlightMu.Unlock()

	// Create completion channel for this caller
	callerDone := make(chan error, 1)

	// Create the request with a Done channel that broadcasts to all waiters
	req := NewDownloadRequest(payloadID, chunkIdx, blockIdx, nil)
	req.Done = make(chan error, 1)

	// Goroutine to handle completion: broadcast to all waiters
	go func() {
		err := <-req.Done // Wait for worker to signal completion

		// Set result and broadcast to ALL waiters by closing the done channel
		result.mu.Lock()
		result.err = err
		result.mu.Unlock()
		close(result.done) // Broadcast: closing notifies ALL receivers

		// Cleanup in-flight tracking
		m.inFlightMu.Lock()
		delete(m.inFlight, key)
		m.inFlightMu.Unlock()

		// Signal the original caller
		callerDone <- err
	}()

	// Enqueue the download - if queue is full, signal error immediately
	if !m.queue.EnqueueDownload(req) {
		// Queue is full - signal error on req.Done to trigger the broadcast
		req.Done <- fmt.Errorf("download queue full, cannot enqueue block %s", key)
	}

	return callerDone
}

// enqueuePrefetch enqueues a prefetch request (non-blocking, best effort).
func (m *Offloader) enqueuePrefetch(payloadID string, chunkIdx, blockIdx uint32) {
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
func (m *Offloader) isBlockInCache(payloadID string, chunkIdx, blockIdx uint32) bool {
	blockOffset := blockIdx * BlockSize
	covered, err := m.cache.IsRangeCovered(context.Background(), payloadID, chunkIdx, blockOffset, BlockSize)
	return err == nil && covered
}

// isRangeInCache checks if a range is fully in cache.
func (m *Offloader) isRangeInCache(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32) bool {
	covered, err := m.cache.IsRangeCovered(ctx, payloadID, chunkIdx, offset, length)
	return err == nil && covered
}
