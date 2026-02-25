package handlers

import (
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// handleRenew implements the RENEW operation (RFC 7530 Section 16.30).
// Renews the lease associated with a client ID to prevent state expiration.
// Delegates to StateManager.RenewLease for client validation and lease refresh.
// Extends client lease timer; no file or directory state changes.
// Errors: NFS4ERR_STALE_CLIENTID, NFS4ERR_EXPIRED, NFS4ERR_BADXDR.
func (h *Handler) handleRenew(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Read clientid (uint64)
	clientID, err := xdr.DecodeUint64(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_RENEW,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Delegate to StateManager for validation and lease renewal
	if err := h.StateManager.RenewLease(clientID); err != nil {
		nfsStatus := mapStateError(err)
		logger.Info("NFSv4 RENEW failed",
			"client_id", clientID,
			"error", err,
			"nfs_status", nfsStatus,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: nfsStatus,
			OpCode: types.OP_RENEW,
			Data:   encodeStatusOnly(nfsStatus),
		}
	}

	logger.Debug("NFSv4 RENEW: lease renewed",
		"client_id", clientID,
		"client", ctx.ClientAddr)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_RENEW,
		Data:   encodeStatusOnly(types.NFS4_OK),
	}
}
