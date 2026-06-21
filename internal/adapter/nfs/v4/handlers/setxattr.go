package handlers

import (
	"bytes"
	"errors"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// SETXATTR4args / SETXATTR4res (RFC 8276 Section 8.6):
//
//	enum setxattr_option4 {
//	    SETXATTR4_EITHER  = 0,
//	    SETXATTR4_CREATE  = 1,
//	    SETXATTR4_REPLACE = 2
//	};
//	struct SETXATTR4args {
//	    setxattr_option4 sxa_option;
//	    xattrkey4        sxa_key;    // component4 (utf8str_cs)
//	    xattrvalue4      sxa_value;  // opaque<>
//	};
//	struct SETXATTR4resok {
//	    change_info4 sxr_info;
//	};
//	union SETXATTR4res switch (nfsstat4 status) {
//	 case NFS4_OK: SETXATTR4resok;
//	 default:      void;
//	};
//
// SETXATTR carries NO stateid. CREATE on an existing xattr returns
// NFS4ERR_EXIST; REPLACE on a missing xattr returns NFS4ERR_NOXATTR. The
// change_info4 reflects the file's change attribute (ctime) before/after.
// Pseudo-fs handles return NFS4ERR_ROFS (the virtual namespace is read-only).
func (h *Handler) handleSetXattr(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return xattrErr(types.OP_SETXATTR, status)
	}

	option, err := xdr.DecodeUint32(reader)
	if err != nil {
		return xattrErr(types.OP_SETXATTR, types.NFS4ERR_BADXDR)
	}
	name, err := xdr.DecodeString(reader)
	if err != nil {
		return xattrErr(types.OP_SETXATTR, types.NFS4ERR_BADXDR)
	}
	// DecodeOpaque enforces a generic 1 MiB XDR cap. A value over our 64 KiB
	// inline ceiling but within that cap decodes here and the store returns
	// ErrXattrTooLarge -> NFS4ERR_XATTR2BIG below. A value over 1 MiB is
	// unreachable from a conformant client (Linux XATTR_SIZE_MAX is 64 KiB) and
	// surfaces as NFS4ERR_BADXDR — acceptable for malformed input.
	value, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return xattrErr(types.OP_SETXATTR, types.NFS4ERR_BADXDR)
	}

	if option != types.SETXATTR4_EITHER &&
		option != types.SETXATTR4_CREATE &&
		option != types.SETXATTR4_REPLACE {
		return xattrErr(types.OP_SETXATTR, types.NFS4ERR_INVAL)
	}

	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return xattrErr(types.OP_SETXATTR, types.NFS4ERR_ROFS)
	}

	canonical, ok := canonicalizeXattrName(name)
	if !ok {
		// Unsupported namespace: there is no place to create such an xattr,
		// and REPLACE of a non-existent one is NOXATTR.
		return xattrErr(types.OP_SETXATTR, types.NFS4ERR_NOXATTR)
	}

	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		return xattrErr(types.OP_SETXATTR, types.NFS4ERR_SERVERFAULT)
	}
	backend, err := xattrBackendForHandler(h)
	if err != nil {
		return xattrErr(types.OP_SETXATTR, types.NFS4ERR_SERVERFAULT)
	}

	handle := metadata.FileHandle(ctx.CurrentFH)

	// change_info4 "before". Best-effort: the change attribute is advisory WCC
	// data, so a lookup failure falls back to 0 rather than failing the op.
	before := h.xattrChangeID(ctx, handle)

	if option != types.SETXATTR4_EITHER {
		_, exists, gerr := backend.GetXattr(authCtx, handle, canonical)
		if gerr != nil {
			return xattrErr(types.OP_SETXATTR, common.MapToNFS4(gerr))
		}
		if option == types.SETXATTR4_CREATE && exists {
			return xattrErr(types.OP_SETXATTR, types.NFS4ERR_EXIST)
		}
		if option == types.SETXATTR4_REPLACE && !exists {
			return xattrErr(types.OP_SETXATTR, types.NFS4ERR_NOXATTR)
		}
	}

	if err := backend.SetXattr(authCtx, handle, canonical, value); err != nil {
		// RFC 8276 §11.2: a value exceeding the server's limit is XATTR2BIG.
		// ErrXattrTooLarge is a metadata sentinel (ErrInvalidArgument-coded), so
		// it would otherwise coarsen to NFS4ERR_INVAL via the generic map.
		if errors.Is(err, metadata.ErrXattrTooLarge) {
			return xattrErr(types.OP_SETXATTR, types.NFS4ERR_XATTR2BIG)
		}
		return xattrErr(types.OP_SETXATTR, common.MapToNFS4(err))
	}

	after := h.xattrChangeID(ctx, handle)
	if after == 0 {
		// The set succeeded but the post-op changeid was unavailable; fall back
		// to the pre-op changeid so before/after stay coherent.
		after = before
	}

	logger.Debug("NFSv4.2 SETXATTR", "name", canonical, "option", option,
		"len", len(value), "client", ctx.ClientAddr)

	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	encodeChangeInfo4(&buf, true, before, after)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_SETXATTR,
		Data:   buf.Bytes(),
	}
}

// xattrChangeID returns the current change attribute (changeid4) of the file,
// derived from its ctime in nanoseconds — the same source GETATTR uses for
// FATTR4_CHANGE — for use in the change_info4 of SETXATTR/REMOVEXATTR.
//
// It is best-effort: the change attribute is advisory WCC data, so when no
// metadata service is available (e.g. a fake-backend unit test) or the lookup
// fails it returns 0 rather than failing the operation.
func (h *Handler) xattrChangeID(ctx *types.CompoundContext, handle metadata.FileHandle) uint64 {
	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil || metaSvc == nil {
		return 0
	}
	file, err := metaSvc.GetFile(ctx.Context, handle)
	if err != nil {
		return 0
	}
	return uint64(file.Ctime.UnixNano())
}
