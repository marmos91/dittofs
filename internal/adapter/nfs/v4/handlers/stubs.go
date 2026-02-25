package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
)

// handleDelegPurge implements the DELEGPURGE operation (RFC 7530 Section 16.7).
// Purges all delegations for a client (only meaningful with CLAIM_DELEGATE_PREV support).
// No delegation; always returns NFS4ERR_NOTSUPP (DittoFS has no persistent delegation state).
// No side effects; consumes the clientid arg to prevent XDR desync.
// Errors: NFS4ERR_NOTSUPP (always).
func (h *Handler) handleDelegPurge(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Consume the clientid arg to prevent XDR desync
	_, _ = xdr.DecodeUint64(reader)

	logger.Debug("NFSv4 DELEGPURGE not supported (no CLAIM_DELEGATE_PREV)",
		"client", ctx.ClientAddr)

	return &types.CompoundResult{
		Status: types.NFS4ERR_NOTSUPP,
		OpCode: types.OP_DELEGPURGE,
		Data:   encodeStatusOnly(types.NFS4ERR_NOTSUPP),
	}
}

// handleOpenAttr implements the OPENATTR operation (RFC 7530 Section 16.17).
// Opens a named attribute directory for a file (named attributes not supported).
// No delegation; always returns NFS4ERR_NOTSUPP (DittoFS does not support named attributes).
// No side effects; consumes the createdir arg to prevent XDR desync.
// Errors: NFS4ERR_NOTSUPP (always).
func (h *Handler) handleOpenAttr(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Consume the createdir bool to prevent XDR desync
	_, _ = xdr.DecodeUint32(reader)

	logger.Debug("OPENATTR not supported (named attributes deferred)",
		"client", ctx.ClientAddr)

	return &types.CompoundResult{
		Status: types.NFS4ERR_NOTSUPP,
		OpCode: types.OP_OPENATTR,
		Data:   encodeStatusOnly(types.NFS4ERR_NOTSUPP),
	}
}

// handleOpenDowngrade implements the OPEN_DOWNGRADE operation (RFC 7530 Section 16.19).
// Reduces share_access and/or share_deny bits for an open file to a subset of current bits.
// Delegates to StateManager.DowngradeOpen for validation and stateid update.
// Updates open state share modes; returns updated stateid with incremented seqid.
// Errors: NFS4ERR_NOFILEHANDLE, NFS4ERR_BAD_STATEID, NFS4ERR_INVAL (not a subset), NFS4ERR_BADXDR.
func (h *Handler) handleOpenDowngrade(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_OPEN_DOWNGRADE,
			Data:   encodeStatusOnly(status),
		}
	}

	// Decode args: stateid4 + seqid + share_access + share_deny
	stateid, err := types.DecodeStateid4(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_OPEN_DOWNGRADE,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	downgradeSeqid, err := xdr.DecodeUint32(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_OPEN_DOWNGRADE,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	newShareAccess, err := xdr.DecodeUint32(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_OPEN_DOWNGRADE,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	newShareDeny, err := xdr.DecodeUint32(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_OPEN_DOWNGRADE,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	logger.Debug("NFSv4 OPEN_DOWNGRADE",
		"stateid_seqid", stateid.Seqid,
		"seqid", downgradeSeqid,
		"new_access", newShareAccess,
		"new_deny", newShareDeny,
		"client", ctx.ClientAddr)

	// Delegate to StateManager
	resultStateid, stateErr := h.StateManager.DowngradeOpen(
		stateid, downgradeSeqid, newShareAccess, newShareDeny,
	)
	if stateErr != nil {
		nfsStatus := mapStateError(stateErr)
		logger.Debug("NFSv4 OPEN_DOWNGRADE failed",
			"error", stateErr,
			"nfs_status", nfsStatus,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_OPEN_DOWNGRADE,
			Data:   encodeStatusOnly(nfsStatus),
		}
	}

	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	types.EncodeStateid4(&buf, resultStateid)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_OPEN_DOWNGRADE,
		Data:   buf.Bytes(),
	}
}

// handleReleaseLockOwner implements the RELEASE_LOCKOWNER operation (RFC 7530 Section 16.34).
// Releases all state associated with a lock owner (no-op if lock-owner unknown).
// Delegates to StateManager.ReleaseLockOwner for state cleanup and validation.
// Removes lock-owner tracking; fails if lock-owner holds active byte-range locks.
// Errors: NFS4ERR_LOCKS_HELD (active locks exist), NFS4ERR_BADXDR.
func (h *Handler) handleReleaseLockOwner(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Decode lock_owner4: clientid (uint64) + owner (opaque)
	clientID, err := xdr.DecodeUint64(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_RELEASE_LOCKOWNER,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	ownerData, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_RELEASE_LOCKOWNER,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	logger.Debug("NFSv4 RELEASE_LOCKOWNER",
		"clientid", clientID,
		"client", ctx.ClientAddr)

	// Delegate to StateManager
	stateErr := h.StateManager.ReleaseLockOwner(clientID, ownerData)
	if stateErr != nil {
		nfsStatus := mapStateError(stateErr)
		logger.Debug("NFSv4 RELEASE_LOCKOWNER failed",
			"error", stateErr,
			"nfs_status", nfsStatus,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_RELEASE_LOCKOWNER,
			Data:   encodeStatusOnly(nfsStatus),
		}
	}

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_RELEASE_LOCKOWNER,
		Data:   encodeStatusOnly(types.NFS4_OK),
	}
}
