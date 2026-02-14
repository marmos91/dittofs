package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// handleDelegPurge implements the DELEGPURGE operation (RFC 7530 Section 16.7).
//
// DELEGPURGE notifies the server that all delegations for a given client
// (or "all reclaim complete") should be purged. This is only meaningful for
// servers that support CLAIM_DELEGATE_PREV; since DittoFS does not support
// persistent delegation state, DELEGPURGE returns NFS4ERR_NOTSUPP.
//
// Wire format args (DELEGPURGE4args):
//
//	clientid:  uint64
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
//
// OPENATTR opens a named attribute directory for a file. DittoFS does not
// support named attributes, so this always returns NFS4ERR_NOTSUPP.
//
// Wire format args:
//
//	createdir:  bool (uint32) -- whether to create the attr dir if absent
//
// The createdir arg must be consumed to prevent XDR stream desync.
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
//
// OPEN_DOWNGRADE reduces the share_access and/or share_deny bits for an
// open file. It delegates to StateManager.DowngradeOpen which verifies
// that the new bits are a subset of the current bits.
//
// Wire format args:
//
//	open_stateid:  stateid4 (seqid:uint32 + other:12 bytes = 16 bytes)
//	seqid:         uint32
//	share_access:  uint32
//	share_deny:    uint32
//
// Wire format res (success):
//
//	nfsstat4  status (NFS4_OK)
//	stateid4  open_stateid (updated)
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
		nfsStatus := mapOpenStateError(stateErr)
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
//
// RELEASE_LOCKOWNER releases all state associated with a lock owner.
// If the lock-owner has active byte-range locks, NFS4ERR_LOCKS_HELD is returned.
// If the lock-owner has no active locks, all lock state is cleaned up and NFS4_OK is returned.
// Releasing an unknown lock-owner is a no-op (NFS4_OK).
//
// Wire format args:
//
//	lock_owner:  lock_owner4
//	  clientid:  uint64
//	  owner:     opaque<> (uint32 length + bytes + padding)
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
		nfsStatus := mapOpenStateError(stateErr)
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
