package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// handleReadLink implements the READLINK operation (RFC 7530 Section 16.27).
//
// READLINK returns the target path of a symbolic link. The current filehandle
// must reference a symlink; otherwise NFS4ERR_INVAL is returned.
//
// For pseudo-fs handles, READLINK always returns NFS4ERR_INVAL since pseudo-fs
// nodes are directories and cannot be symlinks.
//
// Wire format args: none
// Wire format res:  nfsstat4 (uint32) + linktext (XDR string)
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
