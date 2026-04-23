package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
)

// resolveStoreKey returns the remote store key for downloading a block.
// Returns "" if no FileBlock exists (sparse) or if the block is not yet remote.
func (m *Syncer) resolveStoreKey(ctx context.Context, payloadID string, blockIdx uint64) (string, error) {
	blockID := fmt.Sprintf("%s/%d", payloadID, blockIdx)
	fb, err := m.fileBlockStore.GetFileBlock(ctx, blockID)
	if err != nil {
		if errors.Is(err, blockstore.ErrFileBlockNotFound) {
			return "", nil // Sparse block, not uploaded yet
		}
		return "", fmt.Errorf("resolve store key %s: %w", blockID, err)
	}
	return fb.BlockStoreKey, nil
}

// fetchBlock downloads a single block from the remote store and writes it to the local store.
// Returns nil data for sparse blocks (no FileBlock entry or missing S3 object).
// Returns nil data when remoteStore is nil (local-only mode -- no remote data exists).
func (m *Syncer) fetchBlock(ctx context.Context, payloadID string, blockIdx uint64) ([]byte, error) {
	if !m.canProcess(ctx) {
		return nil, ErrClosed
	}

	if m.remoteStore == nil {
		logger.Debug("syncer: skipping fetchBlock, no remote store")
		return nil, nil // No remote data exists
	}

	// Health gate: fail fast when remote is unreachable
	if !m.IsRemoteHealthy() {
		m.offlineReadsBlocked.Add(1)
		m.logOfflineRead("fetchBlock", payloadID, blockIdx)
		return nil, m.remoteUnavailableError()
	}

	storeKey, err := m.resolveStoreKey(ctx, payloadID, blockIdx)
	if err != nil {
		return nil, err
	}
	if storeKey == "" {
		return nil, nil
	}

	data, err := m.remoteStore.ReadBlock(ctx, storeKey)
	if err != nil {
		if errors.Is(err, blockstore.ErrBlockNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("download block %s: %w", storeKey, err)
	}

	offset := blockIdx * uint64(BlockSize)
	if err := m.local.WriteFromRemote(ctx, payloadID, data, offset); err != nil {
		return nil, fmt.Errorf("store downloaded block %s locally: %w", storeKey, err)
	}

	return data, nil
}

// blockRange returns the start and end block indices for a byte range.
func blockRange(offset uint64, length uint32) (start, end uint64) {
	return offset / uint64(BlockSize), (offset + uint64(length) - 1) / uint64(BlockSize)
}

// allBlocksLocal returns true if every block in the range is already in the local store.
func (m *Syncer) allBlocksLocal(ctx context.Context, payloadID string, startIdx, endIdx uint64) bool {
	for blockIdx := startIdx; blockIdx <= endIdx; blockIdx++ {
		if !m.local.IsBlockLocal(ctx, payloadID, blockIdx) {
			return false
		}
	}
	return true
}

// EnsureAvailableAndRead downloads blocks and copies data directly to dest, avoiding
// a second local ReadAt. Demanded blocks are downloaded inline in the caller's goroutine;
// prefetch uses the worker pool. Returns (filled, error).
func (m *Syncer) EnsureAvailableAndRead(ctx context.Context, payloadID string, offset uint64, length uint32, dest []byte) (bool, error) {
	if length == 0 {
		return false, nil
	}
	if !m.canProcess(ctx) {
		return false, ErrClosed
	}
	if m.remoteStore == nil {
		return false, nil // Local-only: all data must be in local store, no downloads possible
	}

	startBlockIdx, endBlockIdx := blockRange(offset, length)
	if m.allBlocksLocal(ctx, payloadID, startBlockIdx, endBlockIdx) {
		return false, nil
	}

	// Health gate: fail fast when remote is unreachable
	if !m.IsRemoteHealthy() {
		m.offlineReadsBlocked.Add(1)
		m.logOfflineRead("EnsureAvailableAndRead", payloadID, startBlockIdx)
		return false, m.remoteUnavailableError()
	}

	filled := false
	needLocalReadAt := false

	for blockIdx := startBlockIdx; blockIdx <= endBlockIdx; blockIdx++ {
		if m.local.IsBlockLocal(ctx, payloadID, blockIdx) {
			needLocalReadAt = true
			continue
		}

		data, downloaded, err := m.inlineFetchOrWait(ctx, payloadID, blockIdx)
		if err != nil {
			return false, err
		}

		if !downloaded {
			needLocalReadAt = true
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

	for i := range m.config.PrefetchBlocks {
		m.enqueuePrefetch(payloadID, endBlockIdx+1+uint64(i))
	}

	if needLocalReadAt {
		return false, nil // Some blocks were in local store -- caller should use local store ReadAt
	}
	return filled, nil
}

// inlineFetchOrWait downloads a block inline or waits for an in-flight download.
// Returns (data, true, nil) for inline download, (nil, false, nil) if piggybacked on existing.
func (m *Syncer) inlineFetchOrWait(ctx context.Context, payloadID string, blockIdx uint64) ([]byte, bool, error) {
	key := blockstore.FormatStoreKey(payloadID, blockIdx)

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

	result := &fetchResult{done: make(chan struct{})}
	m.inFlight[key] = result
	m.inFlightMu.Unlock()

	// Guarantee inFlight cleanup on all exit paths (including panics).
	// The deferred completeInFlight uses completionErr which is set by
	// each exit path before returning.
	var completionErr error
	completed := false
	defer func() {
		if !completed {
			m.completeInFlight(key, result, completionErr)
		}
	}()

	storeKey, err := m.resolveStoreKey(ctx, payloadID, blockIdx)
	if err != nil {
		completionErr = err
		return nil, false, err
	}
	if storeKey == "" {
		return nil, true, nil
	}

	// Caller (EnsureAvailableAndRead) already verified remoteStore != nil.
	data, err := m.remoteStore.ReadBlock(ctx, storeKey)
	if err != nil {
		if errors.Is(err, blockstore.ErrBlockNotFound) {
			return nil, true, nil
		}
		completionErr = err
		return nil, false, fmt.Errorf("download block %s: %w", storeKey, err)
	}

	// Store locally synchronously; data is already downloaded so there's no
	// reason to hold it in a background goroutine. Under high concurrency,
	// background goroutines each holding 8MB data caused OOM.
	blockOffset := blockIdx * uint64(BlockSize)
	if writeErr := m.local.WriteFromRemote(ctx, payloadID, data, blockOffset); writeErr != nil {
		logger.Warn("inline download: local write failed",
			"block", key, "error", writeErr)
	}
	completed = true
	m.completeInFlight(key, result, nil)

	return data, true, nil
}

// completeInFlight signals completion to all waiters and cleans up tracking.
func (m *Syncer) completeInFlight(key string, result *fetchResult, err error) {
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

// EnsureAvailable ensures the requested data range is in the local store, downloading if needed.
// Blocks until data is available and triggers prefetch for upcoming blocks.
func (m *Syncer) EnsureAvailable(ctx context.Context, payloadID string, offset uint64, length uint32) error {
	if length == 0 {
		return nil
	}
	if !m.canProcess(ctx) {
		return ErrClosed
	}
	if m.remoteStore == nil {
		return nil // Local-only: all data must be in local store, no downloads possible
	}

	startBlockIdx, endBlockIdx := blockRange(offset, length)
	if m.allBlocksLocal(ctx, payloadID, startBlockIdx, endBlockIdx) {
		return nil
	}

	// Health gate: fail fast when remote is unreachable
	if !m.IsRemoteHealthy() {
		m.offlineReadsBlocked.Add(1)
		m.logOfflineRead("EnsureAvailable", payloadID, startBlockIdx)
		return m.remoteUnavailableError()
	}

	var doneChannels []chan error

	for blockIdx := startBlockIdx; blockIdx <= endBlockIdx; blockIdx++ {
		done := m.enqueueDownload(payloadID, blockIdx)
		if done != nil {
			doneChannels = append(doneChannels, done)
		}
	}

	for i := range m.config.PrefetchBlocks {
		m.enqueuePrefetch(payloadID, endBlockIdx+1+uint64(i))
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
// Returns a channel to wait on, or nil if already available locally.
func (m *Syncer) enqueueDownload(payloadID string, blockIdx uint64) chan error {
	if m.local.IsBlockLocal(context.Background(), payloadID, blockIdx) {
		return nil
	}

	// Health gate: fail fast when remote is unreachable
	if !m.IsRemoteHealthy() {
		ch := make(chan error, 1)
		ch <- m.remoteUnavailableError()
		return ch
	}

	key := blockstore.FormatStoreKey(payloadID, blockIdx)

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

	result := &fetchResult{done: make(chan struct{})}
	m.inFlight[key] = result
	m.inFlightMu.Unlock()

	callerDone := make(chan error, 1)
	req := NewDownloadRequest(payloadID, blockIdx, nil)
	req.Done = make(chan error, 1)

	go func() {
		// Wait for either download completion or syncer shutdown.
		// Without the stopCh case, this goroutine leaks if the queue
		// is stopped before processing the request (req.Done never signaled).
		var err error
		select {
		case err = <-req.Done:
		case <-m.stopCh:
			err = ErrClosed
		}
		m.completeInFlight(key, result, err)
		// Non-blocking send: if caller abandoned (ctx cancelled), don't block.
		select {
		case callerDone <- err:
		default:
		}
	}()

	if !m.queue.EnqueueDownload(req) {
		req.Done <- fmt.Errorf("download queue full, cannot enqueue block %s", key)
	}

	return callerDone
}

// enqueuePrefetch enqueues a prefetch request (non-blocking, best effort).
func (m *Syncer) enqueuePrefetch(payloadID string, blockIdx uint64) {
	if m.local.IsBlockLocal(context.Background(), payloadID, blockIdx) {
		return
	}

	// Suppress prefetch when remote is unreachable
	if !m.IsRemoteHealthy() {
		return
	}

	key := blockstore.FormatStoreKey(payloadID, blockIdx)
	m.inFlightMu.Lock()
	_, inFlight := m.inFlight[key]
	m.inFlightMu.Unlock()
	if inFlight {
		return
	}

	m.queue.EnqueuePrefetch(NewPrefetchRequest(payloadID, blockIdx))
}
