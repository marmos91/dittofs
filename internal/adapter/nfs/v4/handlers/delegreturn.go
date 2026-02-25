package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// handleDelegReturn implements the DELEGRETURN operation (RFC 7530 Section 16.8).
// Returns a delegation to the server after CB_RECALL or when the client no longer needs it.
// Delegates to StateManager.ReturnDelegation to remove the delegation tracking state.
// Releases delegation state; idempotent (returning an already-returned delegation succeeds).
// Errors: NFS4ERR_NOFILEHANDLE, NFS4ERR_BAD_STATEID, NFS4ERR_BADXDR.
func (h *Handler) handleDelegReturn(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_DELEGRETURN,
			Data:   encodeStatusOnly(status),
		}
	}

	// Decode DELEGRETURN4args: stateid4
	stateid, err := types.DecodeStateid4(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_DELEGRETURN,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	logger.Debug("NFSv4 DELEGRETURN",
		"stateid_seqid", stateid.Seqid,
		"client", ctx.ClientAddr)

	// Remove delegation state
	stateErr := h.StateManager.ReturnDelegation(stateid)
	if stateErr != nil {
		nfsStatus := mapStateError(stateErr)
		logger.Debug("NFSv4 DELEGRETURN failed",
			"error", stateErr,
			"nfs_status", nfsStatus,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_DELEGRETURN,
			Data:   encodeStatusOnly(nfsStatus),
		}
	}

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_DELEGRETURN,
		Data:   encodeStatusOnly(types.NFS4_OK),
	}
}
