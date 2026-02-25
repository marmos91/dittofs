package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// handleReadLink implements the READLINK operation (RFC 7530 Section 16.27).
// Returns the target pathname stored in a symbolic link for client-side path resolution.
// Delegates to MetadataService.ReadSymlink; pseudo-fs handles always return NFS4ERR_INVAL.
// No side effects; read-only metadata operation returning the symlink target path.
// Errors: NFS4ERR_NOFILEHANDLE, NFS4ERR_INVAL (not a symlink), NFS4ERR_STALE, NFS4ERR_IO.
func (h *Handler) handleReadLink(ctx *types.CompoundContext, _ io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_READLINK,
			Data:   encodeStatusOnly(status),
		}
	}

	// Pseudo-fs handles have no symlinks
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return &types.CompoundResult{
			Status: types.NFS4ERR_INVAL,
			OpCode: types.OP_READLINK,
			Data:   encodeStatusOnly(types.NFS4ERR_INVAL),
		}
	}

	// Real filesystem handle -- read symlink target
	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_READLINK,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_READLINK,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	target, _, err := metaSvc.ReadSymlink(authCtx, metadata.FileHandle(ctx.CurrentFH))
	if err != nil {
		status := types.MapMetadataErrorToNFS4(err)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_READLINK,
			Data:   encodeStatusOnly(status),
		}
	}

	// Encode response: status + linktext (XDR string)
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	_ = xdr.WriteXDRString(&buf, target)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_READLINK,
		Data:   buf.Bytes(),
	}
}
