// Package handlers -- FREE_STATEID operation handler (op 45).
//
// FREE_STATEID frees a stateid that the client no longer needs.
// Per RFC 8881 Section 18.38.
// FREE_STATEID requires SEQUENCE (not session-exempt).
package handlers

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// handleFreeStateid implements the FREE_STATEID operation
// (RFC 8881 Section 18.38).
//
// FREE_STATEID frees a stateid that the client no longer needs, allowing
// the server to reclaim associated resources. The stateid must belong to
// the client identified by the SEQUENCE session.
//
// Does NOT trigger a cache flush (locked decision).
func (h *Handler) handleFreeStateid(
	ctx *types.CompoundContext,
	v41ctx *types.V41RequestContext,
	reader io.Reader,
) *types.CompoundResult {
	var args types.FreeStateidArgs
	if err := args.Decode(reader); err != nil {
		logger.Debug("FREE_STATEID: decode error", "error", err, "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_FREE_STATEID,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// FREE_STATEID requires SEQUENCE context for client authorization
	if v41ctx == nil {
		logger.Debug("FREE_STATEID: no session context", "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_OP_NOT_IN_SESSION,
			OpCode: types.OP_FREE_STATEID,
			Data:   encodeStatusOnly(types.NFS4ERR_OP_NOT_IN_SESSION),
		}
	}

	// Extract client ID from the session
	session := h.StateManager.GetSession(v41ctx.SessionID)
	if session == nil {
		logger.Debug("FREE_STATEID: session not found",
			"session_id", v41ctx.SessionID.String(),
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADSESSION,
			OpCode: types.OP_FREE_STATEID,
			Data:   encodeStatusOnly(types.NFS4ERR_BADSESSION),
		}
	}

	clientID := session.ClientID

	// Delegate to StateManager
	err := h.StateManager.FreeStateid(clientID, &args.Stateid)
	if err != nil {
		nfsStatus := mapStateError(err)
		logger.Debug("FREE_STATEID: state error",
			"error", err,
			"client_id", fmt.Sprintf("0x%016x", clientID),
			"stateid_other", hex.EncodeToString(args.Stateid.Other[:]),
			"nfs_status", nfsStatus,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_FREE_STATEID,
			Data:   encodeStatusOnly(nfsStatus),
		}
	}

	// Encode success response
	res := &types.FreeStateidRes{Status: types.NFS4_OK}
	var buf bytes.Buffer
	if err := res.Encode(&buf); err != nil {
		logger.Error("FREE_STATEID: encode response error", "error", err)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_FREE_STATEID,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	logger.Info("FREE_STATEID: stateid freed",
		"client_id", fmt.Sprintf("0x%016x", clientID),
		"stateid_other", hex.EncodeToString(args.Stateid.Other[:]),
		"client", ctx.ClientAddr)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_FREE_STATEID,
		Data:   buf.Bytes(),
	}
}
