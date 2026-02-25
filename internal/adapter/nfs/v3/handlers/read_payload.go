package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/pool"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/payload"
)

// ============================================================================
// Payload Read Operations
// ============================================================================

// payloadReadResult holds the result of reading from the payload service.
type payloadReadResult struct {
	data      []byte
	bytesRead int
	eof       bool
	pooled    bool // true if data buffer came from pool and should be returned
}

// Release returns the data buffer to the pool if it was pooled.
// Must be called after the data is no longer needed (e.g., after encoding).
func (r *payloadReadResult) Release() {
	if r.pooled && r.data != nil {
		pool.Put(r.data)
		r.data = nil
		r.pooled = false
	}
}

// readFromPayloadService reads data using the PayloadService ReadAt method.
// The Cache always supports efficient random-access reads.
//
// The returned result uses a pooled buffer. The caller MUST call result.Release()
// after the data is no longer needed (typically after encoding the response).
//
// Parameters:
//   - ctx: Handler context with cancellation support
//   - payloadSvc: Content service for reading (backed by Cache)
//   - payloadID: Content identifier to read
//   - cowSource: COW source PayloadID for lazy copy (empty if not a COW file)
//   - offset: Byte offset to read from
//   - count: Number of bytes to read
//   - clientIP: Client IP for logging
//   - handle: File handle for logging
//
// Returns:
//   - payloadReadResult: Result with data (caller must call Release())
//   - error: Error if read failed
func readFromPayloadService(
	ctx *NFSHandlerContext,
	payloadSvc *payload.PayloadService,
	payloadID metadata.PayloadID,
	cowSource metadata.PayloadID,
	offset uint64,
	count uint32,
	clientIP string,
	handle []byte,
) (payloadReadResult, error) {
	logger.DebugCtx(ctx.Context, "READ: reading from Payload Service", "handle", fmt.Sprintf("0x%x", handle), "offset", offset, "count", count, "content_id", payloadID, "cow_source", cowSource)

	// Get a pooled buffer for the read
	data := pool.Get(int(count))

	var n int
	var readErr error
	if cowSource != "" {
		// Use COW-aware read that handles lazy copy from source
		n, readErr = payloadSvc.ReadAtWithCOWSource(ctx.Context, payloadID, cowSource, data, offset)
	} else {
		// Standard read
		n, readErr = payloadSvc.ReadAt(ctx.Context, payloadID, data, offset)
	}

	// Handle ReadAt results
	if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
		return payloadReadResult{
			data:      data[:n],
			bytesRead: n,
			eof:       true,
			pooled:    true,
		}, nil
	}

	if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
		// Return buffer to pool on error
		pool.Put(data)
		logger.DebugCtx(ctx.Context, "READ: request cancelled during ReadAt", "handle", fmt.Sprintf("0x%x", handle), "offset", offset, "read", n, "client", clientIP)
		return payloadReadResult{}, readErr
	}

	if readErr != nil {
		// Return buffer to pool on error
		pool.Put(data)
		return payloadReadResult{}, fmt.Errorf("ReadAt error: %w", readErr)
	}

	return payloadReadResult{
		data:      data,
		bytesRead: n,
		eof:       false,
		pooled:    true,
	}, nil
}
