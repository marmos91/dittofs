package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// handleRestoreFH implements the RESTOREFH operation (RFC 7530 Section 16.31).
//
// RESTOREFH restores the saved filehandle to the current filehandle slot.
// Returns NFS4ERR_RESTOREFH if no saved filehandle exists.
//
// Wire format args: none
// Wire format res:  nfsstat4 (uint32)
func (h *Handler) handleRestoreFH(ctx *types.CompoundContext, _ io.Reader) *types.CompoundResult {
	// Require saved filehandle
	if status := types.RequireSavedFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_RESTOREFH,
			Data:   encodeStatusOnly(status),
		}
	}

	// Restore a copy of the saved filehandle (avoid aliasing)
	ctx.CurrentFH = make([]byte, len(ctx.SavedFH))
	copy(ctx.CurrentFH, ctx.SavedFH)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_RESTOREFH,
		Data:   encodeStatusOnly(types.NFS4_OK),
	}
}
