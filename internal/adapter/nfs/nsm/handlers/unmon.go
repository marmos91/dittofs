package handlers

import (
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/xdr"
)

// Unmon handles NSM UNMON (RFC 1813, SM procedure 3).
// Unregisters monitoring for a specific host; caller stops receiving SM_NOTIFY callbacks.
// Delegates to ConnectionTracker.ClearNSMInfo and removes from ClientRegistrationStore.
// Clears NSM callback info for the client; does NOT release NLM locks.
// Errors: none (always returns current state; decode errors are logged).
func (h *Handler) Unmon(ctx *NSMHandlerContext, data []byte) (*HandlerResult, error) {
	state := h.GetServerState()

	// Decode mon_id argument
	r := newBytesReader(data)
	monID, err := xdr.DecodeMonID(r)
	if err != nil {
		logger.Warn("NSM UNMON decode error",
			"client", ctx.ClientAddr,
			"error", err)
		return encodeStatResponse(state)
	}

	logger.Debug("NSM UNMON request",
		"client", ctx.ClientAddr,
		"mon_name", monID.MonName,
		"callback_host", monID.MyID.MyName)

	// Generate client ID matching the SM_MON registration
	clientID := generateClientID(ctx.ClientAddr, monID.MyID.MyName)

	// Clear NSM info from the registration
	// This keeps the connection registered but removes NSM callback info
	h.tracker.ClearNSMInfo(clientID)

	// Remove from persistent store if configured
	if h.clientStore != nil {
		if err := h.clientStore.DeleteClientRegistration(ctx.Context, clientID); err != nil {
			logger.Warn("NSM UNMON persistence deletion failed",
				"client", ctx.ClientAddr,
				"client_id", clientID,
				"error", err)
			// Don't fail - memory state was updated
		}
	}

	logger.Info("NSM UNMON completed",
		"client_id", clientID,
		"mon_name", monID.MonName)

	// Return current state (per NSM spec, SM_UNMON returns sm_stat)
	return encodeStatResponse(state)
}

// encodeStatResponse returns an sm_stat response with the current state.
func encodeStatResponse(state int32) (*HandlerResult, error) {
	response := &types.SMStat{
		State: state,
	}

	encoded, err := xdr.EncodeSMStat(response)
	if err != nil {
		// If encoding fails, return empty response
		return &HandlerResult{
			Data:      []byte{},
			NSMStatus: types.StatSucc,
		}, nil
	}

	return &HandlerResult{
		Data:      encoded,
		NSMStatus: types.StatSucc,
	}, nil
}
