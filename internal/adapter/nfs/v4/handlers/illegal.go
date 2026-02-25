package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// handleIllegalOp is the standalone handler for the OP_ILLEGAL operation (RFC 7530 Section 16.14).
// Compile-time assertion that handleIllegal exists on Handler with correct OpHandler signature.
// The actual implementation is in handler.go; this file provides organizational clarity.
// Returns NFS4ERR_OP_ILLEGAL for unknown opcodes; terminates compound on error.
// Errors: NFS4ERR_OP_ILLEGAL (always).

// ensureIllegalDefined is a compile-time assertion that handleIllegal exists.
var _ OpHandler = (*Handler)(nil).handleIllegal

// Verify the handler signature matches what COMPOUND dispatch expects.
var _ = func(h *Handler) OpHandler {
	return func(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
		return h.handleIllegal(ctx, reader)
	}
}
