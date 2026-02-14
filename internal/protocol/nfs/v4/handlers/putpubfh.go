package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
)

// handlePutPubFH implements the PUTPUBFH operation (RFC 7530 Section 16.23).
//
// PUTPUBFH sets the current filehandle to the public filehandle.
// Per locked decision "PUTPUBFH = PUTROOTFH = pseudo-fs root",
// this behaves identically to PUTROOTFH.
//
// Wire format args: none
// Wire format res:  nfsstat4 (uint32)
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
