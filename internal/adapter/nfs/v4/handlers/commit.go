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

// handleCommit implements the COMMIT operation (RFC 7530 Section 16.5).
//
// COMMIT flushes unstable writes to stable storage via PayloadService.Flush
// and returns the same server boot verifier as WRITE.
//
// Wire format args (COMMIT4args):
//
//	uint64    offset  (starting offset, 0 = all)
//	uint32    count   (bytes to commit, 0 = all from offset to EOF)
//
// Wire format res (success - COMMIT4resok):
//
//	nfsstat4  status    (NFS4_OK)
//	opaque    writeverf[NFS4_VERIFIER_SIZE] (8 bytes, fixed-length)
func (h *Handler) handleCommit(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_COMMIT,
			Data:   encodeStatusOnly(status),
		}
	}

	// Pseudo-fs is read-only
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return &types.CompoundResult{
			Status: types.NFS4ERR_ROFS,
			OpCode: types.OP_COMMIT,
			Data:   encodeStatusOnly(types.NFS4ERR_ROFS),
		}
	}

	// Decode COMMIT4args
	offset, err := xdr.DecodeUint64(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_COMMIT,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	count, err := xdr.DecodeUint32(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_COMMIT,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	logger.Debug("NFSv4 COMMIT",
		"offset", offset,
		"count", count,
		"client", ctx.ClientAddr)

	// Build auth context
	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		logger.Debug("NFSv4 COMMIT auth context failed", "error", err, "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_COMMIT,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	// Get metadata service to look up PayloadID
	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_COMMIT,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	fileHandle := metadata.FileHandle(ctx.CurrentFH)
	file, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		status := types.MapMetadataErrorToNFS4(err)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_COMMIT,
			Data:   encodeStatusOnly(status),
		}
	}

	// If no content, just return success
	if file.PayloadID == "" {
		logger.Debug("NFSv4 COMMIT: no content to flush", "client", ctx.ClientAddr)
		return encodeCommit4resok()
	}

	// Get payload service and flush
	payloadSvc, err := getPayloadServiceForCtx(h)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_COMMIT,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	_, flushErr := payloadSvc.Flush(authCtx.Context, file.PayloadID)
	if flushErr != nil {
		logger.Debug("NFSv4 COMMIT flush failed",
			"error", flushErr,
			"payloadID", file.PayloadID,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_IO,
			OpCode: types.OP_COMMIT,
			Data:   encodeStatusOnly(types.NFS4ERR_IO),
		}
	}

	// Flush pending metadata writes (deferred commit optimization)
	// This is critical - without this, file size and other metadata changes
	// are not persisted to the store, causing data loss on server restart.
	flushed, metaErr := metaSvc.FlushPendingWriteForFile(authCtx, fileHandle)
	if metaErr != nil {
		logger.Error("NFSv4 COMMIT metadata flush failed",
			"error", metaErr,
			"client", ctx.ClientAddr)
		return encodeCommit4resError(types.NFS4ERR_SERVERFAULT)
	} else if flushed {
		logger.Debug("NFSv4 COMMIT flushed pending metadata",
			"client", ctx.ClientAddr)
	}

	logger.Debug("NFSv4 COMMIT successful",
		"payloadID", file.PayloadID,
		"client", ctx.ClientAddr)

	return encodeCommit4resok()
}

// encodeCommit4resError encodes a failed COMMIT4 response.
func encodeCommit4resError(status uint32) *types.CompoundResult {
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, status)
	return &types.CompoundResult{
		Status: status,
		OpCode: types.OP_COMMIT,
		Data:   buf.Bytes(),
	}
}

// encodeCommit4resok encodes a successful COMMIT4 response.
func encodeCommit4resok() *types.CompoundResult {
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)

	// writeverf: 8-byte server boot verifier (fixed-length, NOT XDR opaque)
	buf.Write(serverBootVerifier[:])

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_COMMIT,
		Data:   buf.Bytes(),
	}
}
