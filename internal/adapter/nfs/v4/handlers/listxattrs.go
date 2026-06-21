package handlers

import (
	"bytes"
	"io"
	"sort"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// LISTXATTRS4args / LISTXATTRS4res (RFC 8276 Section 8.6):
//
//	struct LISTXATTRS4args {
//	    nfs_cookie4 lxa_cookie;    // uint64
//	    count4      lxa_maxcount;  // uint32
//	};
//	struct LISTXATTRS4resok {
//	    nfs_cookie4 lxr_cookie;    // uint64
//	    xattrkey4   lxr_names<>;   // component4<>
//	    bool        lxr_eof;
//	};
//	union LISTXATTRS4res switch (nfsstat4 status) {
//	 case NFS4_OK: LISTXATTRS4resok;
//	 default:      void;
//	};
//
// LISTXATTRS carries NO stateid. Names are returned in the wire ("user."-
// stripped) form so they match what a setfattr/getfattr client expects. The
// cookie is the count of names already returned by prior calls; paging is over
// a stable (sorted) name order. Pseudo-fs handles return NFS4ERR_NOTSUPP.
func (h *Handler) handleListXattrs(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return xattrErr(types.OP_LISTXATTRS, status)
	}

	cookie, err := xdr.DecodeUint64(reader)
	if err != nil {
		return xattrErr(types.OP_LISTXATTRS, types.NFS4ERR_BADXDR)
	}
	maxcount, err := xdr.DecodeUint32(reader)
	if err != nil {
		return xattrErr(types.OP_LISTXATTRS, types.NFS4ERR_BADXDR)
	}

	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return xattrErr(types.OP_LISTXATTRS, types.NFS4ERR_NOTSUPP)
	}

	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		return xattrErr(types.OP_LISTXATTRS, types.NFS4ERR_SERVERFAULT)
	}
	backend, err := xattrBackendForHandler(h)
	if err != nil {
		return xattrErr(types.OP_LISTXATTRS, types.NFS4ERR_SERVERFAULT)
	}

	handle := metadata.FileHandle(ctx.CurrentFH)
	rawNames, err := backend.ListXattr(authCtx, handle)
	if err != nil {
		return xattrErr(types.OP_LISTXATTRS, common.MapToNFS4(err))
	}

	// Strip the "user." prefix and keep only user-namespace names (the only
	// namespace exposed over the wire), then sort for a stable cookie order.
	names := make([]string, 0, len(rawNames))
	for _, n := range rawNames {
		if wire, ok := wireXattrName(n); ok {
			names = append(names, wire)
		}
	}
	sort.Strings(names)

	if cookie > uint64(len(names)) {
		// A cookie past the end is stale/invalid.
		return xattrErr(types.OP_LISTXATTRS, types.NFS4ERR_BAD_COOKIE)
	}

	// Budget the reply to maxcount bytes: cookie(8) + array-count(4) + eof(4)
	// is the fixed overhead; each name costs len(4) + utf8 + XDR pad.
	const fixedOverhead = 8 + 4 + 4
	budget := int(maxcount)
	if budget < fixedOverhead {
		// maxcount must at least hold the empty reply; RFC 8276 returns
		// NFS4ERR_TOOSMALL when it cannot.
		return xattrErr(types.OP_LISTXATTRS, types.NFS4ERR_TOOSMALL)
	}
	used := fixedOverhead

	var out []string
	i := int(cookie)
	for ; i < len(names); i++ {
		entryLen := 4 + len(names[i])
		if pad := (4 - (len(names[i]) % 4)) % 4; pad != 0 {
			entryLen += pad
		}
		if used+entryLen > budget {
			break
		}
		used += entryLen
		out = append(out, names[i])
	}

	if len(out) == 0 && i < len(names) {
		// The very first remaining name does not fit in maxcount.
		return xattrErr(types.OP_LISTXATTRS, types.NFS4ERR_TOOSMALL)
	}

	newCookie := cookie + uint64(len(out))
	eof := int(newCookie) >= len(names)

	logger.Debug("NFSv4.2 LISTXATTRS", "total", len(names), "returned", len(out),
		"cookie", cookie, "new_cookie", newCookie, "eof", eof, "client", ctx.ClientAddr)

	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	_ = xdr.WriteUint64(&buf, newCookie)
	_ = xdr.WriteUint32(&buf, uint32(len(out))) // names<> array length
	for _, n := range out {
		_ = xdr.WriteXDRString(&buf, n)
	}
	_ = xdr.WriteBool(&buf, eof)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_LISTXATTRS,
		Data:   buf.Bytes(),
	}
}

// wireXattrName converts a store-canonical xattr name to its wire form (drop the
// "user." prefix). ok is false for names outside the user namespace, which are
// hidden from LISTXATTRS (e.g. the reserved SMB security.NTACL name).
func wireXattrName(canonical string) (string, bool) {
	stripped := stripXattrPrefix(canonical)
	if stripped == canonical {
		// No "user." prefix: a non-user-namespace name; hide it from the wire.
		return "", false
	}
	if stripped == "" {
		// A bare "user." with no key (unexpected backend data): emitting it would
		// put an empty, invalid name on the wire. Hide it defensively.
		return "", false
	}
	return stripped, true
}
