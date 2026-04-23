package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/pool"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// blockReadResult holds the result of reading from the block store.
type blockReadResult struct {
	data []byte
	eof  bool
}

// Release returns the data buffer to the pool.
// Must be called after the data is no longer needed (e.g., after encoding).
func (r *blockReadResult) Release() {
	if r.data != nil {
		pool.Put(r.data)
		r.data = nil
	}
}

// readFromBlockStore reads data using the BlockStore ReadAt method.
// The returned result uses a pooled buffer; the caller MUST call result.Release()
// after the data is no longer needed (typically after encoding the response).
func readFromBlockStore(
	ctx *NFSHandlerContext,
	blockStore *engine.BlockStore,
	payloadID metadata.PayloadID,
	offset uint64,
	count uint32,
	clientIP string,
	handle []byte,
) (blockReadResult, error) {
	logger.DebugCtx(ctx.Context, "READ: reading from BlockStore", "handle", fmt.Sprintf("0x%x", handle), "offset", offset, "count", count, "payload_id", payloadID)

	data := pool.Get(int(count))

	n, readErr := blockStore.ReadAt(ctx.Context, string(payloadID), data, offset)

	if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
		return blockReadResult{
			data: data[:n],
			eof:  true,
		}, nil
	}

	if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
		pool.Put(data)
		logger.DebugCtx(ctx.Context, "READ: request cancelled during ReadAt", "handle", fmt.Sprintf("0x%x", handle), "offset", offset, "read", n, "client", clientIP)
		return blockReadResult{}, readErr
	}

	if readErr != nil {
		pool.Put(data)
		return blockReadResult{}, fmt.Errorf("ReadAt error: %w", readErr)
	}

	return blockReadResult{
		data: data[:n],
		eof:  false,
	}, nil
}
