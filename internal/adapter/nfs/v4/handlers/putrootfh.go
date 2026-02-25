package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// handlePutRootFH implements the PUTROOTFH operation (RFC 7530 Section 16.24).
// Sets the current filehandle to the pseudo-filesystem root for namespace traversal.
// No delegation; reads PseudoFS.GetRootHandle directly (no store access).
// Sets CurrentFH to pseudo-fs root; entry point for all NFSv4 path resolution.
// Errors: none (always succeeds).
func (h *Handler) handlePutRootFH(ctx *types.CompoundContext, _ io.Reader) *types.CompoundResult {
	rootHandle := h.PseudoFS.GetRootHandle()

	// Make a copy to avoid aliasing the internal PseudoFS handle
	ctx.CurrentFH = make([]byte, len(rootHandle))
	copy(ctx.CurrentFH, rootHandle)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_PUTROOTFH,
		Data:   encodeStatusOnly(types.NFS4_OK),
	}
}
