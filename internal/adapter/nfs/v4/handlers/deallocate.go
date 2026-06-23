package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// DEALLOCATE4args / DEALLOCATE4res (RFC 7862 Section 15.4):
//
//	struct DEALLOCATE4args {
//	    stateid4 da_stateid;
//	    offset4  da_offset;
//	    length4  da_length;
//	};
//	union DEALLOCATE4res switch (nfsstat4 dr_status) {
//	 case NFS4_OK: void;
//	 default:      void;
//	};
//
// DEALLOCATE punches a hole in [da_offset, da_offset+da_length): the range is
// marked as unbacked (reads back as zeros) and its block-store space is
// reclaimed. The file's logical size is unchanged. The metadata mutation
// (block-ref pruning) is done by metadata.Service.PunchHole; the physical
// reclaim (dedup refcount decrement, remote sweep, GC eligibility) is driven
// here via BlockStore.Truncate with the pre-op block snapshot — the same
// reclaim seam SETATTR-truncate uses (CLAUDE.md invariants #1/#5).
func (h *Handler) handleDeallocate(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return deallocErr(status)
	}
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return deallocErr(types.NFS4ERR_ROFS)
	}

	stateid, offset, length, st := decodeAllocArgs(reader)
	if st != types.NFS4_OK {
		return deallocErr(st)
	}

	// DEALLOCATE modifies file content: validate the stateid as a write op and
	// require WRITE share-access on a real open stateid (special stateids pass).
	if openState, stateErr := h.StateManager.ValidateStateid(stateid, ctx.CurrentFH, state.StateidOpWrite); stateErr != nil {
		s := mapStateError(stateErr)
		logger.Debug("NFSv4.2 DEALLOCATE stateid validation failed", "error", stateErr, "nfs_status", s, "client", ctx.ClientAddr)
		return deallocErr(s)
	} else if openState != nil && openState.ShareAccess&types.OPEN4_SHARE_ACCESS_WRITE == 0 {
		return deallocErr(types.NFS4ERR_OPENMODE)
	}

	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		return deallocErr(nfs4StatusForAuthError(err))
	}
	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return deallocErr(types.NFS4ERR_SERVERFAULT)
	}

	handle := metadata.FileHandle(ctx.CurrentFH)
	res, err := metaSvc.PunchHole(authCtx, handle, offset, length)
	if err != nil {
		return deallocErr(common.MapToNFS4(err))
	}

	// The engine punch is correctness-critical, not just reclaim: the read path
	// resolves bytes with an empty BlockRef list (the dual-read shim), so pruning
	// the metadata block list alone does NOT guarantee zero reads — only the
	// block-store zero-overwrite does. Therefore a failure here must FAIL the op
	// (rather than log-and-succeed), or stale bytes could remain readable in the
	// punched range while the client is told NFS4_OK. engine.PunchHole reaps CAS
	// blocks fully inside the range and zero-writes [offset, offset+length) so
	// both the pre-rollup and CAS read paths return zeros.
	if res.PayloadID != "" && length > 0 && offset < res.File.Size {
		if h.Registry == nil {
			logger.Error("NFSv4.2 DEALLOCATE: no registry configured", "handle", string(handle))
			return deallocErr(types.NFS4ERR_SERVERFAULT)
		}
		blockStore, bsErr := common.ResolveForWrite(ctx.Context, h.Registry, handle)
		if bsErr != nil {
			logger.Error("NFSv4.2 DEALLOCATE: cannot resolve block store",
				"handle", string(handle), "error", bsErr)
			return deallocErr(types.NFS4ERR_SERVERFAULT)
		}
		if _, pErr := blockStore.PunchHole(ctx.Context, string(res.PayloadID), res.PreOpBlocks, offset, punchLen(offset, length, res.File.Size)); pErr != nil {
			logger.Error("NFSv4.2 DEALLOCATE: block store punch failed",
				"handle", string(handle), "error", pErr)
			return deallocErr(types.NFS4ERR_IO)
		}
	}

	logger.Debug("NFSv4.2 DEALLOCATE", "offset", offset, "length", length,
		"size", res.File.Size, "client", ctx.ClientAddr)

	return &types.CompoundResult{Status: types.NFS4_OK, OpCode: types.OP_DEALLOCATE, Data: encodeStatusOnly(types.NFS4_OK)}
}

// decodeAllocArgs decodes the shared (stateid, offset, length) argument tuple of
// ALLOCATE and DEALLOCATE. Returns NFS4ERR_BADXDR on a malformed stream and
// NFS4ERR_INVAL when offset+length overflows uint64.
func decodeAllocArgs(reader io.Reader) (*types.Stateid4, uint64, uint64, uint32) {
	stateid, err := types.DecodeStateid4(reader)
	if err != nil {
		return nil, 0, 0, types.NFS4ERR_BADXDR
	}
	offset, err := xdr.DecodeUint64(reader)
	if err != nil {
		return nil, 0, 0, types.NFS4ERR_BADXDR
	}
	length, err := xdr.DecodeUint64(reader)
	if err != nil {
		return nil, 0, 0, types.NFS4ERR_BADXDR
	}
	if length > 0 && offset > ^uint64(0)-length {
		return nil, 0, 0, types.NFS4ERR_INVAL
	}
	return stateid, offset, length, types.NFS4_OK
}

// punchLen clamps a DEALLOCATE length so the zero-overwrite never extends past
// EOF (zeroing beyond the logical size would needlessly grow the payload). The
// caller guarantees offset < size.
func punchLen(offset, length, size uint64) uint64 {
	end := offset + length
	if end > size {
		end = size
	}
	return end - offset
}

// deallocErr builds a DEALLOCATE error result (status only).
func deallocErr(status uint32) *types.CompoundResult {
	return &types.CompoundResult{
		Status: status,
		OpCode: types.OP_DEALLOCATE,
		Data:   encodeStatusOnly(status),
	}
}
