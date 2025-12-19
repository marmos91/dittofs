// Package cache provides shared cache operations between protocol handlers.
//
// This package contains business logic that is protocol-agnostic and can be
// shared between NFS, SMB, and other protocol handlers. It sits above the
// store packages to avoid import cycles.
package cache

import (
	"context"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/bytesize"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// FlushAndFinalizeCache flushes cache data to the content store AND finalizes the upload.
//
// This is used when we need immediate durability (e.g., SMB CLOSE) as opposed to
// just flushing (NFS COMMIT) which allows the background flusher to finalize.
//
// For S3 with IncrementalWriteStore:
//   - FlushIncremental uploads complete parts
//   - CompleteIncrementalWrite finalizes (PutObject for small files, CompleteMultipartUpload for large)
//
// For other stores (filesystem, memory):
//   - Just calls FlushCacheToContentStore (already writes data directly)
func FlushAndFinalizeCache(
	ctx context.Context,
	c cache.Cache,
	contentStore content.ContentStore,
	contentID metadata.ContentID,
) (*FlushResult, error) {
	// First, flush any pending data
	result, err := FlushCacheToContentStore(ctx, c, contentStore, contentID)
	if err != nil {
		return nil, err
	}

	// For incremental stores (S3), we need to finalize the upload
	if incStore, ok := contentStore.(content.IncrementalWriteStore); ok {
		err := incStore.CompleteIncrementalWrite(ctx, contentID, c)
		if err != nil {
			return nil, fmt.Errorf("failed to complete incremental write: %w", err)
		}

		// Transition to StateCached (clean, can be evicted)
		c.SetState(contentID, cache.StateCached)

		logger.Info("Flush: finalized upload", "content_id", contentID)
	}

	return result, nil
}

// FlushResult contains information about a flush operation.
type FlushResult struct {
	// BytesFlushed is the number of bytes written to the content store.
	BytesFlushed uint64

	// Incremental indicates whether incremental flush was used (S3 multipart).
	Incremental bool

	// AlreadyFlushed indicates all data was already flushed (no-op).
	AlreadyFlushed bool
}

// FlushCacheToContentStore flushes cache data to the content store.
//
// This is the core flush logic shared between NFS COMMIT and SMB FLUSH handlers.
// It handles different content store capabilities:
//
//   - IncrementalWriteStore (S3): Uses FlushIncremental for streaming multipart uploads.
//     Small files (< part size) are buffered and uploaded via PutObject on finalization.
//
//   - WriteAt-capable stores (filesystem, memory): Writes only new bytes since the
//     last flush, using GetFlushedOffset/SetFlushedOffset to track progress.
//
// After flushing, the cache state is transitioned to StateUploading so the
// background flusher can complete the upload when the file becomes idle.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - c: The cache containing data to flush
//   - contentStore: The destination content store
//   - contentID: The content identifier to flush
//
// Returns:
//   - *FlushResult: Information about what was flushed
//   - error: Any error during the flush operation
//
// Example (NFS COMMIT):
//
//	result, err := ops.FlushCacheToContentStore(ctx, c, store, file.ContentID)
//	if err != nil {
//	    return types.NFS3ErrIO
//	}
//	logger.Info("Flushed", "bytes", result.BytesFlushed)
func FlushCacheToContentStore(
	ctx context.Context,
	c cache.Cache,
	contentStore content.ContentStore,
	contentID metadata.ContentID,
) (*FlushResult, error) {
	cacheSize := c.Size(contentID)
	flushedOffset := c.GetFlushedOffset(contentID)

	// Check for incremental write support first (S3)
	if incStore, ok := contentStore.(content.IncrementalWriteStore); ok {
		// Incremental write (S3): parallel multipart uploads
		// No handler-level lock needed - FlushIncremental handles concurrency internally
		// using uploadedParts/uploadingParts maps to coordinate parallel uploads

		// Flush to content store - uploads complete parts in parallel
		// Returns 0 for small files (< partSize) - they'll use PutObject on finalization
		flushed, err := incStore.FlushIncremental(ctx, contentID, c)
		if err != nil {
			return nil, fmt.Errorf("incremental flush error: %w", err)
		}

		// Transition to StateUploading so the background flusher can complete the upload
		// when the file becomes idle (no more writes for flush_timeout duration)
		c.SetState(contentID, cache.StateUploading)

		logger.Info("Flush: flushed incrementally", "bytes", bytesize.ByteSize(flushed), "content_id", contentID)

		return &FlushResult{
			BytesFlushed: flushed,
			Incremental:  true,
		}, nil
	}

	// WriteAt-capable store (filesystem, memory): write only new bytes
	bytesToFlush := cacheSize - flushedOffset
	if bytesToFlush <= 0 {
		logger.Info("Flush: already up to date", "bytes", bytesize.ByteSize(0), "content_id", contentID)

		return &FlushResult{
			BytesFlushed:   0,
			AlreadyFlushed: true,
		}, nil
	}

	buf := make([]byte, bytesToFlush)
	n, err := c.ReadAt(ctx, contentID, buf, flushedOffset)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("cache read error: %w", err)
	}

	err = contentStore.WriteAt(ctx, contentID, buf[:n], flushedOffset)
	if err != nil {
		return nil, fmt.Errorf("content store write error: %w", err)
	}

	c.SetFlushedOffset(contentID, flushedOffset+uint64(n))

	// Transition to StateUploading so the background flusher can finalize
	c.SetState(contentID, cache.StateUploading)

	logger.Info("Flush: flushed", "bytes", bytesize.ByteSize(n), "offset", bytesize.ByteSize(flushedOffset), "content_id", contentID)

	return &FlushResult{
		BytesFlushed: uint64(n),
	}, nil
}
