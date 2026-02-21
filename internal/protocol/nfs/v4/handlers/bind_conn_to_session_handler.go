package handlers

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
)

// handleBindConnToSession implements the BIND_CONN_TO_SESSION operation
// (RFC 8881 Section 18.34).
//
// BIND_CONN_TO_SESSION associates the current TCP connection with an existing
// session in a specified channel direction (fore, back, or both). This is a
// session-exempt operation -- it can appear as the first op in a COMPOUND
// without a preceding SEQUENCE.
//
// Direction negotiation follows the generous policy:
//   - CDFC4_FORE -> CDFS4_FORE
//   - CDFC4_BACK -> CDFS4_BACK
//   - CDFC4_FORE_OR_BOTH -> CDFS4_BOTH
//   - CDFC4_BACK_OR_BOTH -> CDFS4_BOTH
func (h *Handler) handleBindConnToSession(
	ctx *types.CompoundContext,
	_ *types.V41RequestContext, // nil for session-exempt ops
	reader io.Reader,
) *types.CompoundResult {
	var args types.BindConnToSessionArgs
	if err := args.Decode(reader); err != nil {
		logger.Debug("BIND_CONN_TO_SESSION: decode error", "error", err, "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_BIND_CONN_TO_SESSION,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Log RDMA request at DEBUG level per CONTEXT.md
	if args.UseConnInRDMAMode {
		logger.Debug("BIND_CONN_TO_SESSION: client requested RDMA mode (not supported, will return false)",
			"session_id", args.SessionID.String(),
			"client", ctx.ClientAddr)
	}

	// Validate connection ID is plumbed through (should never be zero after Phase 21)
	if ctx.ConnectionID == 0 {
		logger.Error("BIND_CONN_TO_SESSION: ConnectionID is zero (plumbing error)",
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_BIND_CONN_TO_SESSION,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	// Delegate to StateManager
	result, err := h.StateManager.BindConnToSession(ctx.ConnectionID, args.SessionID, args.Dir)
	if err != nil {
		nfsStatus := mapStateError(err)
		logger.Debug("BIND_CONN_TO_SESSION: state error",
			"error", err,
			"nfs_status", nfsStatus,
			"session_id", args.SessionID.String(),
			"connection_id", ctx.ConnectionID,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_BIND_CONN_TO_SESSION,
			Data:   encodeStatusOnly(nfsStatus),
		}
	}

	// Encode success response
	res := &types.BindConnToSessionRes{
		Status:            types.NFS4_OK,
		SessionID:         args.SessionID,
		Dir:               result.ServerDir,
		UseConnInRDMAMode: false, // RDMA never enabled
	}

	var buf bytes.Buffer
	if err := res.Encode(&buf); err != nil {
		logger.Error("BIND_CONN_TO_SESSION: encode response error", "error", err)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_BIND_CONN_TO_SESSION,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	logger.Info("BIND_CONN_TO_SESSION: connection bound",
		"session_id", args.SessionID.String(),
		"connection_id", ctx.ConnectionID,
		"direction", fmt.Sprintf("0x%x", result.ServerDir),
		"client", ctx.ClientAddr)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_BIND_CONN_TO_SESSION,
		Data:   buf.Bytes(),
	}
}
