package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// handleLookup implements the LOOKUP operation (RFC 7530 Section 16.15).
// Traverses a directory by name, setting the current filehandle to the resolved child.
// Delegates to MetadataService.GetChild for real files; navigates pseudo-fs tree for virtual handles.
// Sets CurrentFH to the child entry; crosses export junctions into real shares transparently.
// Errors: NFS4ERR_NOFILEHANDLE, NFS4ERR_NOENT, NFS4ERR_NOTDIR, NFS4ERR_BADXDR, NFS4ERR_STALE.
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
		st := nfs4StatusForAuthError(err)
		return &types.CompoundResult{
			Status: st,
			OpCode: types.OP_LOOKUP,
			Data:   encodeStatusOnly(st),
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
		status := types.StatusForErr(err)
		// The store reports every non-directory the same way, but RFC 7530
		// Section 16.15.4 separates a symbolic link out as NFS4ERR_SYMLINK so
		// the client knows to resolve it rather than give up on the path.
		if status == types.NFS4ERR_NOTDIR {
			if fileType, st := h.fileTypeForHandle(ctx, ctx.CurrentFH); st == types.NFS4_OK {
				// Only ever narrows one failure into a more precise one. A
				// directory maps to NFS4_OK, which here would report success
				// for a lookup that resolved nothing and leave the current
				// filehandle where it was.
				if refined := directoryStatus(fileType); refined != types.NFS4_OK {
					status = refined
				}
			}
		}
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
		// Resolve the share's root handle first, and install it only once the
		// gate below has passed. A junction can outlive its share: the pseudo-fs
		// is rebuilt from a share-change callback that RemoveShare fires only
		// after the whole teardown, so the registry entry is gone for the entire
		// time the junction is still walkable, and a share can also be
		// configured but not yet loaded. Both must answer NFS4ERR_NOENT the way
		// a missing export does. The gate cannot distinguish them from a
		// refusal -- its netgroup lookup fails closed on a share it cannot find
		// -- so running it first would report a removed export as a permission
		// denial. Resolving here leaks nothing: the handle is returned only
		// below.
		realHandle, err := h.Registry.GetRootHandle(child.ShareName)
		if err != nil {
			logger.Debug("NFSv4 LOOKUP junction crossing failed",
				"share", child.ShareName,
				"error", err,
				"client", ctx.ClientAddr)
			return &types.CompoundResult{
				Status: types.NFS4ERR_NOENT,
				OpCode: types.OP_LOOKUP,
				Data:   encodeStatusOnly(types.NFS4ERR_NOENT),
			}
		}

		// Apply the same gate PUTFH does before handing out the share's root
		// handle. Crossing the junction puts a real share handle into the
		// current filehandle without building an auth context, so the checks
		// buildV4AuthContext performs never run on this path and the operations
		// that then act on that handle without an auth context of their own
		// (LOCK, LOCKT, LOCKU, GET_DIR_DELEGATION) would reach a disabled or
		// netgroup-restricted share from any address.
		if st := h.shareEntryStatus(ctx, child.ShareName); st != types.NFS4_OK {
			return &types.CompoundResult{
				Status: st,
				OpCode: types.OP_LOOKUP,
				Data:   encodeStatusOnly(st),
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
