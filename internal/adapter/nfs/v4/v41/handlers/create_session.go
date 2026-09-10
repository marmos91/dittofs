package v41handlers

import (
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// HandleCreateSession implements the CREATE_SESSION operation (RFC 8881 Section 18.36).
// Binds a session to a client ID, negotiates channel attributes, and returns a session ID.
// Delegates to StateManager.CreateSession with multi-case replay detection; caches response bytes.
// Creates session state with slot table; auto-binds originating connection as fore-channel.
// Errors: NFS4ERR_STALE_CLIENTID, NFS4ERR_SEQ_MISORDERED, NFS4ERR_ENCR_ALG_UNSUPP, NFS4ERR_BADXDR.
func HandleCreateSession(d *Deps, ctx *types.CompoundContext, _ *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
	var args types.CreateSessionArgs
	if err := args.Decode(reader); err != nil {
		logger.Debug("CREATE_SESSION: decode error", "error", err, "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_CREATE_SESSION,
			Data:   EncodeStatusOnly(types.NFS4ERR_BADXDR),
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
			Data:   EncodeStatusOnly(types.NFS4ERR_ENCR_ALG_UNSUPP),
		}
	}

	// Delegate to StateManager for the multi-case algorithm
	result, cachedReply, err := d.StateManager.CreateSession(
		args.ClientID,
		args.SequenceID,
		args.Flags,
		args.ForeChannelAttrs,
		args.BackChannelAttrs,
		args.CbProgram,
		args.CbSecParms,
		ctx.Principal(),
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
		nfsStatus := MapStateError(err)
		logger.Debug("CREATE_SESSION: state error",
			"error", err,
			"nfs_status", nfsStatus,
			"client_id", fmt.Sprintf("0x%x", args.ClientID),
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_CREATE_SESSION,
			Data:   EncodeStatusOnly(nfsStatus),
		}
	}

	// The state manager already encoded the success response and cached it for
	// replay detection atomically with the sequence-ID bump (RFC 8881 Section
	// 18.36). Reuse those bytes directly; re-encoding here would reopen the
	// replay window the state manager closed.

	// Auto-bind the connection that created the session.
	//
	// It carries the back channel too when the client asked for one: RFC 8881
	// Section 18.36.3 associates the connection CREATE_SESSION arrived on with
	// the session in both directions when csa_flags requests
	// CONN_BACK_CHAN. Binding it fore-only leaves a client that never sends
	// BIND_CONN_TO_SESSION -- which is the normal mount flow for both the Linux
	// client and pynfs -- with a session whose back-channel slot table exists
	// but has no connection under it, so no callback can ever be sent and no
	// delegation can ever be granted.
	//
	// The direction comes from the response flags rather than the request: the
	// server clears CONN_BACK_CHAN when it did not set a back channel up, and
	// binding a direction the session cannot serve would claim a channel that
	// is not there.
	bindDir := uint32(types.CDFC4_FORE)
	if result.Flags&uint32(types.CREATE_SESSION4_FLAG_CONN_BACK_CHAN) != 0 {
		bindDir = types.CDFC4_FORE_OR_BOTH
	}

	// This is best-effort: CREATE_SESSION already succeeded, so we only
	// log a warning if the bind fails (e.g., connection ID not plumbed).
	if ctx.ConnectionID != 0 {
		if _, bindErr := d.StateManager.BindConnToSession(ctx.ConnectionID, result.SessionID, bindDir); bindErr != nil {
			logger.Debug("CREATE_SESSION: auto-bind connection failed",
				"connection_id", ctx.ConnectionID,
				"session_id", result.SessionID.String(),
				"error", bindErr)
		}
	}

	logger.Info("CREATE_SESSION: session created",
		"client_id", fmt.Sprintf("0x%x", args.ClientID),
		"session_id", result.SessionID.String(),
		"fore_slots", result.ForeChannelAttrs.MaxRequests,
		"client", ctx.ClientAddr)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_CREATE_SESSION,
		Data:   result.EncodedRes,
	}
}
