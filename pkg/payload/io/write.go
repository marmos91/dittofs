package io

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/payload/chunk"
)

// Cache-full retry constants.
// When the cache is full of pending data, we wait on a condition variable
// for the offloader to drain blocks, rather than polling with timers.
const (
	cacheFullMaxRetries  = 10
	cacheFullWaitTimeout = 5 * time.Second // per-wait timeout; wakes immediately on drain
)

// WriteAt writes data at the specified offset.
//
// Writes go to cache at block-level granularity (4MB blocks).
// Data is split across block boundaries for hash computation and deduplication.
//
// Eager upload: After each block write, complete 4MB blocks are uploaded
// immediately in background goroutines. This reduces data remaining for
// Flush() and improves SMB CLOSE latency.
//
// Backpressure: If the cache is full of pending data (ErrCacheFull), the write
// blocks on a condition variable until the offloader drains pending blocks. Each
// wait is bounded by cacheFullWaitTimeout but typically wakes in milliseconds
// when a block upload completes. This prevents data loss during large sequential
// writes where the write rate temporarily exceeds the upload drain rate.
func (s *ServiceImpl) WriteAt(ctx context.Context, id metadata.PayloadID, data []byte, offset uint64) error {
	if len(data) == 0 {
		return nil
	}

	// PayloadID is the sole identifier for file content
	payloadID := string(id)

	for blockRange := range chunk.BlockRanges(offset, len(data)) {
		dataEnd := blockRange.BufOffset + int(blockRange.Length)

		// Calculate chunk-level offset from block coordinates
		chunkOffset := chunk.ChunkOffsetForBlock(blockRange.BlockIndex) + blockRange.Offset

		// Write block range to cache with retry on backpressure
		err := s.writeBlockWithRetry(ctx, payloadID, blockRange.ChunkIndex, blockRange.BlockIndex,
			data[blockRange.BufOffset:dataEnd], chunkOffset)
		if err != nil {
			return err
		}

		// Trigger eager upload for any complete 4MB blocks (non-blocking)
		s.blockUploader.OnWriteComplete(ctx, payloadID, blockRange.ChunkIndex, chunkOffset, blockRange.Length)
	}

	return nil
}

// writeBlockWithRetry writes a block range to cache, blocking on a condition
// variable when the cache is full of pending data (ErrCacheFull).
//
// This implements backpressure: instead of failing immediately when the cache
// is temporarily full, we wait for the offloader to drain pending blocks via
// a sync.Cond broadcast (signalled by MarkBlockUploaded). Each wait is up to
// cacheFullWaitTimeout (5s) but typically wakes in milliseconds when a 4MB
// block finishes uploading.
//
// This is critical for large sequential writes (e.g., 2.7GB folder copy via
// Finder) where write throughput can exceed the S3 upload drain rate for
// extended periods.
func (s *ServiceImpl) writeBlockWithRetry(ctx context.Context, payloadID string, chunkIdx, blockIdx uint32, data []byte, chunkOffset uint32) error {
	for attempt := 0; attempt <= cacheFullMaxRetries; attempt++ {
		err := s.cacheWriter.WriteAt(ctx, payloadID, chunkIdx, data, chunkOffset)
		if err == nil {
			return nil
		}

		// Only retry on cache-full backpressure errors
		if !errors.Is(err, CacheFullError) {
			return fmt.Errorf("write block %d/%d failed: %w", chunkIdx, blockIdx, err)
		}

		// Last attempt exhausted
		if attempt == cacheFullMaxRetries {
			break
		}

		// Check context before waiting
		if ctx.Err() != nil {
			return fmt.Errorf("write block %d/%d: context cancelled during backpressure: %w", chunkIdx, blockIdx, ctx.Err())
		}

		logger.Debug("Cache full, waiting for uploads to drain",
			"payloadID", payloadID,
			"chunkIdx", chunkIdx,
			"blockIdx", blockIdx,
			"attempt", attempt+1,
			"timeout", cacheFullWaitTimeout)

		// Block until offloader drains a block (or timeout/context cancel)
		if s.backpressureWaiter != nil {
			s.backpressureWaiter.WaitForPendingDrain(ctx, cacheFullWaitTimeout)
			// Check context after waiting — if cancelled, return early with
			// a specific error rather than retrying and getting a generic failure.
			if ctx.Err() != nil {
				return fmt.Errorf("write block %d/%d: context cancelled during backpressure: %w", chunkIdx, blockIdx, ctx.Err())
			}
		} else {
			// Fallback for tests without a backpressure waiter
			select {
			case <-time.After(cacheFullWaitTimeout):
			case <-ctx.Done():
				return fmt.Errorf("write block %d/%d: context cancelled during backpressure: %w", chunkIdx, blockIdx, ctx.Err())
			}
		}
	}

	return fmt.Errorf("write block %d/%d failed after %d retries: %w", chunkIdx, blockIdx, cacheFullMaxRetries, CacheFullError)
}

// Truncate truncates payload to the specified size.
//
// Updates cache and schedules block store cleanup.
func (s *ServiceImpl) Truncate(ctx context.Context, id metadata.PayloadID, newSize uint64) error {
	payloadID := string(id)

	// Truncate in cache
	if err := s.cacheState.Truncate(ctx, payloadID, newSize); err != nil {
		return fmt.Errorf("cache truncate failed: %w", err)
	}

	// Schedule block store cleanup
	return s.blockUploader.Truncate(ctx, payloadID, newSize)
}

// Delete removes payload for a file.
//
// Removes from cache and schedules block store cleanup.
func (s *ServiceImpl) Delete(ctx context.Context, id metadata.PayloadID) error {
	payloadID := string(id)

	// Remove from cache
	if err := s.cacheState.Remove(ctx, payloadID); err != nil {
		return fmt.Errorf("cache remove failed: %w", err)
	}

	// Schedule block store cleanup
	return s.blockUploader.Delete(ctx, payloadID)
}
