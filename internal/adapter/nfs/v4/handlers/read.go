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

// handleRead implements the READ operation (RFC 7530 Section 16.25).
// Reads file data at a given offset, returning bytes and EOF flag.
// Delegates to PayloadService.ReadAt via pooled buffers; validates stateid for open state.
// No side effects; read-only data operation using buffer pools to reduce GC pressure.
// Errors: NFS4ERR_NOFILEHANDLE, NFS4ERR_ISDIR, NFS4ERR_STALE, NFS4ERR_IO, NFS4ERR_BADXDR.
func (h *Handler) handleRead(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_READ,
			Data:   encodeStatusOnly(status),
		}
	}

	// Pseudo-fs only has directories
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return &types.CompoundResult{
			Status: types.NFS4ERR_ISDIR,
			OpCode: types.OP_READ,
			Data:   encodeStatusOnly(types.NFS4ERR_ISDIR),
		}
	}

	// Decode READ4args
	stateid, err := types.DecodeStateid4(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_READ,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	offset, err := xdr.DecodeUint64(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_READ,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	count, err := xdr.DecodeUint32(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_READ,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Validate stateid via StateManager
	// Special stateids (all-zeros, all-ones) bypass validation.
	// Real stateids are validated for correctness (seqid, epoch, filehandle match).
	if _, stateErr := h.StateManager.ValidateStateid(stateid, ctx.CurrentFH); stateErr != nil {
		nfsStatus := mapStateError(stateErr)
		logger.Debug("NFSv4 READ stateid validation failed",
			"error", stateErr,
			"nfs_status", nfsStatus,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_READ,
			Data:   encodeStatusOnly(nfsStatus),
		}
	}

	logger.Debug("NFSv4 READ",
		"offset", offset,
		"count", count,
		"stateid_seqid", stateid.Seqid,
		"special", stateid.IsSpecialStateid(),
		"client", ctx.ClientAddr)

	// Build auth context
	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		logger.Debug("NFSv4 READ auth context failed", "error", err, "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_READ,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	// Get services
	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_READ,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	// Get file attributes
	fileHandle := metadata.FileHandle(ctx.CurrentFH)
	file, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		status := types.MapMetadataErrorToNFS4(err)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_READ,
			Data:   encodeStatusOnly(status),
		}
	}

	// Verify it's a regular file
	if file.Type != metadata.FileTypeRegular {
		return &types.CompoundResult{
			Status: types.NFS4ERR_ISDIR,
			OpCode: types.OP_READ,
			Data:   encodeStatusOnly(types.NFS4ERR_ISDIR),
		}
	}

	// Empty file or no content
	if file.Size == 0 || file.PayloadID == "" {
		return encodeRead4resok(true, nil)
	}

	// Offset beyond EOF
	if offset >= file.Size {
		return encodeRead4resok(true, nil)
	}

	// Clamp read length
	actualLen := uint64(count)
	if offset+actualLen > file.Size {
		actualLen = file.Size - offset
	}

	// Get payload service and read data
	payloadSvc, err := getPayloadServiceForCtx(h)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_READ,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	data := make([]byte, actualLen)
	var n int

	// Check for COW source
	if file.COWSourcePayloadID != "" {
		n, err = payloadSvc.ReadAtWithCOWSource(ctx.Context, file.PayloadID, file.COWSourcePayloadID, data, offset)
	} else {
		n, err = payloadSvc.ReadAt(ctx.Context, file.PayloadID, data, offset)
	}

	if err != nil {
		logger.Debug("NFSv4 READ payload error", "error", err, "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_IO,
			OpCode: types.OP_READ,
			Data:   encodeStatusOnly(types.NFS4ERR_IO),
		}
	}

	// Detect EOF
	eof := offset+uint64(n) >= file.Size

	logger.Debug("NFSv4 READ successful",
		"offset", offset,
		"requested", count,
		"read", n,
		"eof", eof,
		"client", ctx.ClientAddr)

	return encodeRead4resok(eof, data[:n])
}

// encodeRead4resok encodes a successful READ4 response.
func encodeRead4resok(eof bool, data []byte) *types.CompoundResult {
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	_ = xdr.WriteBool(&buf, eof)

	if data == nil {
		data = []byte{}
	}
	_ = xdr.WriteXDROpaque(&buf, data)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_READ,
		Data:   buf.Bytes(),
	}
}
