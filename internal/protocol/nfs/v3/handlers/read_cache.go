package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/bytesize"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Cache Read Operations
// ============================================================================

// cacheReadResult holds the result of attempting to read from cache.
type cacheReadResult struct {
	data      []byte
	bytesRead int
	eof       bool
	hit       bool // true if data was found in cache
}

// tryReadFromCache attempts to read data from the unified cache.
// Returns cache hit result if successful, or empty result if cache miss.
//
// Cache state handling:
//   - StateBuffering/StateUploading: Read from cache (dirty data, highest priority)
//   - StateCached: Read from cache (clean data)
//   - StatePrefetching: Cache miss (prefetch in progress, read from content store)
//   - StateNone: Cache miss
//
// Parameters:
//   - ctx: Handler context with cancellation support
//   - contentID: Content identifier to read
//   - offset: Byte offset to read from
//   - count: Number of bytes to read
//
// Returns:
//   - cacheReadResult: Result with data if cache hit, empty if cache miss
//   - error: Error if cache read failed (cache miss returns nil error)
func (h *Handler) tryReadFromCache(
	ctx *NFSHandlerContext,
	contentID metadata.ContentID,
	offset uint64,
	count uint32,
) (cacheReadResult, error) {
	c := h.Registry.GetCacheForShare(ctx.Share)
	if c == nil {
		// No cache configured
		return cacheReadResult{hit: false}, nil
	}

	state := c.GetState(contentID)

	switch state {
	case cache.StateBuffering, cache.StateUploading:
		// Dirty data in cache - must read from cache (content store may not have it yet)
		cacheSize := c.Size(contentID)
		if cacheSize > 0 {
			logger.DebugCtx(ctx.Context, "READ: reading dirty data from cache", "state", state, "offset", bytesize.ByteSize(offset), "count", bytesize.ByteSize(count), "cache_size", bytesize.ByteSize(cacheSize), "content_id", contentID)

			data := make([]byte, count)
			n, readErr := c.ReadAt(ctx.Context, contentID, data, offset)

			if readErr == nil || readErr == io.EOF {
				eof := (readErr == io.EOF) || (offset+uint64(n) >= cacheSize)
				logger.DebugCtx(ctx.Context, "READ: cache hit (dirty)", "bytes_read", bytesize.ByteSize(n), "eof", eof, "content_id", contentID)

				if h.Metrics != nil {
					h.Metrics.RecordCacheHit(ctx.Share, "dirty", uint64(n))
				}

				return cacheReadResult{
					data:      data[:n],
					bytesRead: n,
					eof:       eof,
					hit:       true,
				}, nil
			}

			logger.WarnCtx(ctx.Context, "READ: cache read error (dirty data), this is unexpected", "content_id", contentID, "error", readErr)
			// Fall through to content store - but this shouldn't happen for dirty data
		}

	case cache.StateCached:
		// Clean data in cache - read from cache
		cacheSize := c.Size(contentID)
		if cacheSize > 0 {
			logger.DebugCtx(ctx.Context, "READ: reading from cache", "offset", bytesize.ByteSize(offset), "count", bytesize.ByteSize(count), "cache_size", bytesize.ByteSize(cacheSize), "content_id", contentID)

			data := make([]byte, count)
			n, readErr := c.ReadAt(ctx.Context, contentID, data, offset)

			if readErr == nil || readErr == io.EOF {
				eof := (readErr == io.EOF) || (offset+uint64(n) >= cacheSize)
				logger.DebugCtx(ctx.Context, "READ: cache hit", "bytes_read", bytesize.ByteSize(n), "eof", eof, "content_id", contentID)

				if h.Metrics != nil {
					h.Metrics.RecordCacheHit(ctx.Share, "clean", uint64(n))
				}

				return cacheReadResult{
					data:      data[:n],
					bytesRead: n,
					eof:       eof,
					hit:       true,
				}, nil
			}

			logger.WarnCtx(ctx.Context, "READ: cache read error, falling back to content store", "content_id", contentID, "error", readErr)
		}

	case cache.StatePrefetching:
		// Prefetch in progress - wait for the required offset to be available
		requiredOffset := offset + uint64(count)
		logger.DebugCtx(ctx.Context, "READ: prefetch in progress, waiting for offset", "required_offset", bytesize.ByteSize(requiredOffset), "content_id", contentID)

		if err := c.WaitForPrefetchOffset(ctx.Context, contentID, requiredOffset); err != nil {
			return cacheReadResult{hit: false}, err
		}

		// Our bytes are now available - read from cache
		cacheSize := c.Size(contentID)
		data := make([]byte, count)
		n, readErr := c.ReadAt(ctx.Context, contentID, data, offset)

		if readErr == nil || readErr == io.EOF {
			eof := (readErr == io.EOF) || (offset+uint64(n) >= cacheSize)
			logger.DebugCtx(ctx.Context, "READ: cache hit after prefetch", "bytes_read", bytesize.ByteSize(n), "eof", eof, "content_id", contentID)

			if h.Metrics != nil {
				h.Metrics.RecordCacheHit(ctx.Share, "prefetch", uint64(n))
			}

			return cacheReadResult{
				data:      data[:n],
				bytesRead: n,
				eof:       eof,
				hit:       true,
			}, nil
		}

		logger.WarnCtx(ctx.Context, "READ: cache read error after prefetch wait", "content_id", contentID, "error", readErr)
		// Fall through to cache miss

	case cache.StateNone:
		// Not in cache
		logger.DebugCtx(ctx.Context, "READ: cache miss", "content_id", contentID)
	}

	// Cache miss
	if h.Metrics != nil {
		h.Metrics.RecordCacheMiss(ctx.Share, uint64(count))
	}

	return cacheReadResult{hit: false}, nil
}
