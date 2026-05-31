package handlers

import (
	"net"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/xdr"
	"github.com/marmos91/dittofs/internal/logger"
)

// Notify handles NSM NOTIFY (X/Open NSM, SM procedure 6).
//
// An inbound SM_NOTIFY claims that a peer host (stat_chge.mon_name) has changed
// state (typically rebooted) and is the trigger for relaying lock-recovery
// callbacks to local clients and releasing the rebooted host's stale NLM locks.
//
// Because acting on a NOTIFY drops locks, an UNAUTHENTICATED NOTIFY is the
// classic rpc.statd spoofing primitive: any reachable host could forge
// SM_NOTIFY mon_name=<victim> and cause the server to drop the victim's
// byte-range locks, after which another client grabs the range -> silent
// corruption. Notify therefore gates every NOTIFY behind BOTH:
//
//	(1) monitored-list membership + source-address match (H16): the mon_name
//	    must correspond to a host we are actually monitoring (an SM_MON
//	    registration), AND the RPC source address must match that host's
//	    recorded address; and
//	(2) state-number monotonicity (H17): the incoming state must strictly
//	    exceed the last-seen state for that mon_name, otherwise it is a
//	    replay/stale notification.
//
// A NOTIFY that fails either gate is dropped silently (no side effects).
//
// SM_NOTIFY is a one-way procedure; it returns no meaningful result. Decode
// errors and rejected notifications are logged but never fail the RPC.
func (h *Handler) Notify(ctx *NSMHandlerContext, data []byte) (*HandlerResult, error) {
	// SM_NOTIFY returns no result regardless of outcome.
	ack := &HandlerResult{
		Data:      []byte{},
		NSMStatus: types.StatSucc,
	}

	// Decode stat_chge argument.
	r := newBytesReader(data)
	statChge, err := xdr.DecodeStatChge(r)
	if err != nil {
		logger.Warn("NSM NOTIFY decode error",
			"client", ctx.ClientAddr,
			"error", err)
		return ack, nil
	}

	logger.Debug("NSM NOTIFY received",
		"client", ctx.ClientAddr,
		"mon_name", statChge.MonName,
		"new_state", statChge.State)

	// ---- Gate 1: monitored-list membership + source-address match (H16) ----
	if !h.isMonitoredFromSource(statChge.MonName, ctx.ClientAddr) {
		// Either we are not monitoring this host (unknown mon_name) or the
		// NOTIFY arrived from an address that does not match the monitored
		// host's recorded address. Treat as a spoofing attempt: drop silently.
		logger.Warn("NSM NOTIFY rejected: failed monitored-list/source-addr gate",
			"client", ctx.ClientAddr,
			"mon_name", statChge.MonName)
		return ack, nil
	}

	// ---- Gate 2: state-number monotonicity (H17) ----
	if !h.admitPeerState(statChge.MonName, statChge.State) {
		// Replay or stale notification (state did not advance). Drop silently.
		logger.Warn("NSM NOTIFY ignored: non-monotonic state (replay/stale)",
			"client", ctx.ClientAddr,
			"mon_name", statChge.MonName,
			"new_state", statChge.State)
		return ack, nil
	}

	logger.Info("NSM NOTIFY accepted",
		"client", ctx.ClientAddr,
		"mon_name", statChge.MonName,
		"new_state", statChge.State)

	// Relay / lock-recovery dispatch is intentionally NOT implemented here.
	//
	// This PR establishes the security gates (above) so that a future relay is
	// safe by construction: any relay implementation MUST run only after BOTH
	// gates have passed. When the relay ships it should, for the validated
	// mon_name:
	//   1. find local registrations monitoring this host,
	//   2. send SM_NOTIFY callbacks to those clients, and
	//   3. release the rebooted host's stale NLM locks,
	// all gated by the checks performed above.

	return ack, nil
}

// isMonitoredFromSource enforces the H16 gate. It returns true only when:
//
//	(1) monName is on the monitored list (some SM_MON registration records
//	    MonName == monName), AND
//	(2) the NOTIFY's source address (srcAddr) matches the source IP of at
//	    least one of those monitoring registrations.
//
// Both srcAddr and the recorded registration address are "host:port" strings
// (net.Conn.RemoteAddr().String()); only the host (IP) portion is compared,
// because an NSM peer sends NOTIFY from a different ephemeral port than it used
// for SM_MON.
//
// A monName with no matching registration (unmonitored host) or whose
// registrations all have a different source IP (spoofed source) returns false.
func (h *Handler) isMonitoredFromSource(monName, srcAddr string) bool {
	srcIP := hostOf(srcAddr)
	if srcIP == "" {
		return false
	}

	for _, reg := range h.tracker.GetNSMClients() {
		if reg.MonName == monName && hostOf(reg.RemoteAddr) == srcIP {
			return true
		}
	}

	// Either monName is unknown (unmonitored host) or every monitoring
	// registration for it has a different source IP (spoofed source). Reject.
	return false
}

// hostOf extracts the host (IP) portion of a "host:port" address. If the input
// has no port it is returned unchanged. An empty input returns "".
func hostOf(addr string) string {
	if addr == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Not in host:port form; treat the whole string as the host.
		return addr
	}
	return host
}
