// Package handlers -- RECLAIM_COMPLETE operation handler (op 58).
//
// RECLAIM_COMPLETE indicates the client has finished reclaiming state
// after a server restart. Per RFC 8881 Section 18.51.
// RECLAIM_COMPLETE requires SEQUENCE (not session-exempt).
package v41handlers

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// handleReclaimComplete implements the RECLAIM_COMPLETE operation
// (RFC 8881 Section 18.51).
//
// RECLAIM_COMPLETE signals the server that the client has completed
// its reclaim phase. For single-FS servers like DittoFS, rca_one_fs=true
// and rca_one_fs=false behave identically.
//
// Requirements:
//   - RECLAIM_COMPLETE requires SEQUENCE (not session-exempt)
//   - If called twice for the same client, returns NFS4ERR_COMPLETE_ALREADY
//   - Outside grace period, returns NFS4_OK (not an error per RFC 8881)
func HandleReclaimComplete(
	d *Deps,
	ctx *types.CompoundContext,
	v41ctx *types.V41RequestContext,
	reader io.Reader,
) *types.CompoundResult {
	var args types.ReclaimCompleteArgs
	if err := args.Decode(reader); err != nil {
		logger.Debug("RECLAIM_COMPLETE: decode error", "error", err, "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_RECLAIM_COMPLETE,
			Data:   EncodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// RECLAIM_COMPLETE requires SEQUENCE context
	if v41ctx == nil {
		logger.Debug("RECLAIM_COMPLETE: no session context", "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_OP_NOT_IN_SESSION,
			OpCode: types.OP_RECLAIM_COMPLETE,
			Data:   EncodeStatusOnly(types.NFS4ERR_OP_NOT_IN_SESSION),
		}
	}

	// Extract client ID from the session
	session := d.StateManager.GetSession(v41ctx.SessionID)
	if session == nil {
		logger.Debug("RECLAIM_COMPLETE: session not found",
			"session_id", v41ctx.SessionID.String(),
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADSESSION,
			OpCode: types.OP_RECLAIM_COMPLETE,
			Data:   EncodeStatusOnly(types.NFS4ERR_BADSESSION),
		}
	}

	clientID := session.ClientID

	// Delegate to StateManager
	err := d.StateManager.ReclaimComplete(clientID)
	if err != nil {
		nfsStatus := MapStateError(err)
		logger.Debug("RECLAIM_COMPLETE: state error",
			"error", err,
			"client_id", fmt.Sprintf("0x%016x", clientID),
			"nfs_status", nfsStatus,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_RECLAIM_COMPLETE,
			Data:   EncodeStatusOnly(nfsStatus),
		}
	}

	// Encode success response
	res := &types.ReclaimCompleteRes{Status: types.NFS4_OK}
	var buf bytes.Buffer
	if err := res.Encode(&buf); err != nil {
		logger.Error("RECLAIM_COMPLETE: encode response error", "error", err)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_RECLAIM_COMPLETE,
			Data:   EncodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	logger.Info("RECLAIM_COMPLETE: reclaim complete",
		"client_id", fmt.Sprintf("0x%016x", clientID),
		"rca_one_fs", args.OneFS,
		"client", ctx.ClientAddr)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_RECLAIM_COMPLETE,
		Data:   buf.Bytes(),
	}
}
