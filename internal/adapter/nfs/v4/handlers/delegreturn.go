package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// handleDelegReturn implements the DELEGRETURN operation (RFC 7530 Section 16.8).
//
// DELEGRETURN returns a delegation to the server. The client sends this after
// receiving a CB_RECALL callback, or proactively when it no longer needs the
// delegation.
//
// Per RFC 7530 Section 16.8:
//   - The current filehandle must be set
//   - The delegation stateid is decoded from args
//   - The delegation is removed from StateManager
//   - Returns NFS4_OK on success (including idempotent return)
//
// Wire format args (DELEGRETURN4args):
//
//	stateid4  deleg_stateid
//
// Wire format res (DELEGRETURN4res):
//
//	nfsstat4  status
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
