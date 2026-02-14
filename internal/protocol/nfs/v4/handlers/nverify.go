package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
)

// handleNVerify implements the NVERIFY operation (RFC 7530 Section 16.15).
//
// NVERIFY is the inverse of VERIFY: it succeeds (NFS4_OK) when the server's
// attributes do NOT match the client-provided fattr4. If they match,
// NFS4ERR_SAME is returned and the compound stops.
//
// This enables conditional compound sequences like:
//
//	NVERIFY(mtime == cached_mtime) + READ
//
// The READ only executes if the file has been modified (mtime changed).
//
// Wire format args:
//
//	obj_attributes: fattr4 (bitmap4 + opaque attr_vals)
//
// Wire format res:
//
//	nfsstat4 only (no additional data)
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
