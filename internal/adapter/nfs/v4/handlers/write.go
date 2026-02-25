package handlers

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// serverBootVerifier is an 8-byte verifier derived from server boot time.
// Clients compare this value across WRITE and COMMIT responses to detect
// server restarts, at which point they must re-send unstable writes.
var serverBootVerifier [8]byte

func init() {
	binary.BigEndian.PutUint64(serverBootVerifier[:], uint64(time.Now().UnixNano()))
}

// handleWrite implements the WRITE operation (RFC 7530 Section 16.36).
// Writes data to a file using two-phase PrepareWrite/CommitWrite pattern with cache-backed I/O.
// Delegates to MetadataService.PrepareWrite+CommitWrite and PayloadService.WriteAt.
// Updates file size/timestamps via metadata; writes data to cache (flushed on COMMIT); always returns UNSTABLE4.
// Errors: NFS4ERR_NOFILEHANDLE, NFS4ERR_ISDIR, NFS4ERR_FBIG, NFS4ERR_NOSPC, NFS4ERR_IO.
func (h *Handler) handleWrite(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_WRITE,
			Data:   encodeStatusOnly(status),
		}
	}

	// Pseudo-fs is read-only
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return &types.CompoundResult{
			Status: types.NFS4ERR_ROFS,
			OpCode: types.OP_WRITE,
			Data:   encodeStatusOnly(types.NFS4ERR_ROFS),
		}
	}

	// Decode WRITE4args
	stateid, err := types.DecodeStateid4(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_WRITE,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	offset, err := xdr.DecodeUint64(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_WRITE,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	stable, err := xdr.DecodeUint32(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_WRITE,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	data, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_WRITE,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Validate stateid via StateManager
	// Special stateids (all-zeros, all-ones) bypass validation.
	// Real stateids are validated for correctness (seqid, epoch, filehandle match).
	// Implicit lease renewal happens inside ValidateStateid for real stateids.
	openState, stateErr := h.StateManager.ValidateStateid(stateid, ctx.CurrentFH)
	if stateErr != nil {
		nfsStatus := mapStateError(stateErr)
		logger.Debug("NFSv4 WRITE stateid validation failed",
			"error", stateErr,
			"nfs_status", nfsStatus,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_WRITE,
			Data:   encodeStatusOnly(nfsStatus),
		}
	}

	// Check that the open state includes WRITE access (OPEN4_SHARE_ACCESS_WRITE or BOTH).
	// Special stateids (openState == nil) bypass this check.
	if openState != nil {
		if openState.ShareAccess&types.OPEN4_SHARE_ACCESS_WRITE == 0 {
			logger.Debug("NFSv4 WRITE rejected: read-only open",
				"share_access", openState.ShareAccess,
				"client", ctx.ClientAddr)
			return &types.CompoundResult{
				Status: types.NFS4ERR_OPENMODE,
				OpCode: types.OP_WRITE,
				Data:   encodeStatusOnly(types.NFS4ERR_OPENMODE),
			}
		}
	}

	logger.Debug("NFSv4 WRITE",
		"offset", offset,
		"count", len(data),
		"stable", stable,
		"stateid_seqid", stateid.Seqid,
		"client", ctx.ClientAddr)

	// Build auth context
	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		logger.Debug("NFSv4 WRITE auth context failed", "error", err, "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_WRITE,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	// Get services
	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_WRITE,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	payloadSvc, err := getPayloadServiceForCtx(h)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_WRITE,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	// Calculate new size with overflow check
	newSize := offset + uint64(len(data))
	if newSize < offset {
		// Overflow
		return &types.CompoundResult{
			Status: types.NFS4ERR_FBIG,
			OpCode: types.OP_WRITE,
			Data:   encodeStatusOnly(types.NFS4ERR_FBIG),
		}
	}

	fileHandle := metadata.FileHandle(ctx.CurrentFH)

	// Phase 1: PrepareWrite -- validates permissions, returns intent
	intent, err := metaSvc.PrepareWrite(authCtx, fileHandle, newSize)
	if err != nil {
		status := types.MapMetadataErrorToNFS4(err)
		logger.Debug("NFSv4 WRITE PrepareWrite failed",
			"error", err,
			"status", status,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_WRITE,
			Data:   encodeStatusOnly(status),
		}
	}

	// Trace SUID/SGID-related writes for debugging
	if intent.PreWriteAttr.Mode&0o6000 != 0 {
		uid := uint32(0)
		if authCtx.Identity != nil && authCtx.Identity.UID != nil {
			uid = *authCtx.Identity.UID
		}
		logger.Debug("NFSv4 WRITE to SUID/SGID file",
			"pre_mode", fmt.Sprintf("0%o", intent.PreWriteAttr.Mode),
			"uid", uid,
			"offset", offset,
			"count", len(data),
			"client", ctx.ClientAddr)
	}

	// Phase 2: Write actual data via PayloadService
	err = payloadSvc.WriteAt(ctx.Context, intent.PayloadID, data, offset)
	if err != nil {
		logger.Debug("NFSv4 WRITE payload error",
			"error", err,
			"payloadID", intent.PayloadID,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_IO,
			OpCode: types.OP_WRITE,
			Data:   encodeStatusOnly(types.NFS4ERR_IO),
		}
	}

	// Phase 3: CommitWrite -- updates metadata (size, timestamps)
	_, err = metaSvc.CommitWrite(authCtx, intent)
	if err != nil {
		logger.Debug("NFSv4 WRITE CommitWrite failed",
			"error", err,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_IO,
			OpCode: types.OP_WRITE,
			Data:   encodeStatusOnly(types.NFS4ERR_IO),
		}
	}

	logger.Debug("NFSv4 WRITE successful",
		"offset", offset,
		"written", len(data),
		"newSize", newSize,
		"client", ctx.ClientAddr)

	// Encode WRITE4resok
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)

	// count (uint32): bytes written
	_ = xdr.WriteUint32(&buf, uint32(len(data)))

	// committed: always UNSTABLE4 (cache is always enabled)
	_ = xdr.WriteUint32(&buf, types.UNSTABLE4)

	// writeverf: 8-byte server boot verifier (fixed-length, NOT XDR opaque)
	buf.Write(serverBootVerifier[:])

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_WRITE,
		Data:   buf.Bytes(),
	}
}
