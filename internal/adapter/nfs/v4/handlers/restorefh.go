package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// handleRestoreFH implements the RESTOREFH operation (RFC 7530 Section 16.31).
// Restores the saved filehandle to the current filehandle slot (copy, not alias).
// No delegation; reads CompoundContext.SavedFH directly (no store access).
// Sets CurrentFH from SavedFH; used with SAVEFH for two-filehandle operations (LINK, RENAME).
// Errors: NFS4ERR_RESTOREFH (no saved FH set).
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
