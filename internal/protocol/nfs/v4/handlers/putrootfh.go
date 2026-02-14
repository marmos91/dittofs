package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
)

// handlePutRootFH implements the PUTROOTFH operation (RFC 7530 Section 16.24).
//
// PUTROOTFH sets the current filehandle to the pseudo-filesystem root.
// This is the entry point for all NFSv4 namespace traversal.
//
// Wire format args: none
// Wire format res:  nfsstat4 (uint32)
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
