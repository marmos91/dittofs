// Package handlers -- DESTROY_CLIENTID operation handler (op 57).
//
// DESTROY_CLIENTID destroys the client ID and all associated state.
// Per RFC 8881 Section 18.50: the server MUST NOT destroy a client ID
// if it has sessions (NFS4ERR_CLIENTID_BUSY).
// DESTROY_CLIENTID is session-exempt (can be the only op in a COMPOUND
// without SEQUENCE).
//
// The checks below run in a fixed order, and which error a request carrying
// more than one fault is answered with depends on that order (RFC 8881
// Section 18.50.3, and the operation's valid-error list in Section 15.2):
//
//  1. Sharing a COMPOUND with another operation while not preceded by a
//     SEQUENCE -> NFS4ERR_NOT_ONLY_OP. Enforced by the COMPOUND dispatcher
//     before this handler runs, so it outranks every check here.
//  2. Undecodable arguments -> NFS4ERR_BADXDR. Nothing about the target is
//     knowable until the client ID is off the wire.
//  3. A target client ID the server does not recognize ->
//     NFS4ERR_STALE_CLIENTID. This precedes any judgement about the target's
//     state, because NFS4ERR_CLIENTID_BUSY (Section 15.1.13.1) is a statement
//     about state held by a client the server does know.
//  4. A recognized target holding sessions or unexpired state ->
//     NFS4ERR_CLIENTID_BUSY.
//
// NFS4ERR_NOT_SAME is not in the operation's valid-error list and must not be
// returned: an ownership mismatch between requester and target is not an error
// for this operation at all.
package v41handlers

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// HandleDestroyClientID implements the DESTROY_CLIENTID operation (RFC 8881 Section 18.50).
// Destroys a client ID and all associated state (sessions, opens, locks, delegations).
// Delegates to StateManager.PurgeV41Client for state teardown and cleanup.
// Removes all client state; session-exempt (no SEQUENCE required); fails if sessions exist.
// Errors: NFS4ERR_CLIENTID_BUSY (has sessions), NFS4ERR_STALE_CLIENTID, NFS4ERR_BADXDR,
// in the order the package comment records.
func HandleDestroyClientID(
	d *Deps,
	ctx *types.CompoundContext,
	v41ctx *types.V41RequestContext,
	reader io.Reader,
) *types.CompoundResult {
	var args types.DestroyClientidArgs
	if err := args.Decode(reader); err != nil {
		logger.Debug("DESTROY_CLIENTID: decode error", "error", err, "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_DESTROY_CLIENTID,
			Data:   EncodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Which client sent the request does not gate the operation. RFC 8881
	// Section 18.50.3 permits a DESTROY_CLIENTID preceded by a SEQUENCE
	// precisely while the client ID derived from that SEQUENCE's session is
	// *not* the target, so a request naming a client other than the requester's
	// own is well formed and must be carried out. When the two client IDs are
	// the same, the requester still holds the session that carried the request,
	// which is already state on the target and so is answered below as
	// NFS4ERR_CLIENTID_BUSY.
	//
	// ponytail: an unconfirmed or idle client ID is therefore destroyable by
	// anyone who names its 64-bit value; the RFC gates that with SP4_MACH_CRED
	// or SP4_SSV state protection (NFS4ERR_WRONG_CRED), which this server does
	// not enforce. Add the credential check to the operation once state
	// protection is negotiated at EXCHANGE_ID.

	// Delegate to StateManager, which recognizes the target client ID before it
	// weighs the client's state, so an unrecognized target is answered
	// NFS4ERR_STALE_CLIENTID and never NFS4ERR_CLIENTID_BUSY.
	err := d.StateManager.DestroyV41ClientID(args.ClientID)
	if err != nil {
		nfsStatus := MapStateError(err)
		logger.Debug("DESTROY_CLIENTID: state error",
			"error", err,
			"client_id", fmt.Sprintf("0x%016x", args.ClientID),
			"nfs_status", nfsStatus,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_DESTROY_CLIENTID,
			Data:   EncodeStatusOnly(nfsStatus),
		}
	}

	// Encode success response
	res := &types.DestroyClientidRes{Status: types.NFS4_OK}
	var buf bytes.Buffer
	if err := res.Encode(&buf); err != nil {
		logger.Error("DESTROY_CLIENTID: encode response error", "error", err)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_DESTROY_CLIENTID,
			Data:   EncodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	logger.Info("DESTROY_CLIENTID: client destroyed",
		"client_id", fmt.Sprintf("0x%016x", args.ClientID),
		"client", ctx.ClientAddr)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_DESTROY_CLIENTID,
		Data:   buf.Bytes(),
	}
}
