package handlers

import (
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/xdr"
)

// Notify handles NSM NOTIFY (RFC 1813, SM procedure 6).
// Receives state-change notification from a remote NSM when a monitored host restarts.
// No delegation yet; logs the notification (callback dispatch deferred to future plan).
// No side effects currently; will eventually trigger SM_NOTIFY callbacks to local clients.
// Errors: none (one-way procedure; decode errors are logged but do not fail).
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
