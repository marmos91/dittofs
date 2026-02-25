package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// handlePutFH implements the PUTFH operation (RFC 7530 Section 16.21).
// Sets the current filehandle from a client-provided opaque handle byte sequence.
// No delegation; validates handle size (max 128 bytes) and sets CompoundContext.CurrentFH.
// Sets CurrentFH for subsequent compound operations; no store access or state changes.
// Errors: NFS4ERR_BADHANDLE (empty or oversized handle), NFS4ERR_BADXDR.
func (h *Handler) handlePutFH(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Read filehandle as XDR opaque
	handle, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_PUTFH,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Validate handle size (max NFS4_FHSIZE = 128 bytes)
	if len(handle) > types.NFS4_FHSIZE {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADHANDLE,
			OpCode: types.OP_PUTFH,
			Data:   encodeStatusOnly(types.NFS4ERR_BADHANDLE),
		}
	}

	// Validate handle is not empty
	if len(handle) == 0 {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADHANDLE,
			OpCode: types.OP_PUTFH,
			Data:   encodeStatusOnly(types.NFS4ERR_BADHANDLE),
		}
	}

	// Set the current filehandle
	ctx.CurrentFH = make([]byte, len(handle))
	copy(ctx.CurrentFH, handle)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_PUTFH,
		Data:   encodeStatusOnly(types.NFS4_OK),
	}
}
