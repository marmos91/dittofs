package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// handleGetFH implements the GETFH operation (RFC 7530 Section 16.10).
// Returns the current filehandle as an opaque byte sequence for client storage.
// No delegation; reads CompoundContext.CurrentFH directly (no store access).
// No side effects; stateless operation returning the current FH as XDR opaque.
// Errors: NFS4ERR_NOFILEHANDLE (no current FH set).
func (h *Handler) handleGetFH(ctx *types.CompoundContext, _ io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_GETFH,
			Data:   encodeStatusOnly(status),
		}
	}

	// Encode response: status + filehandle as XDR opaque
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	_ = xdr.WriteXDROpaque(&buf, ctx.CurrentFH)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_GETFH,
		Data:   buf.Bytes(),
	}
}
