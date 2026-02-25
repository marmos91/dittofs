package handlers

import (
	"bytes"
	"errors"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// handleSetClientID implements the SETCLIENTID operation (RFC 7530 Section 16.33).
// Establishes client identity using the five-case algorithm from RFC 7530 Section 9.1.1.
// Delegates to StateManager.SetClientID for all case logic and client record management.
// Creates or updates an unconfirmed client record; returns clientid and confirm verifier.
// Errors: NFS4ERR_CLID_INUSE, NFS4ERR_BADXDR.
func (h *Handler) handleSetClientID(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Read client verifier (8 bytes raw)
	var verifier [8]byte
	if _, err := io.ReadFull(reader, verifier[:]); err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_SETCLIENTID,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Read client id string (nfs_client_id4.id)
	clientIDStr, err := xdr.DecodeString(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_SETCLIENTID,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Read callback (cb_client4): program (uint32), netid (string), addr (string)
	cbProgram, err := xdr.DecodeUint32(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_SETCLIENTID,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}
	cbNetID, err := xdr.DecodeString(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_SETCLIENTID,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}
	cbAddr, err := xdr.DecodeString(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_SETCLIENTID,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Read callback_ident (uint32)
	_, err = xdr.DecodeUint32(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_SETCLIENTID,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Build callback info
	callback := state.CallbackInfo{
		Program: cbProgram,
		NetID:   cbNetID,
		Addr:    cbAddr,
	}

	// Delegate to StateManager for the five-case algorithm
	result, err := h.StateManager.SetClientID(clientIDStr, verifier, callback, ctx.ClientAddr)
	if err != nil {
		// Map state errors to NFS4 error codes
		nfsStatus := mapStateError(err)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_SETCLIENTID,
			Data:   encodeStatusOnly(nfsStatus),
		}
	}

	// Encode response: status + clientid (uint64) + confirm verifier (8 bytes)
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	_ = xdr.WriteUint64(&buf, result.ClientID)
	buf.Write(result.ConfirmVerifier[:])

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_SETCLIENTID,
		Data:   buf.Bytes(),
	}
}

// handleSetClientIDConfirm implements the SETCLIENTID_CONFIRM operation (RFC 7530 Section 16.34).
// Confirms a client ID established by SETCLIENTID using the confirm verifier.
// Delegates to StateManager.ConfirmClientID for verifier matching and state promotion.
// Promotes unconfirmed client to confirmed; enables lease tracking and open/lock state.
// Errors: NFS4ERR_STALE_CLIENTID, NFS4ERR_CLID_INUSE, NFS4ERR_BADXDR.
func (h *Handler) handleSetClientIDConfirm(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Read clientid (uint64)
	clientID, err := xdr.DecodeUint64(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_SETCLIENTID_CONFIRM,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Read confirm verifier (8 bytes)
	var confirmVerf [8]byte
	if _, err := io.ReadFull(reader, confirmVerf[:]); err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_SETCLIENTID_CONFIRM,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Delegate to StateManager
	err = h.StateManager.ConfirmClientID(clientID, confirmVerf)
	if err != nil {
		nfsStatus := mapStateError(err)
		logger.Debug("SETCLIENTID_CONFIRM: failed",
			"client_id", clientID,
			"error", err,
			"nfs_status", nfsStatus,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_SETCLIENTID_CONFIRM,
			Data:   encodeStatusOnly(nfsStatus),
		}
	}

	logger.Debug("SETCLIENTID_CONFIRM: success",
		"client_id", clientID,
		"client", ctx.ClientAddr)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_SETCLIENTID_CONFIRM,
		Data:   encodeStatusOnly(types.NFS4_OK),
	}
}

// mapStateError maps state package errors to NFS4 status codes.
// Handles both NFS4StateError (which carry a status directly) and
// sentinel errors (ErrStaleClientID, ErrClientIDInUse).
func mapStateError(err error) uint32 {
	if stateErr, ok := err.(*state.NFS4StateError); ok {
		return stateErr.Status
	}
	switch {
	case errors.Is(err, state.ErrStaleClientID):
		return types.NFS4ERR_STALE_CLIENTID
	case errors.Is(err, state.ErrClientIDInUse):
		return types.NFS4ERR_CLID_INUSE
	default:
		return types.NFS4ERR_SERVERFAULT
	}
}
