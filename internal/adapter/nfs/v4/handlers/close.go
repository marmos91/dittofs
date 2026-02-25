package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// handleClose implements the CLOSE operation (RFC 7530 Section 16.3).
// Releases open state for a file, removing the stateid from StateManager tracking.
// Delegates to StateManager.CloseFile; triggers PayloadService.Flush for dirty data.
// Removes open/lock state and flushes cached writes; does NOT change the current filehandle.
// Errors: NFS4ERR_NOFILEHANDLE, NFS4ERR_BAD_STATEID, NFS4ERR_OLD_STATEID, NFS4ERR_BADXDR.
func (h *Handler) handleClose(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_CLOSE,
			Data:   encodeStatusOnly(status),
		}
	}

	// Pseudo-fs handles are not files that can be opened/closed
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return &types.CompoundResult{
			Status: types.NFS4ERR_INVAL,
			OpCode: types.OP_CLOSE,
			Data:   encodeStatusOnly(types.NFS4ERR_INVAL),
		}
	}

	// Decode CLOSE4args: seqid (uint32) + stateid4
	closeSeqid, err := xdr.DecodeUint32(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_CLOSE,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}
	// In NFSv4.1, per-owner seqid is obsoleted by the slot table (SEQUENCE).
	if ctx.SkipOwnerSeqid {
		closeSeqid = 0
	}

	stateid, err := types.DecodeStateid4(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_CLOSE,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	logger.Debug("NFSv4 CLOSE",
		"seqid", closeSeqid,
		"stateid_seqid", stateid.Seqid,
		"client", ctx.ClientAddr)

	// Delegate to StateManager for state cleanup
	closedStateid, stateErr := h.StateManager.CloseFile(stateid, closeSeqid)
	if stateErr != nil {
		nfsStatus := mapStateError(stateErr)
		logger.Debug("NFSv4 CLOSE failed",
			"error", stateErr,
			"nfs_status", nfsStatus,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_CLOSE,
			Data:   encodeStatusOnly(nfsStatus),
		}
	}

	// Flush pending metadata writes to persist file size and other changes,
	// even if the client doesn't send COMMIT.
	if authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH); err != nil {
		logger.Warn("NFSv4 CLOSE: failed to build auth context for flush",
			"error", err, "client", ctx.ClientAddr)
	} else if metaSvc, metaErr := getMetadataServiceForCtx(h); metaErr != nil {
		logger.Warn("NFSv4 CLOSE: metadata service unavailable for flush",
			"error", metaErr, "client", ctx.ClientAddr)
	} else {
		fileHandle := metadata.FileHandle(ctx.CurrentFH)
		flushed, flushErr := metaSvc.FlushPendingWriteForFile(authCtx, fileHandle)
		if flushErr != nil {
			logger.Warn("NFSv4 CLOSE metadata flush failed",
				"error", flushErr, "client", ctx.ClientAddr)
		} else if flushed {
			logger.Debug("NFSv4 CLOSE flushed pending metadata",
				"client", ctx.ClientAddr)
		}
	}

	// NOTE: CLOSE does NOT clear ctx.CurrentFH per RFC 7530

	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	types.EncodeStateid4(&buf, closedStateid)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_CLOSE,
		Data:   buf.Bytes(),
	}
}
