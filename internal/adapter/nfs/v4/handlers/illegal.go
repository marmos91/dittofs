package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// handleIllegalOp is the standalone handler for the OP_ILLEGAL operation.
//
// Per RFC 7530, this is used for truly unknown opcodes. The existing
// handleIllegal method on Handler is the primary implementation.
// This file exists for organizational clarity.
//
// Note: The actual handleIllegal is defined in handler.go to keep the
// dispatch table registration close to the handler definition.
// This file serves as documentation that ILLEGAL is a proper operation.

// ensureIllegalDefined is a compile-time assertion that handleIllegal exists.
var _ OpHandler = (*Handler)(nil).handleIllegal

// Verify the handler signature matches what COMPOUND dispatch expects.
var _ = func(h *Handler) OpHandler {
	return func(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
		return h.handleIllegal(ctx, reader)
	}
}
