package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
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
// NFS4ERR_STALE (share quiesced for restore).
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

	// A filehandle is either one of the pseudo-fs handles this server mints
	// or a "<share>:<uuid>" object handle. Anything that parses as neither
	// was never issued here, and RFC 7530 Section 16.21.4 answers that with
	// NFS4ERR_BADHANDLE. Accepting it instead deferred the error to whichever
	// later operation first tried to resolve it, which reports the wrong
	// status and blames the wrong operation.
	if !pseudofs.IsPseudoFSHandle(handle) {
		if _, _, decErr := metadata.DecodeFileHandle(metadata.FileHandle(handle)); decErr != nil {
			logger.Debug("NFSv4 PUTFH refused: undecodable filehandle",
				"len", len(handle), "client", ctx.ClientAddr)
			return &types.CompoundResult{
				Status: types.NFS4ERR_BADHANDLE,
				OpCode: types.OP_PUTFH,
				Data:   encodeStatusOnly(types.NFS4ERR_BADHANDLE),
			}
		}
	}

	// If the handle traces back to a known runtime share, apply the gate that
	// covers the operations acting on the current filehandle without an auth
	// context of their own. Handles that do not decode to a known share fall
	// through unchanged — PUTFH stays permissive for pseudo-fs and
	// boot-verifier flows.
	if h.Registry != nil {
		shareName, err := h.Registry.GetShareNameForHandle(ctx.Context, metadata.FileHandle(handle))
		if err == nil {
			if st := h.shareEntryStatus(ctx, shareName); st != types.NFS4_OK {
				return &types.CompoundResult{
					Status: st,
					OpCode: types.OP_PUTFH,
					Data:   encodeStatusOnly(st),
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
