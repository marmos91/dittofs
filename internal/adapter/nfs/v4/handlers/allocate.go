package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ALLOCATE4args / ALLOCATE4res (RFC 7862 Section 15.1):
//
//	struct ALLOCATE4args {
//	    stateid4 aa_stateid;
//	    offset4  aa_offset;
//	    length4  aa_length;
//	};
//	union ALLOCATE4res switch (nfsstat4 ar_status) {
//	 case NFS4_OK: void;
//	 default:      void;
//	};
//
// ALLOCATE guarantees [aa_offset, aa_offset+aa_length) is readable, growing the
// file's logical size when the range extends past EOF.
//
// Reservation semantics (DECISION): DittoFS is thin-provisioned over a
// content-addressed/dedup block store and optional S3 backend, so true physical
// space reservation is not possible (and would be meaningless under dedup). RFC
// 7862 permits a server to satisfy ALLOCATE without a physical reservation, so
// DittoFS provides BEST-EFFORT / LOGICAL preallocation: the requested range is
// guaranteed readable (newly covered bytes are a sparse hole that reads as
// zeros) and the file size grows to cover it. No NFS4ERR_NOSPC pre-reservation
// is attempted; out-of-space surfaces on the eventual WRITE, exactly as for an
// ordinary sparse file. This is documented in docs/FAQ.md and docs/NFS.md.
func (h *Handler) handleAllocate(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return allocErr(status)
	}
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return allocErr(types.NFS4ERR_ROFS)
	}

	stateid, offset, length, st := decodeAllocArgs(reader)
	if st != types.NFS4_OK {
		return allocErr(st)
	}

	// ALLOCATE may change the file size: validate the stateid as a write op and
	// require WRITE share-access on a real open stateid (special stateids pass).
	if openState, stateErr := h.StateManager.ValidateStateid(stateid, ctx.CurrentFH, state.StateidOpWrite); stateErr != nil {
		s := mapStateError(stateErr)
		logger.Debug("NFSv4.2 ALLOCATE stateid validation failed", "error", stateErr, "nfs_status", s, "client", ctx.ClientAddr)
		return allocErr(s)
	} else if openState != nil && openState.ShareAccess&types.OPEN4_SHARE_ACCESS_WRITE == 0 {
		return allocErr(types.NFS4ERR_OPENMODE)
	}

	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		return allocErr(nfs4StatusForAuthError(err))
	}
	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return allocErr(types.NFS4ERR_SERVERFAULT)
	}

	if _, err := metaSvc.Allocate(authCtx, metadata.FileHandle(ctx.CurrentFH), offset, length); err != nil {
		return allocErr(common.MapToNFS4(err))
	}

	logger.Debug("NFSv4.2 ALLOCATE", "offset", offset, "length", length, "client", ctx.ClientAddr)
	return &types.CompoundResult{Status: types.NFS4_OK, OpCode: types.OP_ALLOCATE, Data: encodeStatusOnly(types.NFS4_OK)}
}

// allocErr builds an ALLOCATE error result (status only).
func allocErr(status uint32) *types.CompoundResult {
	return &types.CompoundResult{
		Status: status,
		OpCode: types.OP_ALLOCATE,
		Data:   encodeStatusOnly(status),
	}
}
