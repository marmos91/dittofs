package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// handleClose implements the CLOSE operation (RFC 7530 Section 16.3).
//
// CLOSE releases the open state associated with a stateid via
// StateManager.CloseFile. The state is removed from all tracking maps
// and a zeroed stateid is returned.
//
// Per RFC 7530, CLOSE does NOT change the current filehandle.
//
// Wire format args (CLOSE4args):
//
//	uint32    seqid
//	stateid4  open_stateid
//
// Wire format res (success - CLOSE4res):
//
//	nfsstat4  status (NFS4_OK)
//	stateid4  open_stateid (zeroed out)
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

	// Flush pending metadata writes (deferred commit optimization)
	// This ensures file size and other metadata changes are persisted
	// when the file is closed, even if the client doesn't send COMMIT.
	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		logger.Warn("NFSv4 CLOSE: buildV4AuthContext failed, skipping flush",
			"error", err,
			"client", ctx.ClientAddr)
	} else {
		metaSvc, metaErr := getMetadataServiceForCtx(h)
		if metaErr != nil {
			logger.Warn("NFSv4 CLOSE: getMetadataServiceForCtx failed, skipping flush",
				"error", metaErr,
				"client", ctx.ClientAddr)
		} else {
			fileHandle := metadata.FileHandle(ctx.CurrentFH)
			flushed, flushErr := metaSvc.FlushPendingWriteForFile(authCtx, fileHandle)
			if flushErr != nil {
				logger.Warn("NFSv4 CLOSE metadata flush failed",
					"error", flushErr,
					"client", ctx.ClientAddr)
				// Continue - state is closed, metadata will be fixed eventually
			} else if flushed {
				logger.Info("NFSv4 CLOSE flushed pending metadata",
					"client", ctx.ClientAddr)
			} else {
				logger.Debug("NFSv4 CLOSE no pending metadata to flush",
					"client", ctx.ClientAddr)
			}
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
