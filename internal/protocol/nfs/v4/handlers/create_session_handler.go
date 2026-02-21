package handlers

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
)

// handleCreateSession implements the CREATE_SESSION operation (RFC 8881 Section 18.36).
//
// CREATE_SESSION binds a session to a client ID registered via EXCHANGE_ID,
// negotiates channel attributes for fore and back channels, and returns a
// session ID that the client uses for subsequent SEQUENCE operations.
//
// The state manager handles the multi-case replay detection algorithm;
// the handler only does XDR decode/encode, callback security validation,
// response caching, and error mapping.
func (h *Handler) handleCreateSession(ctx *types.CompoundContext, _ *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
	var args types.CreateSessionArgs
	if err := args.Decode(reader); err != nil {
		logger.Debug("CREATE_SESSION: decode error", "error", err, "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_CREATE_SESSION,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Validate callback security before any state allocation
	if !state.HasAcceptableCallbackSecurity(args.CbSecParms) {
		logger.Debug("CREATE_SESSION: rejecting unacceptable callback security",
			"client_id", fmt.Sprintf("0x%x", args.ClientID),
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_ENCR_ALG_UNSUPP,
			OpCode: types.OP_CREATE_SESSION,
			Data:   encodeStatusOnly(types.NFS4ERR_ENCR_ALG_UNSUPP),
		}
	}

	// Delegate to StateManager for the multi-case algorithm
	result, cachedReply, err := h.StateManager.CreateSession(
		args.ClientID,
		args.SequenceID,
		args.Flags,
		args.ForeChannelAttrs,
		args.BackChannelAttrs,
		args.CbProgram,
		args.CbSecParms,
	)

	// Replay case: return cached XDR response bytes directly
	if cachedReply != nil {
		logger.Debug("CREATE_SESSION: replay detected, returning cached response",
			"client_id", fmt.Sprintf("0x%x", args.ClientID),
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4_OK,
			OpCode: types.OP_CREATE_SESSION,
			Data:   cachedReply,
		}
	}

	if err != nil {
		nfsStatus := mapStateError(err)
		logger.Debug("CREATE_SESSION: state error",
			"error", err,
			"nfs_status", nfsStatus,
			"client_id", fmt.Sprintf("0x%x", args.ClientID),
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_CREATE_SESSION,
			Data:   encodeStatusOnly(nfsStatus),
		}
	}

	// Encode the success response
	res := &types.CreateSessionRes{
		Status:           types.NFS4_OK,
		SessionID:        result.SessionID,
		SequenceID:       result.SequenceID,
		Flags:            result.Flags,
		ForeChannelAttrs: result.ForeChannelAttrs,
		BackChannelAttrs: result.BackChannelAttrs,
	}

	var buf bytes.Buffer
	if err := res.Encode(&buf); err != nil {
		logger.Error("CREATE_SESSION: encode response error", "error", err)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_CREATE_SESSION,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	// Cache the encoded response bytes for replay detection
	h.StateManager.CacheCreateSessionResponse(args.ClientID, buf.Bytes())

	logger.Info("CREATE_SESSION: session created",
		"client_id", fmt.Sprintf("0x%x", args.ClientID),
		"session_id", result.SessionID.String(),
		"fore_slots", result.ForeChannelAttrs.MaxRequests,
		"client", ctx.ClientAddr)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_CREATE_SESSION,
		Data:   buf.Bytes(),
	}
}
