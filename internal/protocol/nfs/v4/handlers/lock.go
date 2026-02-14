package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// handleLock implements the LOCK operation (RFC 7530 Section 16.10).
//
// LOCK acquires a byte-range lock on a file. It supports two paths:
//   - new_lock_owner=true (open_to_lock_owner4): creates a new lock-owner
//     and lock stateid from an existing open stateid
//   - new_lock_owner=false (exist_lock_owner4): uses an existing lock stateid
//     to acquire additional locks
//
// The locker4 union discriminant determines which path to take.
//
// Wire format args (LOCK4args):
//
//	locktype:       uint32 (nfs_lock_type4: READ_LT, WRITE_LT, READW_LT, WRITEW_LT)
//	reclaim:        uint32 (bool)
//	offset:         uint64
//	length:         uint64
//	locker:         locker4 (union)
//	  new_lock_owner: uint32 (discriminant)
//	  if true (open_to_lock_owner4):
//	    open_seqid:   uint32
//	    open_stateid: stateid4
//	    lock_seqid:   uint32
//	    lock_owner:   lock_owner4
//	      clientid:   uint64
//	      owner:      opaque<>
//	  if false (exist_lock_owner4):
//	    lock_stateid: stateid4
//	    lock_seqid:   uint32
//
// Wire format res (success - LOCK4res):
//
//	nfsstat4  status (NFS4_OK)
//	stateid4  lock_stateid
//
// Wire format res (denied - LOCK4res):
//
//	nfsstat4      status (NFS4ERR_DENIED)
//	LOCK4denied   denied
func (h *Handler) handleLock(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_LOCK,
			Data:   encodeStatusOnly(status),
		}
	}

	// Reject pseudo-fs handles (not files that can be locked)
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return &types.CompoundResult{
			Status: types.NFS4ERR_INVAL,
			OpCode: types.OP_LOCK,
			Data:   encodeStatusOnly(types.NFS4ERR_INVAL),
		}
	}

	// Decode LOCK4args common fields
	lockType, err := xdr.DecodeUint32(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_LOCK,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	reclaimVal, err := xdr.DecodeUint32(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_LOCK,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}
	reclaim := reclaimVal != 0

	offset, err := xdr.DecodeUint64(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_LOCK,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	length, err := xdr.DecodeUint64(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_LOCK,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Decode locker4 union discriminant
	newLockOwner, err := xdr.DecodeUint32(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_LOCK,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	var result *state.LockResult
	var stateErr error

	if newLockOwner != 0 {
		// open_to_lock_owner4 path
		openSeqid, decErr := xdr.DecodeUint32(reader)
		if decErr != nil {
			return &types.CompoundResult{
				Status: types.NFS4ERR_BADXDR,
				OpCode: types.OP_LOCK,
				Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
			}
		}

		openStateid, decErr := types.DecodeStateid4(reader)
		if decErr != nil {
			return &types.CompoundResult{
				Status: types.NFS4ERR_BADXDR,
				OpCode: types.OP_LOCK,
				Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
			}
		}

		lockSeqid, decErr := xdr.DecodeUint32(reader)
		if decErr != nil {
			return &types.CompoundResult{
				Status: types.NFS4ERR_BADXDR,
				OpCode: types.OP_LOCK,
				Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
			}
		}

		lockOwnerClientID, decErr := xdr.DecodeUint64(reader)
		if decErr != nil {
			return &types.CompoundResult{
				Status: types.NFS4ERR_BADXDR,
				OpCode: types.OP_LOCK,
				Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
			}
		}

		lockOwnerData, decErr := xdr.DecodeOpaque(reader)
		if decErr != nil {
			return &types.CompoundResult{
				Status: types.NFS4ERR_BADXDR,
				OpCode: types.OP_LOCK,
				Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
			}
		}

		logger.Debug("NFSv4 LOCK (new lock-owner)",
			"lock_type", lockType,
			"reclaim", reclaim,
			"offset", offset,
			"length", length,
			"open_seqid", openSeqid,
			"lock_seqid", lockSeqid,
			"lock_owner_clientid", lockOwnerClientID,
			"client", ctx.ClientAddr)

		result, stateErr = h.StateManager.LockNew(
			lockOwnerClientID, lockOwnerData, lockSeqid,
			openStateid, openSeqid,
			ctx.CurrentFH, lockType, offset, length, reclaim,
		)
	} else {
		// exist_lock_owner4 path
		lockStateid, decErr := types.DecodeStateid4(reader)
		if decErr != nil {
			return &types.CompoundResult{
				Status: types.NFS4ERR_BADXDR,
				OpCode: types.OP_LOCK,
				Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
			}
		}

		lockSeqid, decErr := xdr.DecodeUint32(reader)
		if decErr != nil {
			return &types.CompoundResult{
				Status: types.NFS4ERR_BADXDR,
				OpCode: types.OP_LOCK,
				Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
			}
		}

		logger.Debug("NFSv4 LOCK (existing lock-owner)",
			"lock_type", lockType,
			"reclaim", reclaim,
			"offset", offset,
			"length", length,
			"lock_seqid", lockSeqid,
			"stateid_seqid", lockStateid.Seqid,
			"client", ctx.ClientAddr)

		result, stateErr = h.StateManager.LockExisting(
			lockStateid, lockSeqid,
			ctx.CurrentFH, lockType, offset, length, reclaim,
		)
	}

	// Handle errors
	if stateErr != nil {
		nfsStatus := mapStateError(stateErr)
		logger.Debug("NFSv4 LOCK failed",
			"error", stateErr,
			"nfs_status", nfsStatus,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_LOCK,
			Data:   encodeStatusOnly(nfsStatus),
		}
	}

	// Handle conflict (NFS4ERR_DENIED with LOCK4denied)
	if result.Denied != nil {
		var buf bytes.Buffer
		_ = xdr.WriteUint32(&buf, types.NFS4ERR_DENIED)
		state.EncodeLOCK4denied(&buf, result.Denied)

		return &types.CompoundResult{
			Status: types.NFS4ERR_DENIED,
			OpCode: types.OP_LOCK,
			Data:   buf.Bytes(),
		}
	}

	// Success: encode lock stateid
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	types.EncodeStateid4(&buf, &result.Stateid)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_LOCK,
		Data:   buf.Bytes(),
	}
}

// handleLockT implements the LOCKT operation (RFC 7530 Section 16.11).
//
// LOCKT tests for the existence of a byte-range lock conflict without
// creating any state. It does NOT create lock-owners or stateids.
//
// Wire format args (LOCKT4args):
//
//	locktype:   uint32 (nfs_lock_type4)
//	offset:     uint64
//	length:     uint64
//	lock_owner: lock_owner4
//	  clientid: uint64
//	  owner:    opaque<>
//
// Wire format res (no conflict - LOCKT4res):
//
//	nfsstat4  status (NFS4_OK)
//
// Wire format res (conflict - LOCKT4res):
//
//	nfsstat4      status (NFS4ERR_DENIED)
//	LOCK4denied   denied
func (h *Handler) handleLockT(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_LOCKT,
			Data:   encodeStatusOnly(status),
		}
	}

	// Reject pseudo-fs handles
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return &types.CompoundResult{
			Status: types.NFS4ERR_INVAL,
			OpCode: types.OP_LOCKT,
			Data:   encodeStatusOnly(types.NFS4ERR_INVAL),
		}
	}

	// Decode LOCKT4args: locktype, offset, length, lock_owner4
	lockType, err := xdr.DecodeUint32(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_LOCKT,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	offset, err := xdr.DecodeUint64(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_LOCKT,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	length, err := xdr.DecodeUint64(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_LOCKT,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// lock_owner4: clientid + owner opaque
	clientID, err := xdr.DecodeUint64(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_LOCKT,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	ownerData, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_LOCKT,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	logger.Debug("NFSv4 LOCKT",
		"lock_type", lockType,
		"offset", offset,
		"length", length,
		"clientid", clientID,
		"client", ctx.ClientAddr)

	// Delegate to StateManager (no state created)
	denied, stateErr := h.StateManager.TestLock(clientID, ownerData, ctx.CurrentFH, lockType, offset, length)
	if stateErr != nil {
		nfsStatus := mapStateError(stateErr)
		logger.Debug("NFSv4 LOCKT failed",
			"error", stateErr,
			"nfs_status", nfsStatus,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_LOCKT,
			Data:   encodeStatusOnly(nfsStatus),
		}
	}

	// Conflict found: encode NFS4ERR_DENIED + LOCK4denied
	if denied != nil {
		var buf bytes.Buffer
		_ = xdr.WriteUint32(&buf, types.NFS4ERR_DENIED)
		state.EncodeLOCK4denied(&buf, denied)

		return &types.CompoundResult{
			Status: types.NFS4ERR_DENIED,
			OpCode: types.OP_LOCKT,
			Data:   buf.Bytes(),
		}
	}

	// No conflict: status-only NFS4_OK
	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_LOCKT,
		Data:   encodeStatusOnly(types.NFS4_OK),
	}
}

// handleLockU implements the LOCKU operation (RFC 7530 Section 16.12).
//
// LOCKU releases a byte-range lock. The lock manager handles POSIX split
// semantics (partial unlock may result in 0, 1, or 2 remaining locks).
//
// Wire format args (LOCKU4args):
//
//	locktype:     uint32 (nfs_lock_type4)
//	seqid:        uint32
//	lock_stateid: stateid4
//	offset:       uint64
//	length:       uint64
//
// Wire format res (success - LOCKU4res):
//
//	nfsstat4  status (NFS4_OK)
//	stateid4  lock_stateid (updated)
//
// Wire format res (error - LOCKU4res):
//
//	nfsstat4  status
func (h *Handler) handleLockU(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_LOCKU,
			Data:   encodeStatusOnly(status),
		}
	}

	// Reject pseudo-fs handles
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return &types.CompoundResult{
			Status: types.NFS4ERR_INVAL,
			OpCode: types.OP_LOCKU,
			Data:   encodeStatusOnly(types.NFS4ERR_INVAL),
		}
	}

	// Decode LOCKU4args: locktype, seqid, lock_stateid, offset, length
	lockType, err := xdr.DecodeUint32(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_LOCKU,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	seqid, err := xdr.DecodeUint32(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_LOCKU,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	lockStateid, err := types.DecodeStateid4(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_LOCKU,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	offset, err := xdr.DecodeUint64(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_LOCKU,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	length, err := xdr.DecodeUint64(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_LOCKU,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	logger.Debug("NFSv4 LOCKU",
		"lock_type", lockType,
		"seqid", seqid,
		"stateid_seqid", lockStateid.Seqid,
		"offset", offset,
		"length", length,
		"client", ctx.ClientAddr)

	// Delegate to StateManager
	resultStateid, stateErr := h.StateManager.UnlockFile(
		lockStateid, seqid, lockType, offset, length,
	)
	if stateErr != nil {
		nfsStatus := mapStateError(stateErr)
		logger.Debug("NFSv4 LOCKU failed",
			"error", stateErr,
			"nfs_status", nfsStatus,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_LOCKU,
			Data:   encodeStatusOnly(nfsStatus),
		}
	}

	// Success: encode updated lock stateid
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	types.EncodeStateid4(&buf, resultStateid)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_LOCKU,
		Data:   buf.Bytes(),
	}
}
