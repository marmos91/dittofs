package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// handleSecInfoNoName implements SECINFO_NO_NAME (RFC 8881 Section 18.45).
// Returns the same security flavor list as SECINFO, but for an object reached
// by filehandle rather than by name: style SECINFO_STYLE4_CURRENT_FH asks
// about the current filehandle, style SECINFO_STYLE4_PARENT about its parent.
// Consumes the current filehandle on success, so a following operation in the
// COMPOUND sees none (RFC 8881 Section 2.6.3.1.1.8).
// Errors: NFS4ERR_BADXDR, NFS4ERR_NOFILEHANDLE, NFS4ERR_INVAL, NFS4ERR_NOENT,
// NFS4ERR_NOTDIR, NFS4ERR_ACCESS.
func (h *Handler) handleSecInfoNoName(
	ctx *types.CompoundContext,
	_ *types.V41RequestContext,
	reader io.Reader,
) *types.CompoundResult {
	// Decode before anything can reject the request: the reader must be left
	// at the next operation's arguments either way, or the COMPOUND desyncs.
	var args types.SecinfoNoNameArgs
	if err := args.Decode(reader); err != nil {
		return secInfoErr(types.OP_SECINFO_NO_NAME, types.NFS4ERR_BADXDR)
	}

	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return secInfoErr(types.OP_SECINFO_NO_NAME, status)
	}

	switch args.Style {
	case types.SECINFO_STYLE4_CURRENT_FH:
		// The current filehandle is already the object being asked about.
	case types.SECINFO_STYLE4_PARENT:
		if status := h.secInfoParentStatus(ctx); status != types.NFS4_OK {
			logger.Debug("SECINFO_NO_NAME: parent unavailable",
				"status", status,
				"client", ctx.ClientAddr)
			return secInfoErr(types.OP_SECINFO_NO_NAME, status)
		}
	default:
		return secInfoErr(types.OP_SECINFO_NO_NAME, types.NFS4ERR_INVAL)
	}

	ctx.CurrentFH = nil

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_SECINFO_NO_NAME,
		Data:   encodeSecInfoFlavors(h.KerberosEnabled),
	}
}

// secInfoParentStatus reports whether the current filehandle has a parent
// directory SECINFO_NO_NAME can answer for.
//
// Style SECINFO_STYLE4_PARENT applies LOOKUPP's access rules, and a filehandle
// with no parent at all is NFS4ERR_NOENT (RFC 8881 Section 18.45.3). The only
// such filehandle here is the pseudo-filesystem root: every share root sits
// under a junction in that tree, so a real-filesystem directory always has a
// parent, and what is left to check is that the caller may traverse it.
func (h *Handler) secInfoParentStatus(ctx *types.CompoundContext) uint32 {
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		node, ok := h.PseudoFS.LookupByHandle(ctx.CurrentFH)
		if !ok {
			return types.NFS4ERR_STALE
		}
		parent, ok := h.PseudoFS.LookupParent(node)
		if !ok || parent == nil || parent == node {
			// The root is its own parent in this tree, so it has no parent
			// to report on.
			return types.NFS4ERR_NOENT
		}
		return types.NFS4_OK
	}

	// Resolving "." applies the directory-type and search-permission checks
	// LOOKUPP would, without needing the parent handle the caller never sees.
	return h.secInfoLookupStatus(ctx, ".")
}
