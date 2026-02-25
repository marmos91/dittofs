package handlers

import (
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nsm/xdr"
)

// UnmonAll handles the SM_UNMON_ALL procedure (procedure 4).
//
// SM_UNMON_ALL unregisters all monitoring for a specific callback address.
// This is typically called during client shutdown to clean up all
// monitoring registrations.
//
// Important: SM_UNMON_ALL only affects NSM registrations, NOT NLM locks.
// Clients must separately release their locks via NLM_UNLOCK or NLM_FREE_ALL.
//
// Parameters:
//   - ctx: The NSM handler context
//   - data: XDR-encoded my_id structure (callback info to unregister)
//
// Returns:
//   - *HandlerResult: sm_stat with current state
//   - error: XDR decoding error if input is malformed
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
