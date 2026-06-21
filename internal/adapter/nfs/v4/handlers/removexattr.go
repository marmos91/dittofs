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

// REMOVEXATTR4args / REMOVEXATTR4res (RFC 8276 Section 8.6):
//
//	struct REMOVEXATTR4args {
//	    xattrkey4    rxa_name;   // component4 (utf8str_cs)
//	};
//	struct REMOVEXATTR4resok {
//	    change_info4 rxr_info;
//	};
//	union REMOVEXATTR4res switch (nfsstat4 status) {
//	 case NFS4_OK: REMOVEXATTR4resok;
//	 default:      void;
//	};
//
// REMOVEXATTR carries NO stateid. Removing a missing xattr returns
// NFS4ERR_NOXATTR. The change_info4 reflects the file's change attribute
// (ctime) before/after. Pseudo-fs handles return NFS4ERR_ROFS.
func (h *Handler) handleRemoveXattr(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return xattrErr(types.OP_REMOVEXATTR, status)
	}

	name, err := xdr.DecodeString(reader)
	if err != nil {
		return xattrErr(types.OP_REMOVEXATTR, types.NFS4ERR_BADXDR)
	}

	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return xattrErr(types.OP_REMOVEXATTR, types.NFS4ERR_ROFS)
	}

	canonical, ok := canonicalizeXattrName(name)
	if !ok {
		return xattrErr(types.OP_REMOVEXATTR, types.NFS4ERR_NOXATTR)
	}

	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		return xattrErr(types.OP_REMOVEXATTR, types.NFS4ERR_SERVERFAULT)
	}
	backend, err := xattrBackendForHandler(h)
	if err != nil {
		return xattrErr(types.OP_REMOVEXATTR, types.NFS4ERR_SERVERFAULT)
	}

	handle := metadata.FileHandle(ctx.CurrentFH)

	// change_info4 "before" (best-effort; advisory WCC data).
	before := h.xattrChangeID(ctx, handle)

	// Missing xattr -> NFS4ERR_NOXATTR (the backend RemoveXattr is a no-op on a
	// missing name, so probe existence first).
	_, exists, gerr := backend.GetXattr(authCtx, handle, canonical)
	if gerr != nil {
		return xattrErr(types.OP_REMOVEXATTR, common.MapToNFS4(gerr))
	}
	if !exists {
		return xattrErr(types.OP_REMOVEXATTR, types.NFS4ERR_NOXATTR)
	}

	if err := backend.RemoveXattr(authCtx, handle, canonical); err != nil {
		return xattrErr(types.OP_REMOVEXATTR, common.MapToNFS4(err))
	}

	after := h.xattrChangeID(ctx, handle)
	if after == 0 {
		after = before
	}

	logger.Debug("NFSv4.2 REMOVEXATTR", "name", canonical, "client", ctx.ClientAddr)

	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	encodeChangeInfo4(&buf, true, before, after)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_REMOVEXATTR,
		Data:   buf.Bytes(),
	}
}
