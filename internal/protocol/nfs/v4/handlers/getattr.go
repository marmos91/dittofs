package handlers

import (
	"bytes"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/attrs"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// handleGetAttr implements the GETATTR operation (RFC 7530 Section 16.9).
//
// GETATTR returns requested attributes for the current filehandle.
// For pseudo-fs handles, it encodes attributes of the virtual namespace node.
// Clients specify which attributes they want via a bitmap4 request mask.
//
// Wire format args: attr_request (bitmap4)
// Wire format res:  nfsstat4 (uint32) + obj_attributes (fattr4: bitmap4 + opaque attrvals)
func (h *Handler) handleGetAttr(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_GETATTR,
			Data:   encodeStatusOnly(status),
		}
	}

	// Read requested attributes bitmap
	requested, err := attrs.DecodeBitmap4(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_GETATTR,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Ensure lease_time reflects configured duration from StateManager
	if h.StateManager != nil {
		leaseDur := h.StateManager.LeaseDuration()
		attrs.SetLeaseTime(uint32(leaseDur.Seconds()))
	}

	// Check if current FH is a pseudo-fs handle
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return h.getAttrPseudoFS(ctx, requested)
	}

	// Real filesystem handle -- get attributes from metadata service
	return h.getAttrRealFS(ctx, requested)
}

// getAttrRealFS handles GETATTR for real filesystem files.
func (h *Handler) getAttrRealFS(ctx *types.CompoundContext, requested []uint32) *types.CompoundResult {
	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_GETATTR,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_GETATTR,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	file, err := metaSvc.GetFile(authCtx.Context, metadata.FileHandle(ctx.CurrentFH))
	if err != nil {
		status := types.MapMetadataErrorToNFS4(err)
		logger.Debug("NFSv4 GETATTR real-FS failed",
			"error", err,
			"status", status,
			"handle_len", len(ctx.CurrentFH),
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_GETATTR,
			Data:   encodeStatusOnly(status),
		}
	}

	logger.Debug("NFSv4 GETATTR real-FS",
		"path", file.Path,
		"mode", fmt.Sprintf("0%o", file.Mode),
		"uid", file.UID,
		"gid", file.GID,
		"type", file.Type,
		"size", file.Size,
		"client", ctx.ClientAddr)

	// Encode response: status + fattr4 (bitmap + opaque attr values)
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)

	if err := attrs.EncodeRealFileAttrs(&buf, requested, file, metadata.FileHandle(ctx.CurrentFH)); err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_GETATTR,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_GETATTR,
		Data:   buf.Bytes(),
	}
}

// getAttrPseudoFS handles GETATTR for pseudo-fs nodes.
func (h *Handler) getAttrPseudoFS(ctx *types.CompoundContext, requested []uint32) *types.CompoundResult {
	// Find the node by handle
	node, ok := h.PseudoFS.LookupByHandle(ctx.CurrentFH)
	if !ok {
		return &types.CompoundResult{
			Status: types.NFS4ERR_STALE,
			OpCode: types.OP_GETATTR,
			Data:   encodeStatusOnly(types.NFS4ERR_STALE),
		}
	}

	// Encode response: status + fattr4 (bitmap + opaque attr values)
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)

	// Encode pseudo-fs attributes
	if err := attrs.EncodePseudoFSAttrs(&buf, requested, node); err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_GETATTR,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_GETATTR,
		Data:   buf.Bytes(),
	}
}
