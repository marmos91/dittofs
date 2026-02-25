package v41handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// HandleDestroySession implements the DESTROY_SESSION operation (RFC 8881 Section 18.37).
// Tears down a session, releases slot table memory, and unbinds all connections.
// Delegates to StateManager.DestroySession; returns NFS4ERR_DELAY for in-flight requests.
// Removes session from tracking maps; stops backchannel sender; frees all slot resources.
// Errors: NFS4ERR_BADSESSION, NFS4ERR_DELAY (in-flight requests), NFS4ERR_BADXDR.
func HandleDestroySession(d *Deps, ctx *types.CompoundContext, _ *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
	var args types.DestroySessionArgs
	if err := args.Decode(reader); err != nil {
		logger.Debug("DESTROY_SESSION: decode error", "error", err, "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_DESTROY_SESSION,
			Data:   EncodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Delegate to StateManager
	err := d.StateManager.DestroySession(args.SessionID)
	if err != nil {
		nfsStatus := MapStateError(err)
		logger.Debug("DESTROY_SESSION: state error",
			"error", err,
			"nfs_status", nfsStatus,
			"session_id", args.SessionID.String(),
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_DESTROY_SESSION,
			Data:   EncodeStatusOnly(nfsStatus),
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
			Data:   EncodeStatusOnly(types.NFS4ERR_SERVERFAULT),
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
