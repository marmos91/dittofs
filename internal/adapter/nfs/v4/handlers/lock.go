package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
)

// handleLock implements the LOCK operation (RFC 7530 Section 16.10).
// Acquires a byte-range lock on a file via new or existing lock-owner path.
// Delegates to StateManager.AcquireLock with new_lock_owner or existing lock stateid.
// Creates lock state in StateManager; returns lock stateid on success or conflict details on denial.
// Errors: NFS4ERR_NOFILEHANDLE, NFS4ERR_DENIED, NFS4ERR_GRACE, NFS4ERR_BAD_STATEID, NFS4ERR_BADXDR.
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
		// In NFSv4.1, per-owner seqid is obsoleted by the slot table (SEQUENCE).
		if ctx.SkipOwnerSeqid {
			openSeqid = 0
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
		// In NFSv4.1, per-owner seqid is obsoleted by the slot table (SEQUENCE).
		if ctx.SkipOwnerSeqid {
			lockSeqid = 0
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
		// In NFSv4.1, per-owner seqid is obsoleted by the slot table (SEQUENCE).
		if ctx.SkipOwnerSeqid {
			lockSeqid = 0
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
// Tests for byte-range lock conflicts without creating any state or stateids.
// Delegates to StateManager.TestLock for conflict detection against existing locks.
// No side effects; read-only lock probe returning NFS4_OK (no conflict) or conflict details.
// Errors: NFS4ERR_NOFILEHANDLE, NFS4ERR_DENIED (conflict found), NFS4ERR_GRACE, NFS4ERR_BADXDR.
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
// Releases a byte-range lock with POSIX split semantics (partial unlock may split locks).
// Delegates to StateManager.ReleaseLock for lock removal and stateid update.
// Removes or splits lock state; returns updated lock stateid with incremented seqid.
// Errors: NFS4ERR_NOFILEHANDLE, NFS4ERR_BAD_STATEID, NFS4ERR_OLD_STATEID, NFS4ERR_BADXDR.
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
	// In NFSv4.1, per-owner seqid is obsoleted by the slot table (SEQUENCE).
	if ctx.SkipOwnerSeqid {
		seqid = 0
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
