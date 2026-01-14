package handlers

import (
	"context"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/bufpool"
	"github.com/marmos91/dittofs/pkg/payload"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Content Read Operations
// ============================================================================

// contentReadResult holds the result of reading from the content service.
type contentReadResult struct {
	data      []byte
	bytesRead int
	eof       bool
	pooled    bool // true if data buffer came from bufpool and should be returned
}

// Release returns the data buffer to the pool if it was pooled.
// Must be called after the data is no longer needed (e.g., after encoding).
func (r *contentReadResult) Release() {
	if r.pooled && r.data != nil {
		bufpool.Put(r.data)
		r.data = nil
		r.pooled = false
	}
}

// readFromContentService reads data using the ContentService ReadAt method.
// The Cache always supports efficient random-access reads.
//
// The returned result uses a pooled buffer. The caller MUST call result.Release()
// after the data is no longer needed (typically after encoding the response).
//
// Parameters:
//   - ctx: Handler context with cancellation support
//   - contentSvc: Content service for reading (backed by Cache)
//   - payloadID: Content identifier to read
//   - offset: Byte offset to read from
//   - count: Number of bytes to read
//   - clientIP: Client IP for logging
//   - handle: File handle for logging
//
// Returns:
//   - contentReadResult: Result with data (caller must call Release())
//   - error: Error if read failed
func readFromContentService(
	ctx *NFSHandlerContext,
	contentSvc *payload.PayloadService,
	payloadID metadata.PayloadID,
	offset uint64,
	count uint32,
	clientIP string,
	handle []byte,
) (contentReadResult, error) {
	logger.DebugCtx(ctx.Context, "READ: reading from Cache", "handle", fmt.Sprintf("0x%x", handle), "offset", offset, "count", count, "content_id", payloadID)

	// Get a pooled buffer for the read
	data := bufpool.Get(int(count))
	n, readErr := contentSvc.ReadAt(ctx.Context, ctx.Share, payloadID, data, offset)

	// Handle ReadAt results
	if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
		return contentReadResult{
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
		return contentReadResult{}, readErr
	}

	if readErr != nil {
		// Return buffer to pool on error
		bufpool.Put(data)
		return contentReadResult{}, fmt.Errorf("ReadAt error: %w", readErr)
	}

	return contentReadResult{
		data:      data,
		bytesRead: n,
		eof:       false,
		pooled:    true,
	}, nil
}
