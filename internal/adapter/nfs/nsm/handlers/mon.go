package handlers

import (
	"bytes"
	"fmt"
	"io"
	"net"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/xdr"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// callbackHostFromSource derives the SM_NOTIFY callback host from the SM_MON
// request's transport source address.
//
// NSM clients set my_name to their own hostname — Linux and macOS statd both
// send a name like "localhost", not an IP literal — so dialling my_name
// verbatim breaks every real client. It is also an SSRF vector: my_name is
// fully client-controlled and could point the later callback at an internal
// address such as the cloud metadata endpoint (169.254.169.254). The request's
// transport source is the host that actually issued the MON, so it is both the
// correct callback target and inherently safe (a client cannot forge it to an
// arbitrary internal address). my_name is therefore kept only as a label.
//
// Link-local sources (169.254.0.0/16, fe80::/10) are still rejected; loopback
// is allowed so same-host clients (and local testing) can lock.
func callbackHostFromSource(clientAddr string) (string, error) {
	host, _, err := net.SplitHostPort(clientAddr)
	if err != nil {
		host = clientAddr // already a bare host
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", fmt.Errorf("client source %q is not an IP address", clientAddr)
	}
	if ip.IsLinkLocalUnicast() {
		return "", fmt.Errorf("client source %q is a link-local address", host)
	}
	return host, nil
}

// Mon handles NSM MON (RFC 1813, SM procedure 2).
// Registers caller for SM_NOTIFY callbacks when a monitored host's state changes.
// Delegates to ConnectionTracker.RegisterClient and persists to ClientRegistrationStore.
// Adds NSM callback registration; updates tracker and optional persistent store.
// Errors: STAT_FAIL (client limit exceeded, persistence failure, XDR decode error).
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

	// Derive the SM_NOTIFY callback host from the request's transport source
	// rather than the client-supplied my_name (which is a hostname like
	// "localhost" and an SSRF vector). See callbackHostFromSource.
	callbackHost, err := callbackHostFromSource(ctx.ClientAddr)
	if err != nil {
		logger.Warn("NSM MON rejected: unsafe source address",
			"client", ctx.ClientAddr,
			"my_name", mon.MonID.MyID.MyName,
			"error", err)
		return encodeStatFailure(state)
	}

	// Generate client ID from client address and the derived callback host.
	// This ensures uniqueness per client/callback combination.
	clientID := generateClientID(ctx.ClientAddr, callbackHost)

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
		Hostname: callbackHost,
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
		"callback_host", callbackHost,
		"my_name", mon.MonID.MyID.MyName,
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
