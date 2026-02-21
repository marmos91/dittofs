// Package handlers -- BACKCHANNEL_CTL operation handler (op 40).
//
// BACKCHANNEL_CTL allows the client to update the backchannel's callback
// program and security parameters without destroying the session.
// Per RFC 8881 Section 18.33.
package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
)

// handleBackchannelCtl implements the BACKCHANNEL_CTL operation
// (RFC 8881 Section 18.33).
//
// The client uses BACKCHANNEL_CTL to update the callback program number
// and security parameters for the backchannel. This allows the client
// to change these parameters without destroying and recreating the session.
//
// Requirements:
//   - Session must have a backchannel (BackChannelSlots != nil)
//   - At least one security flavor must be acceptable (AUTH_SYS or AUTH_NONE)
//   - BACKCHANNEL_CTL requires SEQUENCE (non-exempt per RFC 8881)
func (h *Handler) handleBackchannelCtl(
	ctx *types.CompoundContext,
	v41ctx *types.V41RequestContext,
	reader io.Reader,
) *types.CompoundResult {
	var args types.BackchannelCtlArgs
	if err := args.Decode(reader); err != nil {
		logger.Debug("BACKCHANNEL_CTL: decode error", "error", err, "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_BACKCHANNEL_CTL,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Look up the session from the V41RequestContext session ID
	session := h.StateManager.GetSession(v41ctx.SessionID)
	if session == nil {
		logger.Debug("BACKCHANNEL_CTL: session not found",
			"session_id", v41ctx.SessionID.String(),
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADSESSION,
			OpCode: types.OP_BACKCHANNEL_CTL,
			Data:   encodeStatusOnly(types.NFS4ERR_BADSESSION),
		}
	}

	// Validate session has a backchannel
	if session.BackChannelSlots == nil {
		logger.Debug("BACKCHANNEL_CTL: session has no backchannel",
			"session_id", v41ctx.SessionID.String(),
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_INVAL,
			OpCode: types.OP_BACKCHANNEL_CTL,
			Data:   encodeStatusOnly(types.NFS4ERR_INVAL),
		}
	}

	// Validate at least one acceptable security flavor
	if !state.HasAcceptableCallbackSecurity(args.SecParms) {
		logger.Debug("BACKCHANNEL_CTL: no acceptable security flavors",
			"session_id", v41ctx.SessionID.String(),
			"sec_parms_count", len(args.SecParms),
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_INVAL,
			OpCode: types.OP_BACKCHANNEL_CTL,
			Data:   encodeStatusOnly(types.NFS4ERR_INVAL),
		}
	}

	// Update backchannel params on the session
	if err := h.StateManager.UpdateBackchannelParams(
		v41ctx.SessionID,
		args.CbProgram,
		args.SecParms,
	); err != nil {
		nfsStatus := mapStateError(err)
		logger.Debug("BACKCHANNEL_CTL: state error",
			"error", err,
			"session_id", v41ctx.SessionID.String(),
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_BACKCHANNEL_CTL,
			Data:   encodeStatusOnly(nfsStatus),
		}
	}

	// Encode success response
	res := &types.BackchannelCtlRes{
		Status: types.NFS4_OK,
	}
	var buf bytes.Buffer
	if err := res.Encode(&buf); err != nil {
		logger.Error("BACKCHANNEL_CTL: encode response error", "error", err)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_BACKCHANNEL_CTL,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	logger.Info("BACKCHANNEL_CTL: backchannel params updated",
		"session_id", v41ctx.SessionID.String(),
		"cb_program", args.CbProgram,
		"sec_parms_count", len(args.SecParms),
		"client", ctx.ClientAddr)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_BACKCHANNEL_CTL,
		Data:   buf.Bytes(),
	}
}
