package handlers

import (
	"context"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/bytesize"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/store/content"
)

// ============================================================================
// Background Prefetch Operations
// ============================================================================

// Prefetch configuration defaults (used when config values are zero)
const (
	// defaultMaxPrefetchSize is the maximum file size to prefetch.
	// Files larger than this are not prefetched to avoid cache thrashing.
	defaultMaxPrefetchSize = 100 * 1024 * 1024 // 100MB

	// defaultPrefetchChunkSize is the size of each chunk read during prefetch.
	// Larger chunks = fewer requests but longer wait before unblocking reads.
	// Smaller chunks = more requests but faster unblocking of waiting reads.
	defaultPrefetchChunkSize = 512 * 1024 // 512KB
)

// startBackgroundPrefetch starts a background goroutine to fetch the entire file into cache.
//
// This is called on cache miss to prefetch the file for future reads.
// The prefetch runs asynchronously - the current READ request has already been served
// from the content store directly.
//
// Prefetch is skipped if:
//   - No cache is configured for this share
//   - Prefetch is disabled for this share
//   - File is too large (> maxPrefetchSize from config)
//   - Prefetch is already in progress for this content ID
func (h *Handler) startBackgroundPrefetch(
	ctx *NFSHandlerContext,
	contentStore content.ContentStore,
	contentID metadata.ContentID,
	fileSize uint64,
) {
	c := h.Registry.GetCacheForShare(ctx.Share)
	if c == nil {
		return // No cache configured
	}

	// Get share to access prefetch config
	share, err := h.Registry.GetShare(ctx.Share)
	if err != nil {
		return // Share not found
	}

	// Check if prefetch is enabled
	if !share.PrefetchConfig.Enabled {
		return // Prefetch disabled
	}

	// Get max file size from config (use default if not set)
	maxFileSize := share.PrefetchConfig.MaxFileSize
	if maxFileSize == 0 {
		maxFileSize = defaultMaxPrefetchSize
	}

	// Skip large files to avoid cache thrashing
	if fileSize > uint64(maxFileSize) {
		logger.DebugCtx(ctx.Context, "READ: skipping prefetch for large file", "content_id", contentID, "size", bytesize.ByteSize(fileSize), "max", bytesize.ByteSize(maxFileSize))
		return
	}

	// Try to start prefetch - returns false if already in progress or not needed
	if !c.StartPrefetch(contentID, fileSize) {
		logger.DebugCtx(ctx.Context, "READ: prefetch already in progress or not needed", "content_id", contentID)
		return
	}

	// Get chunk size from config (use default if not set)
	chunkSize := share.PrefetchConfig.ChunkSize
	if chunkSize == 0 {
		chunkSize = defaultPrefetchChunkSize
	}

	logger.DebugCtx(ctx.Context, "READ: starting background prefetch", "content_id", contentID, "size", bytesize.ByteSize(fileSize), "chunk_size", bytesize.ByteSize(chunkSize))

	// Spawn background goroutine to fetch the file
	go h.runPrefetch(ctx.Share, contentStore, contentID, fileSize, chunkSize)
}

// runPrefetch fetches the entire file content and writes it to cache.
//
// This runs in a background goroutine. It reads the file in chunks,
// updating the prefetched offset after each chunk so that waiting
// READ requests can be served as soon as their bytes are available.
func (h *Handler) runPrefetch(
	share string,
	contentStore content.ContentStore,
	contentID metadata.ContentID,
	fileSize uint64,
	chunkSize int64,
) {
	c := h.Registry.GetCacheForShare(share)
	if c == nil {
		return // Cache was removed
	}

	// Use a background context - prefetch should continue even if original request is done
	ctx := context.Background()

	var offset uint64
	success := false

	defer func() {
		c.CompletePrefetch(contentID, success)
		if success {
			logger.Debug("READ: prefetch completed", "content_id", contentID, "size", bytesize.ByteSize(fileSize))
		} else {
			logger.Warn("READ: prefetch failed", "content_id", contentID)
		}
	}()

	// Check if content store supports ReadAt for efficient chunked reads
	readAtStore, hasReadAt := contentStore.(content.ReadAtContentStore)

	if hasReadAt {
		// Efficient path: read in chunks using ReadAt
		for offset < fileSize {
			remaining := fileSize - offset
			readSize := min(remaining, uint64(chunkSize))

			chunk := make([]byte, readSize)
			n, err := readAtStore.ReadAt(ctx, contentID, chunk, offset)
			if err != nil && err != io.EOF {
				logger.Warn("READ: prefetch chunk read failed", "content_id", contentID, "offset", offset, "error", err)
				return
			}

			if n > 0 {
				// Write chunk to cache
				if err := c.WriteAt(ctx, contentID, chunk[:n], offset); err != nil {
					logger.Warn("READ: prefetch cache write failed", "content_id", contentID, "offset", offset, "error", err)
					return
				}

				offset += uint64(n)
				c.SetPrefetchedOffset(contentID, offset)
			}

			if err == io.EOF || n == 0 {
				break
			}
		}
	} else {
		// Fallback: read entire content using streaming reader
		reader, err := contentStore.ReadContent(ctx, contentID)
		if err != nil {
			logger.Warn("READ: prefetch read failed", "content_id", contentID, "error", err)
			return
		}
		defer func() { _ = reader.Close() }()

		// Read and write in chunks
		for {
			chunk := make([]byte, chunkSize)
			n, err := reader.Read(chunk)

			if n > 0 {
				if writeErr := c.WriteAt(ctx, contentID, chunk[:n], offset); writeErr != nil {
					logger.Warn("READ: prefetch cache write failed", "content_id", contentID, "offset", offset, "error", writeErr)
					return
				}

				offset += uint64(n)
				c.SetPrefetchedOffset(contentID, offset)
			}

			if err == io.EOF {
				break
			}
			if err != nil {
				logger.Warn("READ: prefetch read failed", "content_id", contentID, "offset", offset, "error", err)
				return
			}
		}
	}

	success = true
}
