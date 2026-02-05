package handlers

import (
	"bytes"
	"io"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nsm/types"
	"github.com/marmos91/dittofs/internal/protocol/nsm/xdr"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// Mon handles the SM_MON procedure (procedure 2).
//
// SM_MON registers the caller to receive notifications when a monitored
// host's state changes (i.e., when it crashes and restarts).
//
// The registration includes:
//   - MonName: The host being monitored (typically this server)
//   - MyID: Callback RPC info (where to send SM_NOTIFY)
//   - Priv: 16-byte private data returned in callbacks
//
// On success:
//   - Registers the client in the ConnectionTracker with NSM callback info
//   - Persists the registration to ClientRegistrationStore (if configured)
//   - Returns STAT_SUCC with current server state
//
// On failure:
//   - Returns STAT_FAIL if client limit exceeded or persistence fails
//
// Parameters:
//   - ctx: The NSM handler context
//   - data: XDR-encoded mon structure
//
// Returns:
//   - *HandlerResult: sm_stat_res with result and current state
//   - error: XDR decoding error if input is malformed
func (h *Handler) Mon(ctx *NSMHandlerContext, data []byte) (*HandlerResult, error) {
	state := h.GetServerState()

	// Decode mon argument
	r := newBytesReader(data)
	mon, err := xdr.DecodeMon(r)
	if err != nil {
		logger.Warn("NSM MON decode error",
			"client", ctx.ClientAddr,
			"error", err)
		return encodeStatFailure(state)
	}

	logger.Debug("NSM MON request",
		"client", ctx.ClientAddr,
		"mon_name", mon.MonID.MonName,
		"callback_host", mon.MonID.MyID.MyName,
		"callback_prog", mon.MonID.MyID.MyProg,
		"callback_vers", mon.MonID.MyID.MyVers,
		"callback_proc", mon.MonID.MyID.MyProc)

	// Generate client ID from client address and callback info
	// This ensures uniqueness per client/callback combination
	clientID := generateClientID(ctx.ClientAddr, mon.MonID.MyID.MyName)

	// Check if we're at the client limit
	currentCount := h.tracker.GetClientCount("")
	if currentCount >= h.maxClients {
		logger.Warn("NSM MON rejected: client limit reached",
			"client", ctx.ClientAddr,
			"current", currentCount,
			"max", h.maxClients)
		return encodeStatFailure(state)
	}

	// Register the client in the connection tracker
	// Use 0 TTL since NSM clients manage their own lifecycle
	err = h.tracker.RegisterClient(clientID, "nsm", ctx.ClientAddr, 0)
	if err != nil {
		logger.Warn("NSM MON registration failed",
			"client", ctx.ClientAddr,
			"error", err)
		return encodeStatFailure(state)
	}

	// Update NSM-specific info in the registration
	callback := &lock.NSMCallback{
		Hostname: mon.MonID.MyID.MyName,
		Program:  mon.MonID.MyID.MyProg,
		Version:  mon.MonID.MyID.MyVers,
		Proc:     mon.MonID.MyID.MyProc,
	}
	h.tracker.UpdateNSMInfo(clientID, mon.MonID.MonName, mon.Priv, callback)
	h.tracker.UpdateSMState(clientID, state)

	// Persist to client store if configured
	if h.clientStore != nil {
		reg, _ := h.tracker.GetClient(clientID)
		if reg != nil {
			persisted := lock.ToPersistedClientRegistration(reg, uint64(state))
			if err := h.clientStore.PutClientRegistration(ctx.Context, persisted); err != nil {
				logger.Warn("NSM MON persistence failed",
					"client", ctx.ClientAddr,
					"error", err)
				// Don't fail the registration - memory registration succeeded
			}
		}
	}

	logger.Info("NSM MON registered",
		"client_id", clientID,
		"mon_name", mon.MonID.MonName,
		"callback_host", mon.MonID.MyID.MyName,
		"state", state)

	// Return success with current state
	response := &types.SMStatRes{
		Result: types.StatSucc,
		State:  state,
	}

	encoded, err := xdr.EncodeSMStatRes(response)
	if err != nil {
		logger.Error("NSM MON encode error",
			"client", ctx.ClientAddr,
			"error", err)
		return encodeStatFailure(state)
	}

	return &HandlerResult{
		Data:      encoded,
		NSMStatus: types.StatSucc,
	}, nil
}

// generateClientID creates a unique client identifier from address and callback host.
//
// Format: "nsm:{client_addr}:{callback_host}"
//
// This ensures each client/callback combination has a unique registration.
func generateClientID(clientAddr, callbackHost string) string {
	return "nsm:" + clientAddr + ":" + callbackHost
}

// newBytesReader creates an io.Reader from a byte slice.
func newBytesReader(data []byte) io.Reader {
	return bytes.NewReader(data)
}

// Helper to get time for registration (allows mocking in tests)
var nowFunc = time.Now
