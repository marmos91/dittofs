package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// handleLookupP implements the LOOKUPP operation (RFC 7530 Section 16.16).
// Navigates to the parent directory of the current filehandle (root's parent is root itself).
// Delegates to MetadataService.GetParent for real files; traverses pseudo-fs tree for virtual handles.
// Sets CurrentFH to the parent directory; crosses from share root back to pseudo-fs at boundaries.
// Errors: NFS4ERR_NOFILEHANDLE, NFS4ERR_NOENT, NFS4ERR_STALE, NFS4ERR_IO.
func (h *Handler) handleLookupP(ctx *types.CompoundContext, _ io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_LOOKUPP,
			Data:   encodeStatusOnly(status),
		}
	}

	// Check if current FH is a pseudo-fs handle
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return h.lookupParentInPseudoFS(ctx)
	}

	// Real filesystem handle -- navigate to parent in real-FS
	return h.lookupParentInRealFS(ctx)
}

// lookupParentInRealFS handles LOOKUPP within a real filesystem.
//
// If the current file is at the share root (parent resolves to itself or
// returns not-found), the handler crosses back to the pseudo-fs junction
// for this share.
func (h *Handler) lookupParentInRealFS(ctx *types.CompoundContext) *types.CompoundResult {
	authCtx, shareName, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_LOOKUPP,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_LOOKUPP,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	// Get current file's parent handle from metadata store
	store, err := metaSvc.GetStoreForShare(shareName)
	if err != nil {
		status := types.MapMetadataErrorToNFS4(err)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_LOOKUPP,
			Data:   encodeStatusOnly(status),
		}
	}

	parentHandle, err := store.GetParent(authCtx.Context, metadata.FileHandle(ctx.CurrentFH))
	if err != nil {
		// No parent found -- this is the share root. Cross back to pseudo-fs.
		return h.crossBackToPseudoFS(ctx, shareName)
	}

	// Check if parent handle is the same as current handle (root's parent is root)
	if string(parentHandle) == string(ctx.CurrentFH) {
		return h.crossBackToPseudoFS(ctx, shareName)
	}

	// Set current FH to parent handle (copy-on-set)
	ctx.CurrentFH = make([]byte, len(parentHandle))
	copy(ctx.CurrentFH, parentHandle)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_LOOKUPP,
		Data:   encodeStatusOnly(types.NFS4_OK),
	}
}

// crossBackToPseudoFS sets the current filehandle to the pseudo-fs junction
// for the given share name. Called when LOOKUPP at share root needs to go
// back to the virtual namespace.
func (h *Handler) crossBackToPseudoFS(ctx *types.CompoundContext, shareName string) *types.CompoundResult {
	if h.PseudoFS == nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_LOOKUPP,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	junction, ok := h.PseudoFS.FindJunction(shareName)
	if !ok {
		// No junction found -- shouldn't happen for a valid share
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_LOOKUPP,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	// Set current FH to the junction's handle (copy-on-set)
	ctx.CurrentFH = make([]byte, len(junction.Handle))
	copy(ctx.CurrentFH, junction.Handle)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_LOOKUPP,
		Data:   encodeStatusOnly(types.NFS4_OK),
	}
}

// lookupParentInPseudoFS handles LOOKUPP within the pseudo-filesystem.
func (h *Handler) lookupParentInPseudoFS(ctx *types.CompoundContext) *types.CompoundResult {
	// Find the current node by handle
	node, ok := h.PseudoFS.LookupByHandle(ctx.CurrentFH)
	if !ok {
		return &types.CompoundResult{
			Status: types.NFS4ERR_STALE,
			OpCode: types.OP_LOOKUPP,
			Data:   encodeStatusOnly(types.NFS4ERR_STALE),
		}
	}

	// Get parent node (root's parent is root itself)
	parent, ok := h.PseudoFS.LookupParent(node)
	if !ok {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_LOOKUPP,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	// Set current FH to parent's handle
	ctx.CurrentFH = make([]byte, len(parent.Handle))
	copy(ctx.CurrentFH, parent.Handle)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_LOOKUPP,
		Data:   encodeStatusOnly(types.NFS4_OK),
	}
}
