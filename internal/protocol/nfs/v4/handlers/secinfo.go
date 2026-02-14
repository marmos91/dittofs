package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// handleSecInfo implements the SECINFO operation (RFC 7530 Section 16.31).
//
// SECINFO returns the security mechanisms available for a given name in
// a directory. Returns AUTH_SYS (flavor 1) and AUTH_NONE (flavor 0) as
// available mechanisms. Phase 12 will add Kerberos (RPCSEC_GSS) flavors.
//
// Wire format args:
//
//	name:  component4 (XDR string -- the filename to query security for)
//
// Wire format res:
//
//	nfsstat4:       uint32
//	SECINFO4resok:  array of secinfo4 entries
//	  each entry:   flavor (uint32) [+ RPCSEC_GSS info if flavor == 6]
//
// For AUTH_SYS (flavor 1) and AUTH_NONE (flavor 0), no additional data
// follows the flavor number.
//
// Per RFC 7530 Section 16.31.4: SECINFO consumes the current filehandle.
// After SECINFO, the current FH is cleared.
func (h *Handler) handleSecInfo(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_SECINFO,
			Data:   encodeStatusOnly(status),
		}
	}

	// Read the component name
	name, err := xdr.DecodeString(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_SECINFO,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	logger.Debug("SECINFO: returning AUTH_SYS + AUTH_NONE",
		"name", name,
		"client", ctx.ClientAddr)

	// Per RFC 7530 Section 16.31.4: SECINFO consumes the current filehandle.
	ctx.CurrentFH = nil

	// Encode response: status + array of secinfo4
	// Return two entries: AUTH_SYS (strongest first) and AUTH_NONE
	const (
		authNoneFlavor = 0
		authSysFlavor  = 1
	)
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	_ = xdr.WriteUint32(&buf, 2) // array length: 2 entries
	_ = xdr.WriteUint32(&buf, authSysFlavor)
	_ = xdr.WriteUint32(&buf, authNoneFlavor)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_SECINFO,
		Data:   buf.Bytes(),
	}
}
