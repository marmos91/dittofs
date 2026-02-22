package handlers

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/attrs"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// handleSetAttr implements the SETATTR operation (RFC 7530 Section 16.32).
//
// SETATTR modifies file attributes (mode, owner, group, size, timestamps).
// It requires a stateid for size changes (Phase 9 validates; Phase 8 accepts
// any special stateid).
//
// Wire format args (SETATTR4args):
//
//	stateid4    stateid       (state identifier for size changes)
//	fattr4      obj_attributes (attribute values to set)
//
// Wire format res (SETATTR4res):
//
//	nfsstat4    status
//	bitmap4     attrsset      (attributes actually set; empty on error)
func (h *Handler) handleSetAttr(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// 1. Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_SETATTR,
			Data:   encodeSetAttrError(status),
		}
	}

	// 2. Pseudo-fs is read-only
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return &types.CompoundResult{
			Status: types.NFS4ERR_ROFS,
			OpCode: types.OP_SETATTR,
			Data:   encodeSetAttrError(types.NFS4ERR_ROFS),
		}
	}

	// 3. Read stateid4 (16 bytes: uint32 seqid + [12]byte other)
	stateid, err := types.DecodeStateid4(reader)
	if err != nil {
		logger.Error("NFSv4 SETATTR decode stateid failed", "error", err)
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_SETATTR,
			Data:   encodeSetAttrError(types.NFS4ERR_BADXDR),
		}
	}

	// Log stateid at debug level (Phase 9 validates; Phase 8 accepts any)
	logger.Debug("NFSv4 SETATTR stateid",
		"seqid", stateid.Seqid,
		"special", stateid.IsSpecialStateid(),
		"handle", string(ctx.CurrentFH))

	// 4. Decode fattr4 (bitmap + attr_vals) -> SetAttrs
	setAttrs, requestedBitmap, err := attrs.DecodeFattr4ToSetAttrs(reader)
	if err != nil {
		// Check for typed NFS4 error
		if nfsErr, ok := err.(attrs.NFS4StatusError); ok {
			status := nfsErr.NFS4Status()
			logger.Debug("NFSv4 SETATTR decode attrs error",
				"error", err,
				"nfs4status", status)
			return &types.CompoundResult{
				Status: status,
				OpCode: types.OP_SETATTR,
				Data:   encodeSetAttrError(status),
			}
		}
		logger.Error("NFSv4 SETATTR decode attrs failed", "error", err)
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_SETATTR,
			Data:   encodeSetAttrError(types.NFS4ERR_BADXDR),
		}
	}

	// 5. Build auth context
	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		logger.Error("NFSv4 SETATTR build auth context failed", "error", err)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_SETATTR,
			Data:   encodeSetAttrError(types.NFS4ERR_SERVERFAULT),
		}
	}

	// Trace mode changes for debugging SUID/SGID operations
	if setAttrs.Mode != nil {
		logger.Debug("NFSv4 SETATTR mode change",
			"new_mode", fmt.Sprintf("0%o", *setAttrs.Mode),
			"uid", authCtx.Identity.UID,
			"handle", string(ctx.CurrentFH),
			"client", ctx.ClientAddr)
	}

	// 6. Get MetadataService
	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		logger.Error("NFSv4 SETATTR get metadata service failed", "error", err)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_SETATTR,
			Data:   encodeSetAttrError(types.NFS4ERR_SERVERFAULT),
		}
	}

	// 7. Apply attributes via MetadataService (all-or-nothing semantics)
	if err := metaSvc.SetFileAttributes(authCtx, metadata.FileHandle(ctx.CurrentFH), setAttrs); err != nil {
		nfsStatus := types.MapMetadataErrorToNFS4(err)
		logger.Debug("NFSv4 SETATTR failed",
			"error", err,
			"nfs4status", nfsStatus,
			"mode_requested", setAttrs.Mode,
			"uid", authCtx.Identity.UID,
			"handle", string(ctx.CurrentFH),
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_SETATTR,
			Data:   encodeSetAttrError(nfsStatus),
		}
	}

	// 8. Notify directory delegation holders about significant attribute changes
	if h.StateManager != nil && isSignificantAttrChange(requestedBitmap) {
		// Check if the target is a directory by inspecting the file
		file, getErr := metaSvc.GetFile(ctx.Context, metadata.FileHandle(ctx.CurrentFH))
		if getErr == nil && file != nil && file.Type == metadata.FileTypeDirectory {
			h.StateManager.NotifyDirChange(ctx.CurrentFH, state.DirNotification{
				Type: types.NOTIFY4_CHANGE_DIR_ATTRS,
			})
		}
	}

	// 9. Success: encode response with attrsset bitmap
	logger.Debug("NFSv4 SETATTR success",
		"handle", string(ctx.CurrentFH))

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_SETATTR,
		Data:   encodeSetAttrSuccess(requestedBitmap),
	}
}

// isSignificantAttrChange returns true if the attribute bitmap contains
// significant changes (mode, owner, owner_group, size) that warrant
// directory delegation notifications. Ignores atime-only and ctime-only
// changes as they are too noisy.
func isSignificantAttrChange(bitmap []uint32) bool {
	return attrs.IsBitSet(bitmap, attrs.FATTR4_MODE) ||
		attrs.IsBitSet(bitmap, attrs.FATTR4_OWNER) ||
		attrs.IsBitSet(bitmap, attrs.FATTR4_OWNER_GROUP) ||
		attrs.IsBitSet(bitmap, attrs.FATTR4_SIZE)
}

// encodeSetAttrSuccess encodes a successful SETATTR response.
// Response format: status (NFS4_OK) + attrsset bitmap
func encodeSetAttrSuccess(attrsset []uint32) []byte {
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	_ = attrs.EncodeBitmap4(&buf, attrsset)
	return buf.Bytes()
}

// encodeSetAttrError encodes a failed SETATTR response.
// Response format: status + empty attrsset bitmap (0 words)
func encodeSetAttrError(status uint32) []byte {
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, status)
	// Empty bitmap: 0 words
	_ = attrs.EncodeBitmap4(&buf, nil)
	return buf.Bytes()
}
