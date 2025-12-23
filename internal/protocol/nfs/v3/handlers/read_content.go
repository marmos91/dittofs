package handlers

import (
	"context"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/bufpool"
	"github.com/marmos91/dittofs/pkg/bytesize"
	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// ============================================================================
// Content Store Read Operations
// ============================================================================

// contentStoreReadResult holds the result of reading from content store.
type contentStoreReadResult struct {
	data      []byte
	bytesRead int
	eof       bool
	pooled    bool // true if data buffer came from bufpool and should be returned
}

// Release returns the data buffer to the pool if it was pooled.
// Must be called after the data is no longer needed (e.g., after encoding).
func (r *contentStoreReadResult) Release() {
	if r.pooled && r.data != nil {
		bufpool.Put(r.data)
		r.data = nil
		r.pooled = false
	}
}

// readFromContentStoreWithReadAt reads data using the ReadAt interface for efficient range reads.
// This is dramatically more efficient for backends like S3.
//
// The returned result uses a pooled buffer. The caller MUST call result.Release()
// after the data is no longer needed (typically after encoding the response).
//
// Parameters:
//   - ctx: Handler context with cancellation support
//   - readAtStore: Content store that supports ReadAt
//   - contentID: Content identifier to read
//   - offset: Byte offset to read from
//   - count: Number of bytes to read
//   - clientIP: Client IP for logging
//   - handle: File handle for logging
//
// Returns:
//   - contentStoreReadResult: Result with data (caller must call Release())
//   - error: Error if read failed
func readFromContentStoreWithReadAt(
	ctx *NFSHandlerContext,
	readAtStore content.ReadAtContentStore,
	contentID metadata.ContentID,
	offset uint64,
	count uint32,
	clientIP string,
	handle []byte,
) (contentStoreReadResult, error) {
	logger.DebugCtx(ctx.Context, "READ: using content store ReadAt path", "handle", fmt.Sprintf("0x%x", handle), "offset", offset, "count", count, "content_id", contentID)

	// Get a pooled buffer for the read
	data := bufpool.Get(int(count))
	n, readErr := readAtStore.ReadAt(ctx.Context, contentID, data, offset)

	// Handle ReadAt results
	if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
		return contentStoreReadResult{
			data:      data[:n],
			bytesRead: n,
			eof:       true,
			pooled:    true,
		}, nil
	}

	if readErr == context.Canceled || readErr == context.DeadlineExceeded {
		// Return buffer to pool on error
		bufpool.Put(data)
		logger.DebugCtx(ctx.Context, "READ: request cancelled during ReadAt", "handle", fmt.Sprintf("0x%x", handle), "offset", offset, "read", n, "client", clientIP)
		return contentStoreReadResult{}, readErr
	}

	if readErr != nil {
		// Return buffer to pool on error
		bufpool.Put(data)
		return contentStoreReadResult{}, fmt.Errorf("ReadAt error: %w", readErr)
	}

	return contentStoreReadResult{
		data:      data,
		bytesRead: n,
		eof:       false,
		pooled:    true,
	}, nil
}

// seekToOffset seeks or discards bytes to reach the requested offset in a reader.
// Handles both seekable and non-seekable readers.
//
// Parameters:
//   - ctx: Handler context with cancellation support
//   - reader: Reader to seek (may or may not support io.Seeker)
//   - offset: Target offset
//   - clientIP: Client IP for logging
//   - handle: File handle for logging
//
// Returns:
//   - error: Error if seek/discard failed
func seekToOffset(
	ctx *NFSHandlerContext,
	reader io.ReadCloser,
	offset uint64,
	clientIP string,
	handle []byte,
) error {
	if offset == 0 {
		return nil // Already at start
	}

	if seeker, ok := reader.(io.Seeker); ok {
		// Reader supports seeking - use efficient seek
		_, err := seeker.Seek(int64(offset), io.SeekStart)
		if err != nil {
			return fmt.Errorf("seek error: %w", err)
		}
		return nil
	}

	// Reader doesn't support seeking - read and discard bytes
	logger.DebugCtx(ctx.Context, "READ: reader not seekable, discarding bytes", "bytes", bytesize.ByteSize(offset))

	// Use chunked discard with cancellation checks for large offsets
	const discardChunkSize = 64 * 1024 // 64KB chunks
	remaining := int64(offset)
	totalDiscarded := int64(0)

	for remaining > 0 {
		// Check for cancellation during discard
		select {
		case <-ctx.Context.Done():
			logger.DebugCtx(ctx.Context, "READ: request cancelled during seek discard", "handle", fmt.Sprintf("0x%x", handle), "offset", offset, "discarded", totalDiscarded, "client", clientIP)
			return ctx.Context.Err()
		default:
			// Continue
		}

		// Discard in chunks
		chunkSize := discardChunkSize
		if remaining < int64(chunkSize) {
			chunkSize = int(remaining)
		}

		discardN, discardErr := io.CopyN(io.Discard, reader, int64(chunkSize))
		totalDiscarded += discardN
		remaining -= discardN

		if discardErr == io.EOF {
			return io.EOF // EOF reached while seeking
		}

		if discardErr != nil {
			return fmt.Errorf("cannot skip to offset: %w", discardErr)
		}
	}

	return nil
}

// readFromContentStoreSequential reads data using sequential ReadContent + Seek + Read.
// This is a fallback for content stores that don't support ReadAt.
//
// The returned result uses a pooled buffer. The caller MUST call result.Release()
// after the data is no longer needed (typically after encoding the response).
//
// Parameters:
//   - ctx: Handler context with cancellation support
//   - contentStore: Content store to read from
//   - contentID: Content identifier to read
//   - offset: Byte offset to read from
//   - count: Number of bytes to read
//   - clientIP: Client IP for logging
//   - handle: File handle for logging
//
// Returns:
//   - contentStoreReadResult: Result with data (caller must call Release())
//   - error: Error if read failed
func readFromContentStoreSequential(
	ctx *NFSHandlerContext,
	contentStore content.ContentStore,
	contentID metadata.ContentID,
	offset uint64,
	count uint32,
	clientIP string,
	handle []byte,
) (contentStoreReadResult, error) {
	logger.DebugCtx(ctx.Context, "READ: using sequential read path (no ReadAt support)", "handle", fmt.Sprintf("0x%x", handle), "offset", offset, "count", count)

	reader, err := contentStore.ReadContent(ctx.Context, contentID)
	if err != nil {
		return contentStoreReadResult{}, fmt.Errorf("cannot open content: %w", err)
	}
	defer func() { _ = reader.Close() }()

	// Seek to requested offset
	if err := seekToOffset(ctx, reader, offset, clientIP, handle); err != nil {
		if err == io.EOF {
			// EOF reached while seeking - return empty with EOF (no buffer needed)
			logger.DebugCtx(ctx.Context, "READ: EOF reached while seeking", "handle", fmt.Sprintf("0x%x", handle), "offset", offset, "client", clientIP)
			return contentStoreReadResult{
				data:      []byte{},
				bytesRead: 0,
				eof:       true,
				pooled:    false,
			}, nil
		}
		return contentStoreReadResult{}, err
	}

	// Get a pooled buffer for the read
	data := bufpool.Get(int(count))

	// For large reads (>1MB), use chunked reading with cancellation checks
	const largeReadThreshold = 1024 * 1024 // 1MB
	var n int
	var readErr error

	if count > largeReadThreshold {
		n, readErr = readWithCancellation(ctx.Context, reader, data)
	} else {
		n, readErr = io.ReadFull(reader, data)
	}

	// Handle read results
	if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
		return contentStoreReadResult{
			data:      data[:n],
			bytesRead: n,
			eof:       true,
			pooled:    true,
		}, nil
	}

	if readErr == context.Canceled || readErr == context.DeadlineExceeded {
		// Return buffer to pool on error
		bufpool.Put(data)
		logger.DebugCtx(ctx.Context, "READ: request cancelled during data read", "handle", fmt.Sprintf("0x%x", handle), "offset", offset, "read", n, "client", clientIP)
		return contentStoreReadResult{}, readErr
	}

	if readErr != nil {
		// Return buffer to pool on error
		bufpool.Put(data)
		return contentStoreReadResult{}, fmt.Errorf("I/O error: %w", readErr)
	}

	return contentStoreReadResult{
		data:      data,
		bytesRead: n,
		eof:       false,
		pooled:    true,
	}, nil
}

// readWithCancellation reads data from a reader with periodic context cancellation checks.
// This is used for large reads to ensure responsive cancellation without checking
// on every byte.
//
// The function reads in chunks, checking for cancellation between chunks to balance
// performance with responsiveness.
//
// Parameters:
//   - ctx: Context for cancellation detection
//   - reader: Source to read from
//   - buf: Destination buffer to fill
//
// Returns:
//   - int: Number of bytes actually read
//   - error: Any error encountered (including context cancellation)
func readWithCancellation(ctx context.Context, reader io.Reader, buf []byte) (int, error) {
	const chunkSize = 256 * 1024 // 256KB chunks for cancellation checks

	totalRead := 0
	remaining := len(buf)

	for remaining > 0 {
		// Check for cancellation before each chunk
		select {
		case <-ctx.Done():
			// Return what we've read so far along with context error
			return totalRead, ctx.Err()
		default:
			// Continue reading
		}

		// Determine chunk size for this iteration
		readSize := min(remaining, chunkSize)

		// Read chunk
		n, err := io.ReadFull(reader, buf[totalRead:totalRead+readSize])
		totalRead += n
		remaining -= n

		if err != nil {
			// Return total read and the error (could be EOF, io.ErrUnexpectedEOF, or I/O error)
			return totalRead, err
		}
	}

	return totalRead, nil
}
