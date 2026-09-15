package v41handlers

import (
	"bytes"
	"fmt"
	"io"

	v4state "github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// HandleDestroySession implements the DESTROY_SESSION operation (RFC 8881 Section 18.37).
// Tears down a session, releases slot table memory, and unbinds all connections.
// Delegates to StateManager.DestroySession; returns NFS4ERR_DELAY for in-flight requests.
// Removes session from tracking maps; stops backchannel sender; frees all slot resources.
// Errors: NFS4ERR_BADSESSION, NFS4ERR_DELAY (in-flight requests), NFS4ERR_BADXDR.
func HandleDestroySession(d *Deps, ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult {
	var args types.DestroySessionArgs
	if err := args.Decode(reader); err != nil {
		logger.Debug("DESTROY_SESSION: decode error", "error", err, "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_DESTROY_SESSION,
			Data:   EncodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Verify the requesting client owns the target session (RFC 8881 Section 18.37.3).
	// GetSession uses a read lock; we look up before the write-locking DestroySession.
	//
	// DESTROY_SESSION is session-exempt: it may be sent standalone, without a
	// preceding SEQUENCE. The server MUST still confirm the requester owns the
	// target session, otherwise an anonymous caller could destroy a victim's.
	//
	// How the requester is identified differs by path, and only one of them
	// reaches across connections. With a SEQUENCE, the requesting session names
	// the client, so an owner can tear down one of its sessions over another —
	// any connection carrying a session of the same client will do. Standalone,
	// there is nothing to name the client but the connection itself, so it is
	// authorized only when that connection is bound to the target session; a
	// standalone request on an unrelated connection is refused.
	targetSess := d.StateManager.GetSession(args.SessionID)
	if targetSess != nil {
		requestingClientID, identified := resolveRequestingClientID(d, v41ctx, ctx, targetSess)
		if !identified {
			// The caller has no resolvable association with any session/client
			// (no SEQUENCE, the connection is not bound to a session, and no
			// v4.0 client state). Refuse rather than fall through to destroy.
			logger.Debug("DESTROY_SESSION: requester identity unknown -- refusing destroy",
				"target_session", args.SessionID.String(),
				"target_client_id", fmt.Sprintf("0x%016x", targetSess.ClientID),
				"connection_id", ctx.ConnectionID,
				"client", ctx.ClientAddr)
			return &types.CompoundResult{
				Status: types.NFS4ERR_OP_NOT_IN_SESSION,
				OpCode: types.OP_DESTROY_SESSION,
				Data:   EncodeStatusOnly(types.NFS4ERR_OP_NOT_IN_SESSION),
			}
		}
		if requestingClientID != targetSess.ClientID {
			logger.Debug("DESTROY_SESSION: ownership mismatch -- not same client",
				"target_session", args.SessionID.String(),
				"target_client_id", fmt.Sprintf("0x%016x", targetSess.ClientID),
				"requesting_client_id", fmt.Sprintf("0x%016x", requestingClientID),
				"client", ctx.ClientAddr)
			return &types.CompoundResult{
				Status: types.NFS4ERR_NOT_SAME,
				OpCode: types.OP_DESTROY_SESSION,
				Data:   EncodeStatusOnly(types.NFS4ERR_NOT_SAME),
			}
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

// resolveRequestingClientID returns the client ID of the entity that sent this
// DESTROY_SESSION and whether that identity could be established at all, using
// the most authoritative source available (RFC 8881 Section 18.37.3):
//
//  1. v41ctx != nil: a preceding SEQUENCE identified the requesting session, so
//     the requester is that session's owning client.
//  2. The connection this request arrived on is bound to the session being
//     destroyed, which RFC 8881 Section 18.37.3 requires of a standalone
//     DESTROY_SESSION: "DESTROY_SESSION MUST be invoked on a connection that is
//     associated with the session being destroyed." The requester is then the
//     target's own client. One connection may carry several sessions, so every
//     binding on it is searched rather than whichever bound last.
//  3. ctx.ClientState != nil: the v4.0 connection layer set a client ID.
//
// The second return value is false when none of the above can associate the
// caller with a client. In that case the caller is anonymous with respect to
// session state and MUST NOT be allowed to destroy an existing session.
func resolveRequestingClientID(d *Deps, v41ctx *types.V41RequestContext, ctx *types.CompoundContext, target *v4state.Session) (uint64, bool) {
	if v41ctx != nil {
		if reqSess := d.StateManager.GetSession(v41ctx.SessionID); reqSess != nil {
			return reqSess.ClientID, true
		}
		return 0, false
	}
	// Standalone DESTROY_SESSION (no SEQUENCE): authorize via the connection
	// the request arrived on being associated with the target session.
	if ctx.ConnectionID != 0 && target != nil {
		for _, binding := range d.StateManager.GetConnectionBindingsForConn(ctx.ConnectionID) {
			if binding.SessionID == target.SessionID {
				return target.ClientID, true
			}
		}
	}
	if ctx.ClientState != nil {
		return ctx.ClientState.ClientID, true
	}
	return 0, false
}
