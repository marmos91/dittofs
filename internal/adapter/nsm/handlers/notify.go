package handlers

import (
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nsm/types"
	"github.com/marmos91/dittofs/internal/adapter/nsm/xdr"
)

// Notify handles the SM_NOTIFY procedure (procedure 6).
//
// SM_NOTIFY is sent by another NSM instance when a host restarts.
// This notifies our NSM that a remote host has changed state, so we
// can inform any local clients monitoring that host.
//
// The crash recovery flow:
//  1. Remote server crashes and restarts
//  2. Remote NSM sends SM_NOTIFY to all registered monitors
//  3. Our NSM receives SM_NOTIFY (this handler)
//  4. Our NSM should send callbacks to local clients monitoring that host
//  5. Local clients reclaim their locks during grace period
//
// For now, this handler logs the notification. Full crash recovery
// with callback dispatch will be implemented in Plan 03-03.
//
// Parameters:
//   - ctx: The NSM handler context
//   - data: XDR-encoded stat_chge structure (host and new state)
//
// Returns:
//   - *HandlerResult: Empty response (SM_NOTIFY is one-way)
//   - error: XDR decoding error if input is malformed
func (h *Handler) Notify(ctx *NSMHandlerContext, data []byte) (*HandlerResult, error) {
	// Decode stat_chge argument
	r := newBytesReader(data)
	statChge, err := xdr.DecodeStatChge(r)
	if err != nil {
		logger.Warn("NSM NOTIFY decode error",
			"client", ctx.ClientAddr,
			"error", err)
		// SM_NOTIFY doesn't return a result, just log the error
		return &HandlerResult{
			Data:      []byte{},
			NSMStatus: types.StatSucc,
		}, nil
	}

	logger.Info("NSM NOTIFY received",
		"client", ctx.ClientAddr,
		"mon_name", statChge.MonName,
		"new_state", statChge.State)

	// TODO (Plan 03-03): Dispatch callbacks to local clients monitoring this host
	//
	// The full implementation should:
	// 1. Find all local registrations where MonName matches statChge.MonName
	// 2. For each registration, send SM_NOTIFY callback with:
	//    - MonName: statChge.MonName
	//    - State: statChge.State
	//    - Priv: The client's stored priv data
	// 3. Track callback success/failure for observability
	//
	// For now, we log the notification. This is enough for basic operation
	// since our own crash recovery (notifying clients of OUR restart) is
	// more critical than relaying third-party notifications.

	// SM_NOTIFY doesn't return a meaningful result
	return &HandlerResult{
		Data:      []byte{},
		NSMStatus: types.StatSucc,
	}, nil
}
