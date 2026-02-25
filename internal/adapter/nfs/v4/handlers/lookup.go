package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// handleLookup implements the LOOKUP operation (RFC 7530 Section 16.15).
//
// LOOKUP traverses a directory by name, setting the current filehandle to the
// child entry. For pseudo-fs handles, it navigates the virtual namespace tree.
// When a LOOKUP resolves to an export junction point, it crosses into the real
// share by obtaining the share's root handle from the runtime.
//
// Wire format args: component name (XDR string: uint32 length + bytes + padding)
// Wire format res:  nfsstat4 (uint32)
func (h *Handler) handleLookup(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_LOOKUP,
			Data:   encodeStatusOnly(status),
		}
	}

	// Read component name from XDR
	name, err := xdr.DecodeString(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_LOOKUP,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Validate UTF-8 filename
	if status := types.ValidateUTF8Filename(name); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_LOOKUP,
			Data:   encodeStatusOnly(status),
		}
	}

	// Check if current FH is a pseudo-fs handle
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return h.lookupInPseudoFS(ctx, name)
	}

	// Real filesystem handle -- resolve name in real directory
	return h.lookupInRealFS(ctx, name)
}

// lookupInRealFS handles LOOKUP within a real filesystem directory.
func (h *Handler) lookupInRealFS(ctx *types.CompoundContext, name string) *types.CompoundResult {
	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		logger.Debug("NFSv4 LOOKUP real-FS auth context failed",
			"error", err,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_LOOKUP,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_LOOKUP,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	child, err := metaSvc.Lookup(authCtx, metadata.FileHandle(ctx.CurrentFH), name)
	if err != nil {
		status := types.MapMetadataErrorToNFS4(err)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_LOOKUP,
			Data:   encodeStatusOnly(status),
		}
	}

	// Encode the child's file handle
	childHandle, err := metadata.EncodeFileHandle(child)
	if err != nil {
		logger.Debug("NFSv4 LOOKUP real-FS encode handle failed",
			"error", err,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_LOOKUP,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	// Copy-on-set the result into ctx.CurrentFH
	ctx.CurrentFH = make([]byte, len(childHandle))
	copy(ctx.CurrentFH, childHandle)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_LOOKUP,
		Data:   encodeStatusOnly(types.NFS4_OK),
	}
}

// lookupInPseudoFS handles LOOKUP within the pseudo-filesystem.
func (h *Handler) lookupInPseudoFS(ctx *types.CompoundContext, name string) *types.CompoundResult {
	// Find the parent node by handle
	node, ok := h.PseudoFS.LookupByHandle(ctx.CurrentFH)
	if !ok {
		return &types.CompoundResult{
			Status: types.NFS4ERR_STALE,
			OpCode: types.OP_LOOKUP,
			Data:   encodeStatusOnly(types.NFS4ERR_STALE),
		}
	}

	// Look up child by name
	child, ok := h.PseudoFS.LookupChild(node, name)
	if !ok {
		return &types.CompoundResult{
			Status: types.NFS4ERR_NOENT,
			OpCode: types.OP_LOOKUP,
			Data:   encodeStatusOnly(types.NFS4ERR_NOENT),
		}
	}

	// Check for export junction crossing
	if child.IsExport && h.Registry != nil {
		// Get the real share root handle from runtime
		realHandle, err := h.Registry.GetRootHandle(child.ShareName)
		if err != nil {
			logger.Debug("NFSv4 LOOKUP junction crossing failed",
				"share", child.ShareName,
				"error", err,
				"client", ctx.ClientAddr)
			// If the share is configured but not yet loaded, return NOENT
			return &types.CompoundResult{
				Status: types.NFS4ERR_NOENT,
				OpCode: types.OP_LOOKUP,
				Data:   encodeStatusOnly(types.NFS4ERR_NOENT),
			}
		}

		// Set current FH to the real share root handle
		ctx.CurrentFH = make([]byte, len(realHandle))
		copy(ctx.CurrentFH, realHandle)

		logger.Debug("NFSv4 LOOKUP crossed junction to real share",
			"share", child.ShareName,
			"client", ctx.ClientAddr)

		return &types.CompoundResult{
			Status: types.NFS4_OK,
			OpCode: types.OP_LOOKUP,
			Data:   encodeStatusOnly(types.NFS4_OK),
		}
	}

	// Stay in pseudo-fs: set current FH to child's handle
	ctx.CurrentFH = make([]byte, len(child.Handle))
	copy(ctx.CurrentFH, child.Handle)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_LOOKUP,
		Data:   encodeStatusOnly(types.NFS4_OK),
	}
}
