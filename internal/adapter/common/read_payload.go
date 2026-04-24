package common

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/pool"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// BlockReadResult holds a pooled read buffer and an EOF flag.
// Callers MUST call Release() after the buffer is no longer referenced
// (typically after the response has been written to the wire).
// For SMB, the Release closure is handed to the response encoder via
// SMBResponseBase.ReleaseData (wired in plan 02 per D-09). For NFSv3,
// the existing Releaser path at internal/adapter/nfs/helpers.go fires it.
type BlockReadResult struct {
	Data []byte
	EOF  bool
}

// Release returns the data buffer to the pool.
// Must be called after the data is no longer needed (e.g., after encoding).
// Safe to call multiple times — subsequent calls are no-ops.
func (r *BlockReadResult) Release() {
	if r.Data != nil {
		pool.Put(r.Data)
		r.Data = nil
	}
}

// ReadFromBlockStore reads `count` bytes starting at `offset` from the given
// block store into a pooled buffer. On any error path the pooled buffer is
// returned via pool.Put and a zero-value BlockReadResult is returned.
//
// Per D-03 this helper takes a plain context.Context — callers thread in
// an auth-scoped ctx from their own dispatch layer (NFSv3 NFSHandlerContext,
// SMB SMBHandlerContext, NFSv4 compound ctx). Structural logging fields
// (clientIP, handle bytes) are logged at the call site, not here, because
// they are protocol-specific and common/ cannot couple to those types.
//
// Phase-12 seam: when FileAttr.Blocks becomes []BlockRef (META-01) and
// engine.BlockStore.ReadAt takes []BlockRef (API-01), this function body
// gains the "fetch FileAttr.Blocks → slice to [offset, offset+len) → pass
// resolved refs" logic. Call-site code (protocol handlers) does not change.
func ReadFromBlockStore(
	ctx context.Context,
	blockStore *engine.BlockStore,
	payloadID metadata.PayloadID,
	offset uint64,
	count uint32,
) (BlockReadResult, error) {
	data := pool.Get(int(count))
	n, readErr := blockStore.ReadAt(ctx, string(payloadID), data, offset)

	if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
		return BlockReadResult{Data: data[:n], EOF: true}, nil
	}

	if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
		pool.Put(data)
		return BlockReadResult{}, readErr
	}

	if readErr != nil {
		pool.Put(data)
		return BlockReadResult{}, fmt.Errorf("ReadAt error: %w", readErr)
	}

	return BlockReadResult{Data: data[:n], EOF: false}, nil
}
