package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// handlePutFH implements the PUTFH operation (RFC 7530 Section 16.21).
// Sets the current filehandle from a client-provided opaque handle byte sequence.
// No delegation; validates handle size (max 128 bytes) and sets CompoundContext.CurrentFH.
// Sets CurrentFH for subsequent compound operations; no store access or state changes.
// Errors: NFS4ERR_BADHANDLE (empty or oversized handle), NFS4ERR_BADXDR,
// NFS4ERR_STALE (REST-02: share quiesced for restore, Plan 05-09 D-02).
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

	// REST-02 / D-02: if the handle traces back to a known runtime share and
	// that share is disabled, refuse with NFS4ERR_STALE. Clients reacquire
	// fresh handles after restore + explicit re-enable. Handles that do not
	// decode to a known share fall through unchanged — PUTFH stays
	// permissive for pseudo-fs and boot-verifier flows.
	if h.Registry != nil {
		shareName, err := h.Registry.GetShareNameForHandle(ctx.Context, metadata.FileHandle(handle))
		if err == nil {
			if share, sErr := h.Registry.GetShare(shareName); sErr == nil && share != nil && !share.Enabled {
				logger.Warn("NFSv4 PUTFH refused: share disabled",
					"share", share.Name, "client", ctx.ClientAddr)
				return &types.CompoundResult{
					Status: types.NFS4ERR_STALE,
					OpCode: types.OP_PUTFH,
					Data:   encodeStatusOnly(types.NFS4ERR_STALE),
				}
			}
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
