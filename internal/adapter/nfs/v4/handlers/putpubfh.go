package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// handlePutPubFH implements the PUTPUBFH operation (RFC 7530 Section 16.23).
// Sets the current filehandle to the public filehandle (pseudo-fs root).
// No delegation; reads PseudoFS.GetRootHandle directly (identical to PUTROOTFH).
// Sets CurrentFH to pseudo-fs root; no store access or state changes.
// Errors: none (always succeeds).
func (h *Handler) handlePutPubFH(ctx *types.CompoundContext, _ io.Reader) *types.CompoundResult {
	rootHandle := h.PseudoFS.GetRootHandle()

	// Make a copy to avoid aliasing the internal PseudoFS handle
	ctx.CurrentFH = make([]byte, len(rootHandle))
	copy(ctx.CurrentFH, rootHandle)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_PUTPUBFH,
		Data:   encodeStatusOnly(types.NFS4_OK),
	}
}
