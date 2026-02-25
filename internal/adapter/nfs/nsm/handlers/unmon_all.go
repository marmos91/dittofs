package handlers

import (
	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/xdr"
	"github.com/marmos91/dittofs/internal/logger"
)

// UnmonAll handles NSM UNMON_ALL (RFC 1813, SM procedure 4).
// Unregisters all monitoring for a callback address; used during client shutdown.
// Iterates ConnectionTracker.GetNSMClients and clears matching registrations.
// Removes all NSM callback info for the callback host; does NOT release NLM locks.
// Errors: none (always returns current state; decode errors are logged).
func (h *Handler) UnmonAll(ctx *NSMHandlerContext, data []byte) (*HandlerResult, error) {
	state := h.GetServerState()

	// Decode my_id argument
	r := newBytesReader(data)
	myID, err := xdr.DecodeMyID(r)
	if err != nil {
		logger.Warn("NSM UNMON_ALL decode error",
			"client", ctx.ClientAddr,
			"error", err)
		return encodeStatResponse(state)
	}

	logger.Debug("NSM UNMON_ALL request",
		"client", ctx.ClientAddr,
		"callback_host", myID.MyName)

	// Find and clear all registrations for this callback host
	// We iterate through all NSM clients and clear those matching the callback
	clearedCount := 0
	nsmClients := h.tracker.GetNSMClients()

	for _, client := range nsmClients {
		if client.CallbackInfo != nil && client.CallbackInfo.Hostname == myID.MyName {
			// Clear NSM info for this client
			h.tracker.ClearNSMInfo(client.ClientID)

			// Remove from persistent store if configured
			if h.clientStore != nil {
				if err := h.clientStore.DeleteClientRegistration(ctx.Context, client.ClientID); err != nil {
					logger.Warn("NSM UNMON_ALL persistence deletion failed",
						"client", ctx.ClientAddr,
						"client_id", client.ClientID,
						"error", err)
				}
			}
			clearedCount++
		}
	}

	logger.Info("NSM UNMON_ALL completed",
		"client", ctx.ClientAddr,
		"callback_host", myID.MyName,
		"cleared", clearedCount)

	// Return current state
	return encodeStatResponse(state)
}
