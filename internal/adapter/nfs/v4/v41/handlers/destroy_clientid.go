// Package handlers -- DESTROY_CLIENTID operation handler (op 57).
//
// DESTROY_CLIENTID destroys the client ID and all associated state.
// Per RFC 8881 Section 18.50: the server MUST NOT destroy a client ID
// if it has sessions (NFS4ERR_CLIENTID_BUSY).
// DESTROY_CLIENTID is session-exempt (can be the only op in a COMPOUND
// without SEQUENCE).
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
// Errors: NFS4ERR_CLIENTID_BUSY (has sessions), NFS4ERR_STALE_CLIENTID, NFS4ERR_BADXDR.
func HandleDestroyClientID(
	d *Deps,
	ctx *types.CompoundContext,
	_ *types.V41RequestContext,
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

	// Delegate to StateManager
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
