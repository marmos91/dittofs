package offloader

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/payload/store"
)

// resolveStoreKey returns the remote store key for downloading a block.
// Returns "" if no FileBlock exists (sparse) or if the block is not yet remote.
func (m *Offloader) resolveStoreKey(ctx context.Context, payloadID string, blockIdx uint64) (string, error) {
	blockID := fmt.Sprintf("%s/%d", payloadID, blockIdx)
	fb, err := m.fileBlockStore.GetFileBlock(ctx, blockID)
	if err != nil {
		if errors.Is(err, metadata.ErrFileBlockNotFound) {
			return "", nil // Sparse block, not uploaded yet
		}
		return "", fmt.Errorf("resolve store key %s: %w", blockID, err)
	}
	return fb.BlockStoreKey, nil
}

// downloadBlock downloads a single block from the block store and caches it.
// Returns nil data for sparse blocks (no FileBlock entry or missing S3 object).
// Returns nil data when blockStore is nil (local-only mode — no remote data exists).
func (m *Offloader) downloadBlock(ctx context.Context, payloadID string, blockIdx uint64) ([]byte, error) {
	if err := m.checkReady(ctx); err != nil {
		return nil, err
	}

	if m.blockStore == nil {
		logger.Debug("offloader: skipping downloadBlock, no remote store")
		return nil, nil // No remote data exists
	}

	storeKey, err := m.resolveStoreKey(ctx, payloadID, blockIdx)
	if err != nil {
		return nil, err
	}
	if storeKey == "" {
		return nil, nil
	}

	data, err := m.blockStore.ReadBlock(ctx, storeKey)
	if err != nil {
		if errors.Is(err, store.ErrBlockNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("download block %s: %w", storeKey, err)
	}

	offset := blockIdx * uint64(BlockSize)
	if err := m.cache.WriteFromRemote(ctx, payloadID, data, offset); err != nil {
		return nil, fmt.Errorf("cache downloaded block %s: %w", storeKey, err)
	}

	return data, nil
}

// blockRange returns the start and end block indices for a byte range.
func blockRange(offset uint64, length uint32) (start, end uint64) {
	return offset / uint64(BlockSize), (offset + uint64(length) - 1) / uint64(BlockSize)
}

// allBlocksCached returns true if every block in the range is already in cache.
func (m *Offloader) allBlocksCached(ctx context.Context, payloadID string, startIdx, endIdx uint64) bool {
	for blockIdx := startIdx; blockIdx <= endIdx; blockIdx++ {
		if !m.cache.IsBlockCached(ctx, payloadID, blockIdx) {
			return false
		}
	}
	return true
}

// EnsureAvailableAndRead downloads blocks and copies data directly to dest, avoiding
// a second cache ReadAt. Demanded blocks are downloaded inline in the caller's goroutine;
// prefetch uses the worker pool. Returns (filled, error).
func (m *Offloader) EnsureAvailableAndRead(ctx context.Context, payloadID string, offset uint64, length uint32, dest []byte) (bool, error) {
	if length == 0 {
		return false, nil
	}
	if err := m.checkReady(ctx); err != nil {
		return false, err
	}
	if m.blockStore == nil {
		return false, nil // Local-only: all data must be in cache, no downloads possible
	}

	startBlockIdx, endBlockIdx := blockRange(offset, length)
	if m.allBlocksCached(ctx, payloadID, startBlockIdx, endBlockIdx) {
		return false, nil
	}

	filled := false
	anyNeedCache := false

	for blockIdx := startBlockIdx; blockIdx <= endBlockIdx; blockIdx++ {
		if m.cache.IsBlockCached(ctx, payloadID, blockIdx) {
			anyNeedCache = true
			continue
		}

		data, downloaded, err := m.inlineDownloadOrWait(ctx, payloadID, blockIdx)
		if err != nil {
			return false, err
		}

		if !downloaded {
			anyNeedCache = true
			continue
		}

		if data == nil {
			zeroBlockRegion(dest, blockIdx, offset, uint64(length))
			filled = true
			continue
		}

		if copyBlockToDest(dest, data, blockIdx, offset, uint64(length)) {
			filled = true
		}
	}

	if m.config.PrefetchBlocks > 0 {
		for i := 0; i < m.config.PrefetchBlocks; i++ {
			prefetchBlockIdx := endBlockIdx + 1 + uint64(i)
			m.enqueuePrefetch(payloadID, prefetchBlockIdx)
		}
	}

	if anyNeedCache {
		return false, nil // Some blocks were in cache -- caller should use cache ReadAt
	}
	return filled, nil
}

// inlineDownloadOrWait downloads a block inline or waits for an in-flight download.
// Returns (data, true, nil) for inline download, (nil, false, nil) if piggybacked on existing.
func (m *Offloader) inlineDownloadOrWait(ctx context.Context, payloadID string, blockIdx uint64) ([]byte, bool, error) {
	key := cache.FormatStoreKey(payloadID, blockIdx)

	m.inFlightMu.Lock()
	if existing, ok := m.inFlight[key]; ok {
		m.inFlightMu.Unlock()
		select {
		case <-existing.done:
			existing.mu.Lock()
			err := existing.err
			existing.mu.Unlock()
			return nil, false, err
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}

	result := &downloadResult{done: make(chan struct{})}
	m.inFlight[key] = result
	m.inFlightMu.Unlock()

	storeKey, err := m.resolveStoreKey(ctx, payloadID, blockIdx)
	if err != nil {
		m.completeInFlight(key, result, err)
		return nil, false, err
	}
	if storeKey == "" {
		m.completeInFlight(key, result, nil)
		return nil, true, nil
	}

	if m.blockStore == nil {
		m.completeInFlight(key, result, nil)
		return nil, true, nil // No remote store — treat as sparse
	}

	data, err := m.blockStore.ReadBlock(ctx, storeKey)
	if err != nil {
		if errors.Is(err, store.ErrBlockNotFound) {
			m.completeInFlight(key, result, nil)
			return nil, true, nil
		}
		m.completeInFlight(key, result, err)
		return nil, false, fmt.Errorf("download block %s: %w", storeKey, err)
	}

	// Cache write in background; piggybacked waiters are signaled after completion.
	blockOffset := blockIdx * uint64(BlockSize)
	go func() {
		bgCtx := context.Background()
		if cacheErr := m.cache.WriteFromRemote(bgCtx, payloadID, data, blockOffset); cacheErr != nil {
			logger.Warn("inline download: cache write failed",
				"block", key, "error", cacheErr)
		}
		m.completeInFlight(key, result, nil)
	}()

	return data, true, nil
}

// completeInFlight signals completion to all waiters and cleans up tracking.
func (m *Offloader) completeInFlight(key string, result *downloadResult, err error) {
	result.mu.Lock()
	result.err = err
	result.mu.Unlock()
	close(result.done)

	m.inFlightMu.Lock()
	delete(m.inFlight, key)
	m.inFlightMu.Unlock()
}

// blockRegion computes the source offset within a block and destination offset within
// the read buffer for a given block, read offset, and read length.
// Returns (srcOffset, destOffset, copyLen). copyLen=0 means no overlap.
func blockRegion(blockIdx, readOffset, readLength, blockDataLen uint64) (srcOff, destOff, copyLen uint64) {
	blockStart := blockIdx * uint64(BlockSize)
	if readOffset > blockStart {
		srcOff = readOffset - blockStart
	}
	if blockStart > readOffset {
		destOff = blockStart - readOffset
	}
	if srcOff >= blockDataLen || destOff >= readLength {
		return 0, 0, 0
	}
	available := blockDataLen - srcOff
	remaining := readLength - destOff
	copyLen = available
	if remaining < copyLen {
		copyLen = remaining
	}
	return srcOff, destOff, copyLen
}

// zeroBlockRegion zeroes the portion of dest that corresponds to a sparse block.
func zeroBlockRegion(dest []byte, blockIdx, offset, length uint64) {
	_, destOff, n := blockRegion(blockIdx, offset, length, uint64(BlockSize))
	if n > 0 && int(destOff+n) <= len(dest) {
		clear(dest[destOff : destOff+n])
	}
}

// copyBlockToDest copies the relevant portion of block data into dest.
func copyBlockToDest(dest, data []byte, blockIdx, offset, length uint64) bool {
	srcOff, destOff, n := blockRegion(blockIdx, offset, length, uint64(len(data)))
	if n > 0 && int(destOff+n) <= len(dest) && int(srcOff+n) <= len(data) {
		copy(dest[destOff:destOff+n], data[srcOff:srcOff+n])
		return true
	}
	return false
}

// EnsureAvailable ensures the requested data range is in cache, downloading if needed.
// Blocks until data is available and triggers prefetch for upcoming blocks.
func (m *Offloader) EnsureAvailable(ctx context.Context, payloadID string, offset uint64, length uint32) error {
	if length == 0 {
		return nil
	}
	if err := m.checkReady(ctx); err != nil {
		return err
	}
	if m.blockStore == nil {
		return nil // Local-only: all data must be in cache, no downloads possible
	}

	startBlockIdx, endBlockIdx := blockRange(offset, length)
	if m.allBlocksCached(ctx, payloadID, startBlockIdx, endBlockIdx) {
		return nil
	}

	var doneChannels []chan error

	for blockIdx := startBlockIdx; blockIdx <= endBlockIdx; blockIdx++ {
		done := m.enqueueDownload(payloadID, blockIdx)
		if done != nil {
			doneChannels = append(doneChannels, done)
		}
	}

	if m.config.PrefetchBlocks > 0 {
		for i := 0; i < m.config.PrefetchBlocks; i++ {
			m.enqueuePrefetch(payloadID, endBlockIdx+1+uint64(i))
		}
	}

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

// enqueueDownload enqueues a download with in-flight dedup (broadcast pattern).
// Returns a channel to wait on, or nil if already cached.
func (m *Offloader) enqueueDownload(payloadID string, blockIdx uint64) chan error {
	if m.cache.IsBlockCached(context.Background(), payloadID, blockIdx) {
		return nil
	}

	key := cache.FormatStoreKey(payloadID, blockIdx)

	m.inFlightMu.Lock()
	if existing, ok := m.inFlight[key]; ok {
		m.inFlightMu.Unlock()
		waiter := make(chan error, 1)
		go func() {
			<-existing.done
			existing.mu.Lock()
			err := existing.err
			existing.mu.Unlock()
			waiter <- err
		}()
		return waiter
	}

	result := &downloadResult{done: make(chan struct{})}
	m.inFlight[key] = result
	m.inFlightMu.Unlock()

	callerDone := make(chan error, 1)
	req := NewDownloadRequest(payloadID, blockIdx, nil)
	req.Done = make(chan error, 1)

	go func() {
		err := <-req.Done
		m.completeInFlight(key, result, err)
		callerDone <- err
	}()

	if !m.queue.EnqueueDownload(req) {
		req.Done <- fmt.Errorf("download queue full, cannot enqueue block %s", key)
	}

	return callerDone
}

// enqueuePrefetch enqueues a prefetch request (non-blocking, best effort).
func (m *Offloader) enqueuePrefetch(payloadID string, blockIdx uint64) {
	if m.cache.IsBlockCached(context.Background(), payloadID, blockIdx) {
		return
	}

	key := cache.FormatStoreKey(payloadID, blockIdx)
	m.inFlightMu.Lock()
	_, inFlight := m.inFlight[key]
	m.inFlightMu.Unlock()
	if inFlight {
		return
	}

	m.queue.EnqueuePrefetch(NewPrefetchRequest(payloadID, blockIdx))
}
