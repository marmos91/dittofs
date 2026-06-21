package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// GETXATTR4args / GETXATTR4res (RFC 8276 Section 8.6):
//
//	struct GETXATTR4args {
//	    xattrkey4   gxa_name;   // component4 (utf8str_cs)
//	};
//	struct GETXATTR4resok {
//	    xattrvalue4 gxr_value;  // opaque<>
//	};
//	union GETXATTR4res switch (nfsstat4 status) {
//	 case NFS4_OK:  GETXATTR4resok;
//	 default:       void;
//	};
//
// A missing xattr returns NFS4ERR_NOXATTR. Pseudo-fs handles return
// NFS4ERR_NOTSUPP (the virtual namespace carries no named attributes).
func (h *Handler) handleGetXattr(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return xattrErr(types.OP_GETXATTR, status)
	}

	name, err := xdr.DecodeString(reader)
	if err != nil {
		return xattrErr(types.OP_GETXATTR, types.NFS4ERR_BADXDR)
	}

	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return xattrErr(types.OP_GETXATTR, types.NFS4ERR_NOTSUPP)
	}

	canonical, ok := canonicalizeXattrName(name)
	if !ok {
		// Unsupported namespace: no such attribute (RFC 8276 §8.1).
		return xattrErr(types.OP_GETXATTR, types.NFS4ERR_NOXATTR)
	}

	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		return xattrErr(types.OP_GETXATTR, types.NFS4ERR_SERVERFAULT)
	}

	backend, err := xattrBackendForHandler(h)
	if err != nil {
		return xattrErr(types.OP_GETXATTR, types.NFS4ERR_SERVERFAULT)
	}

	handle := metadata.FileHandle(ctx.CurrentFH)
	value, found, err := backend.GetXattr(authCtx, handle, canonical)
	if err != nil {
		status := common.MapToNFS4(err)
		return xattrErr(types.OP_GETXATTR, status)
	}
	if !found {
		return xattrErr(types.OP_GETXATTR, types.NFS4ERR_NOXATTR)
	}

	logger.Debug("NFSv4.2 GETXATTR", "name", canonical, "len", len(value), "client", ctx.ClientAddr)

	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	_ = xdr.WriteXDROpaque(&buf, value)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_GETXATTR,
		Data:   buf.Bytes(),
	}
}

// xattrErr builds a status-only CompoundResult for an xattr op error.
func xattrErr(opCode, status uint32) *types.CompoundResult {
	return &types.CompoundResult{
		Status: status,
		OpCode: opCode,
		Data:   encodeStatusOnly(status),
	}
}
