package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// handleDestroySession implements the DESTROY_SESSION operation (RFC 8881 Section 18.37).
//
// DESTROY_SESSION tears down a session, releases slot table memory, and
// unbinds connections. If the session has in-flight requests, the operation
// returns NFS4ERR_DELAY to let the client retry.
func (h *Handler) handleDestroySession(ctx *types.CompoundContext, _ *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
	var args types.DestroySessionArgs
	if err := args.Decode(reader); err != nil {
		logger.Debug("DESTROY_SESSION: decode error", "error", err, "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_DESTROY_SESSION,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Delegate to StateManager
	err := h.StateManager.DestroySession(args.SessionID)
	if err != nil {
		nfsStatus := mapStateError(err)
		logger.Debug("DESTROY_SESSION: state error",
			"error", err,
			"nfs_status", nfsStatus,
			"session_id", args.SessionID.String(),
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_DESTROY_SESSION,
			Data:   encodeStatusOnly(nfsStatus),
		}
	}

	// Encode success response (status only)
	res := &types.DestroySessionRes{Status: types.NFS4_OK}
	var buf bytes.Buffer
	if err := res.Encode(&buf); err != nil {
		logger.Error("DESTROY_SESSION: encode response error", "error", err)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_DESTROY_SESSION,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	logger.Info("DESTROY_SESSION: session destroyed",
		"session_id", args.SessionID.String(),
		"client", ctx.ClientAddr)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_DESTROY_SESSION,
		Data:   buf.Bytes(),
	}
}
