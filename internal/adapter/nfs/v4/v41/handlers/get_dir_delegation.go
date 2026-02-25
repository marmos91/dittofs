// Package handlers -- GET_DIR_DELEGATION operation handler (op 46).
//
// GET_DIR_DELEGATION allows a client to request directory change notifications
// from the server. Per RFC 8881 Section 18.39.
// GET_DIR_DELEGATION requires SEQUENCE (not session-exempt).
package v41handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// handleGetDirDelegation implements the GET_DIR_DELEGATION operation
// (RFC 8881 Section 18.39).
//
// GET_DIR_DELEGATION grants a directory delegation with a notification bitmask
// to clients with a valid lease, enabling the client to cache directory
// contents and receive change notifications via CB_NOTIFY.
//
// Response encoding:
//   - On success: NFS4_OK + GDD4_OK with stateid, cookie verifier, notification types
//   - On limit/disabled: NFS4_OK + GDD4_UNAVAIL with will_signal_deleg_avail=false
//   - On expired lease: NFS4ERR_EXPIRED
//   - On missing FH: NFS4ERR_NOFILEHANDLE
//   - On bad session: NFS4ERR_BADSESSION
func HandleGetDirDelegation(
	d *Deps,
	ctx *types.CompoundContext,
	v41ctx *types.V41RequestContext,
	reader io.Reader,
) *types.CompoundResult {
	// Decode GET_DIR_DELEGATION4args
	var args types.GetDirDelegationArgs
	if err := args.Decode(reader); err != nil {
		logger.Debug("GET_DIR_DELEGATION: decode error", "error", err, "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_GET_DIR_DELEGATION,
			Data:   EncodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Require current filehandle (must be a directory)
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_GET_DIR_DELEGATION,
			Data:   EncodeStatusOnly(status),
		}
	}

	// Require session context
	if v41ctx == nil {
		logger.Debug("GET_DIR_DELEGATION: no session context", "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_OP_NOT_IN_SESSION,
			OpCode: types.OP_GET_DIR_DELEGATION,
			Data:   EncodeStatusOnly(types.NFS4ERR_OP_NOT_IN_SESSION),
		}
	}

	// Look up session to get clientID
	session := d.StateManager.GetSession(v41ctx.SessionID)
	if session == nil {
		logger.Debug("GET_DIR_DELEGATION: session not found",
			"session_id", v41ctx.SessionID.String(),
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADSESSION,
			OpCode: types.OP_GET_DIR_DELEGATION,
			Data:   EncodeStatusOnly(types.NFS4ERR_BADSESSION),
		}
	}

	// Extract notification types bitmask from bitmap4
	// The bitmap4 is a []uint32; the first word contains the notification bits
	var notifMask uint32
	if len(args.NotificationTypes) > 0 {
		notifMask = args.NotificationTypes[0]
	}

	// Attempt to grant the directory delegation
	deleg, err := d.StateManager.GrantDirDelegation(session.ClientID, ctx.CurrentFH, notifMask)
	if err != nil {
		// Check if this is an expired lease error
		if stateErr, ok := err.(*state.NFS4StateError); ok && stateErr.Status == types.NFS4ERR_EXPIRED {
			logger.Debug("GET_DIR_DELEGATION: expired lease",
				"client_id", session.ClientID,
				"client", ctx.ClientAddr)
			return &types.CompoundResult{
				Status: types.NFS4ERR_EXPIRED,
				OpCode: types.OP_GET_DIR_DELEGATION,
				Data:   EncodeStatusOnly(types.NFS4ERR_EXPIRED),
			}
		}

		// All other errors (disabled, limit exceeded, duplicate) -> GDD4_UNAVAIL
		logger.Debug("GET_DIR_DELEGATION: unavailable",
			"reason", err.Error(),
			"client_id", session.ClientID,
			"client", ctx.ClientAddr)
		return encodeGetDirDelegationUnavail()
	}

	// Success: encode GDD4_OK response
	logger.Debug("GET_DIR_DELEGATION: granted",
		"client_id", session.ClientID,
		"notification_mask", notifMask,
		"stateid_seqid", deleg.Stateid.Seqid,
		"client", ctx.ClientAddr)

	return encodeGetDirDelegationOK(deleg)
}

// encodeGetDirDelegationOK encodes a successful GET_DIR_DELEGATION response.
func encodeGetDirDelegationOK(deleg *state.DelegationState) *types.CompoundResult {
	res := &types.GetDirDelegationRes{
		Status:         types.NFS4_OK,
		NonFatalStatus: types.GDD4_OK,
		OK: &types.GetDirDelegationResOK{
			CookieVerf:        deleg.CookieVerf,
			Stateid:           deleg.Stateid,
			NotificationTypes: types.Bitmap4{deleg.NotificationMask},
			ChildAttrs:        types.Bitmap4{}, // empty bitmap
			DirAttrs:          types.Bitmap4{}, // empty bitmap
		},
	}

	var buf bytes.Buffer
	if err := res.Encode(&buf); err != nil {
		logger.Error("GET_DIR_DELEGATION: encode response error", "error", err)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_GET_DIR_DELEGATION,
			Data:   EncodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_GET_DIR_DELEGATION,
		Data:   buf.Bytes(),
	}
}

// encodeGetDirDelegationUnavail encodes a GDD4_UNAVAIL response.
// Per user decision: will_signal_deleg_avail=false (no proactive offering).
func encodeGetDirDelegationUnavail() *types.CompoundResult {
	res := &types.GetDirDelegationRes{
		Status:               types.NFS4_OK,
		NonFatalStatus:       types.GDD4_UNAVAIL,
		WillSignalDelegAvail: false,
	}

	var buf bytes.Buffer
	if err := res.Encode(&buf); err != nil {
		logger.Error("GET_DIR_DELEGATION: encode unavail response error", "error", err)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_GET_DIR_DELEGATION,
			Data:   EncodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_GET_DIR_DELEGATION,
		Data:   buf.Bytes(),
	}
}
