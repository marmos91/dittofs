package handlers

import (
	"net"
	"sync"
	"sync/atomic"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/xdr"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
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

	// Both gates passed: relay the state change to every local monitor that
	// registered (via SM_MON) for this mon_name. Each callback carries the
	// monitor's own stored priv data so its lock manager (lockd/NLM) can
	// reclaim or release the rebooted host's locks. Lock release proper is the
	// callback recipient's responsibility, not ours.
	h.dispatchNotify(ctx, statChge.MonName, statChge.State)

	return ack, nil
}

// dispatchNotify sends an SM_NOTIFY callback to every local registration that
// monitors monName. It runs ONLY after both H16 and H17 gates have passed.
//
// For each matching registration it builds a status carrying:
//   - mon_name = the rebooted peer (monName), so the recipient knows whose
//     locks to reclaim;
//   - state    = the new peer state from the notification;
//   - priv     = the registration's own 16-byte priv from SM_MON, returned
//     unchanged so the recipient can correlate recovery context.
//
// The callback uses the prog/vers/proc the monitor supplied at SM_MON time.
// Success and failure are counted and logged for observability; a failed
// callback never aborts the remaining ones. With no dispatcher configured the
// relay is a no-op (gates already enforced).
func (h *Handler) dispatchNotify(ctx *NSMHandlerContext, monName string, state int32) {
	if h.dispatcher == nil {
		logger.Debug("NSM NOTIFY relay skipped: no dispatcher configured",
			"mon_name", monName)
		return
	}

	targets := h.monitorsFor(monName)
	if len(targets) == 0 {
		// Gate 1 guaranteed at least one registration matched mon_name; a zero
		// count here means it was unregistered between the gate and now.
		logger.Debug("NSM NOTIFY relay: no current monitors", "mon_name", monName)
		return
	}

	// Fire callbacks in parallel: each Send carries its own (up to 5s) timeout,
	// so serial dispatch would let one slow monitor stall the rest. This
	// mirrors Notifier.NotifyAllClients.
	var (
		wg           sync.WaitGroup
		sent, failed atomic.Int64
		attempted    int
	)
	for _, reg := range targets {
		if reg.CallbackInfo == nil {
			// No callback target recorded; nothing to send to.
			continue
		}
		attempted++
		wg.Add(1)
		go func(reg *lock.ClientRegistration) {
			defer wg.Done()
			status := &types.Status{
				MonName: monName,
				State:   state,
				Priv:    reg.Priv,
			}
			err := h.dispatcher.Send(
				ctx.Context,
				reg.CallbackInfo.Hostname,
				status,
				reg.CallbackInfo.Proc,
				reg.CallbackInfo.Program,
				reg.CallbackInfo.Version,
			)
			if err != nil {
				failed.Add(1)
				logger.Warn("NSM NOTIFY callback failed",
					"mon_name", monName,
					"client_id", reg.ClientID,
					"callback_host", reg.CallbackInfo.Hostname,
					"error", err)
				return
			}
			sent.Add(1)
			logger.Debug("NSM NOTIFY callback sent",
				"mon_name", monName,
				"client_id", reg.ClientID,
				"callback_host", reg.CallbackInfo.Hostname)
		}(reg)
	}
	wg.Wait()

	logger.Info("NSM NOTIFY relayed",
		"mon_name", monName,
		"new_state", state,
		"monitors", attempted,
		"sent", sent.Load(),
		"failed", failed.Load())
}

// monitorsFor returns all local registrations monitoring monName. It mirrors
// the membership half of the H16 gate (mon_name match) over the tracker's
// snapshot of NSM clients.
func (h *Handler) monitorsFor(monName string) []*lock.ClientRegistration {
	var matches []*lock.ClientRegistration
	for _, reg := range h.tracker.GetNSMClients() {
		if reg.MonName == monName {
			matches = append(matches, reg)
		}
	}
	return matches
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
