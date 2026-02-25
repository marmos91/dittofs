package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// handleSaveFH implements the SAVEFH operation (RFC 7530 Section 16.32).
// Saves the current filehandle to the saved filehandle slot (copy, not alias).
// No delegation; copies CompoundContext.CurrentFH directly (no store access).
// Sets SavedFH from CurrentFH; used before LINK/RENAME to establish the source handle.
// Errors: NFS4ERR_NOFILEHANDLE (no current FH set).
func (h *Handler) handleSaveFH(ctx *types.CompoundContext, _ io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_SAVEFH,
			Data:   encodeStatusOnly(status),
		}
	}

	// Save a copy of the current filehandle (avoid aliasing)
	ctx.SavedFH = make([]byte, len(ctx.CurrentFH))
	copy(ctx.SavedFH, ctx.CurrentFH)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_SAVEFH,
		Data:   encodeStatusOnly(types.NFS4_OK),
	}
}
