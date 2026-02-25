package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// handleNVerify implements the NVERIFY operation (RFC 7530 Section 16.15).
// Succeeds (NFS4_OK) when server attributes do NOT match client-provided fattr4 (inverse of VERIFY).
// Delegates to verifyAttributes for byte-exact comparison of encoded attribute data.
// No side effects; enables conditional compounds (e.g., NVERIFY+READ to skip unchanged files).
// Errors: NFS4ERR_SAME (attrs match), NFS4ERR_NOFILEHANDLE, NFS4ERR_BADXDR, NFS4ERR_STALE.
func (h *Handler) handleNVerify(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	match, status := verifyAttributes(h, ctx, reader)

	if status != types.NFS4_OK {
		logger.Debug("NFSv4 NVERIFY failed",
			"status", status,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_NVERIFY,
			Data:   encodeStatusOnly(status),
		}
	}

	if !match {
		// Attributes differ -- this is what NVERIFY wants
		logger.Debug("NFSv4 NVERIFY: attributes differ (success)",
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4_OK,
			OpCode: types.OP_NVERIFY,
			Data:   encodeStatusOnly(types.NFS4_OK),
		}
	}

	// Attributes match -- NVERIFY considers this an error
	logger.Debug("NFSv4 NVERIFY: attributes match (same)",
		"client", ctx.ClientAddr)
	return &types.CompoundResult{
		Status: types.NFS4ERR_SAME,
		OpCode: types.OP_NVERIFY,
		Data:   encodeStatusOnly(types.NFS4ERR_SAME),
	}
}
