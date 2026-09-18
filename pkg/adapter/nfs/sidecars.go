package nfs

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/adapter/sidecar"
	"github.com/marmos91/dittofs/pkg/discovery/hostinfo"
	"github.com/marmos91/dittofs/pkg/discovery/mdns"
)

// This file adapts the NFS sidecar services (portmapper, system
// rpcbind registration, the UDP lock-manager transport, and NSM startup) to the
// shared sidecar.Service interface. Each wrapper is a thin, stateless shim over
// the adapter methods that already implement the behavior — the protocol logic
// is unchanged; only the lifecycle is unified so every companion is started and
// stopped uniformly through the adapter's sidecar.Group, so the
// mDNS / WS-Discovery advertisers can join the same pattern).
//
// startEnabledSidecars registers every enabled companion with the group, in
// the order the adapter historically started them. The group tears them down in
// reverse order in Stop, which preserves the one ordering that matters:
// unregistering from the system rpcbind before the embedded portmapper closes.
func (s *NFSAdapter) startEnabledSidecars(ctx context.Context) {
	s.sidecars.SetBaseContext(ctx)

	// Embedded portmapper (RFC 1057). Non-fatal: privileged ports may need root.
	if s.isPortmapperEnabled() {
		if err := s.sidecars.Start(portmapSidecar{s}); err != nil {
			logger.Warn("Portmapper failed to start (NFS will continue without it)", "error", err)
		}
	} else {
		logger.Info("Portmapper disabled by configuration")
	}

	// System rpcbind registration (port 111). Best-effort and time-bounded.
	s.reconcileSysreg()

	// UDP transport for NLM/NSM/MOUNT. Non-fatal: TCP continues.
	if s.isUDPEnabled() {
		if err := s.sidecars.Start(udpSidecar{s}); err != nil {
			logger.Warn("NFS UDP transport failed to start (NLM/NSM/MOUNT over UDP unavailable)", "error", err)
		}
	}

	// NSM startup notifier (SM_NOTIFY on restart). No-op when uninitialized.
	if s.nsmNotifier != nil {
		_ = s.sidecars.Start(nsmSidecar{s})
	}

	// mDNS advertiser (_nfs._tcp) for macOS Finder / Linux Avahi.
	// Live-toggled via NFS settings; the initial start happens here.
	s.reconcileDiscovery()
}

// discoveryName is the instance-wide name to advertise, resolved from the
// control plane (the `discovery.name` setting, defaulting to
// "DittoFS-<hostname>"). NFS advertises only over mDNS, which uses it verbatim.
func (s *NFSAdapter) discoveryName(ctx context.Context) string {
	if s.Registry != nil {
		if n := s.Registry.DiscoveryName(ctx); n != "" {
			return n
		}
	}
	return hostinfo.DefaultDiscoveryName()
}

// mdnsEnabled reports whether the NFS mDNS advertiser should run, from live
// settings (defaults false when settings are unavailable).
func (s *NFSAdapter) mdnsEnabled() bool {
	if s.Registry == nil {
		return false
	}
	settings := s.Registry.GetNFSSettings()
	return settings != nil && settings.MDNSEnabled
}

// newMDNSSidecar builds the NFS mDNS advertiser: an _nfs._tcp instance named
// after the server, on the adapter's real port (12049), with a path= TXT naming
// an export so Finder mounts a valid share.
//
// The path is the lexicographically-first export, chosen deterministically
// (ListShares order is not defined). Limitation: the record is built when the
// advertiser starts and is not rebuilt when shares change, so renaming/removing
// that export leaves a stale path hint until the adapter or advertiser restarts;
// re-advertising on share changes is a follow-up.
func (s *NFSAdapter) newMDNSSidecar(ctx context.Context) sidecar.Service {
	rec := mdns.ServiceRecord{
		Instance: s.discoveryName(ctx),
		Service:  "_nfs._tcp",
		Port:     uint16(s.Port()),
	}
	if s.Registry != nil {
		if shares := s.Registry.ListShares(); len(shares) > 0 {
			first := shares[0]
			for _, sh := range shares[1:] {
				if sh < first {
					first = sh
				}
			}
			rec.TXT = []string{"path=/" + first}
		}
	}
	return mdns.NewSidecar([]mdns.ServiceRecord{rec})
}

// reconcileDiscovery starts or stops the NFS discovery advertiser(s) to match
// live settings. Called from Serve (initial start) and from applyNFSSettings
// (live toggle); Group.Reconcile is a no-op until Serve has seeded the group.
func (s *NFSAdapter) reconcileDiscovery() {
	if err := s.sidecars.Reconcile(mdns.SidecarName, s.mdnsEnabled(), s.newMDNSSidecar); err != nil {
		logger.Warn("NFS mDNS advertiser failed to start", "error", err)
	}
}

// portmapSidecar wraps the embedded RFC 1057 portmapper.
type portmapSidecar struct{ a *NFSAdapter }

func (p portmapSidecar) Name() string                    { return "portmapper" }
func (p portmapSidecar) Start(ctx context.Context) error { return p.a.startPortmapper(ctx) }
func (p portmapSidecar) Stop(context.Context) error      { p.a.stopPortmapper(); return nil }

// Values of NFSAdapter.sysregState.
const (
	sysregIdle int32 = iota
	sysregRunning
	sysregDirty
)

// reconcileSysreg starts or stops the host-rpcbind registration to match the
// live setting, so toggling it takes effect without restarting the adapter.
// Called from Serve (initial start) and from applyNFSSettings (live toggle);
// Group.Reconcile is a no-op until Serve has seeded the group.
//
// The transition itself talks to rpcbind and is bounded by systemRegTimeout, so
// it runs in the background: the callers are the accept loop and the
// settings-watcher goroutine, and an unreachable rpcbind must not stall either.
// Reconcile tolerates a start racing shutdown, and the steady-state check keeps
// the per-connection call (every accepted connection) allocation-free.
//
// A caller that finds a transition already in flight cannot simply return: the
// running transition captured the setting as it was when it started, and an
// unreachable rpcbind holds it there for up to systemRegTimeout. Dropping the
// flip would leave the sidecar in the state the user just turned off until some
// unrelated call happened to reconcile again. Such a caller marks the in-flight
// transition dirty instead, and the transition makes another pass.
func (s *NFSAdapter) reconcileSysreg() {
	// Steady state: nothing is in flight, and the sidecar already matches the
	// setting. Every accepted connection lands here.
	//
	// The order of these two reads is load-bearing, and not interchangeable.
	// Observing idle FIRST is what makes the running state read after it
	// trustworthy: a transition holds the state non-idle from its claim until
	// after its mutation is visible, so idle means no mutation is in flight,
	// and any transition claimed afterwards re-reads the setting this caller
	// has already written. Reading the running state first would let a caller
	// see the value from before an in-flight mutation and the idle that the
	// same transition published after settling, conclude "steady", and return
	// with its flip applied nowhere.
	want := s.registerWithSystemEnabled()
	if s.sysregState.Load() == sysregIdle && s.sidecars.IsRunning(sysregSidecarName) == want {
		return
	}
	// Nothing can be applied once the group has been torn down: StopAll clears
	// the base context and empties the running set, so Reconcile no-ops and the
	// steady-state check above can never match again. Without this the accept
	// loop would claim a transition per connection for the rest of the
	// process's life — and the listener outlives StopAll, since Stop tears the
	// sidecars down before closing it. Serve seeds the group before its own
	// reconcile, so the initial start still runs.
	if !s.sidecars.Ready() {
		return
	}
	// Claim the transition, or hand this flip to the one already running: the
	// swap marks dirty either way, and only the caller that found it idle owns
	// the goroutine.
	if s.sysregState.Swap(sysregDirty) != sysregIdle {
		return
	}
	go func() {
		for {
			// Re-read per pass rather than capturing once: the value that
			// matters is the one current when the pass begins, and a flip that
			// landed during the previous pass is exactly what dirty records.
			// Marking running before the read is what makes that true.
			s.sysregState.Store(sysregRunning)
			want := s.registerWithSystemEnabled()
			err := s.sidecars.Reconcile(sysregSidecarName, want,
				func(context.Context) sidecar.Service { return sysregSidecar{s} })
			if err != nil {
				logger.Debug("System rpcbind registration sidecar failed to start", "error", err)
			}
			// Settle only if no flip arrived while this pass was running. The
			// CAS is what closes the window: a caller marking dirty between the
			// read above and here loses the CAS and gets another pass.
			if s.sysregState.CompareAndSwap(sysregRunning, sysregIdle) {
				return
			}
		}
	}()
}

const sysregSidecarName = "sysreg"

// sysregSidecar wraps registration of DittoFS's services with the host rpcbind.
// Start registers; Stop unregisters. Both are best-effort and self-gating.
type sysregSidecar struct{ a *NFSAdapter }

func (r sysregSidecar) Name() string { return sysregSidecarName }
func (r sysregSidecar) Start(ctx context.Context) error {
	r.a.startSystemPortmapRegistration(ctx)
	return nil
}
func (r sysregSidecar) Stop(context.Context) error { r.a.stopSystemPortmapRegistration(); return nil }

// udpSidecar wraps the NLM/NSM/MOUNT-over-UDP transport. Start binds the socket
// and launches the read loop; Stop claims the conn and its shutdown cancel,
// then closes the socket so the loop unblocks and exits (startUDP also closes
// it when the per-generation ctx or the adapter ctx fires, so a double close
// is possible and harmless).
type udpSidecar struct{ a *NFSAdapter }

func (u udpSidecar) Name() string                    { return "nfs-udp" }
func (u udpSidecar) Start(ctx context.Context) error { return u.a.startUDP(ctx) }
func (u udpSidecar) Stop(ctx context.Context) error {
	// Claim conn, cancel func and done channel together: each generation is
	// stopped exactly once (a disable racing a re-enable can no longer snapshot
	// the newer conn) and the per-generation shutdown ctx fires so the
	// conn-close waiter and the read loop exit instead of parking until adapter
	// shutdown.
	u.a.sidecarMu.Lock()
	udpConn := u.a.udpConn
	udpStop := u.a.udpStop
	udpDone := u.a.udpDone
	u.a.udpConn = nil
	u.a.udpStop = nil
	u.a.sidecarMu.Unlock()
	if udpStop != nil {
		udpStop()
	}
	if udpConn != nil {
		_ = udpConn.Close()
	}
	// Closing the socket only unblocks the read: the loop and the datagram
	// handlers it spawned are still running, and they touch adapter and runtime
	// state that teardown is about to dismantle. Wait for them, bounded by the
	// caller's context so one wedged handler cannot hold shutdown open forever.
	// A nil channel means nothing was bound, so there is nothing to wait for.
	if udpDone != nil {
		select {
		case <-udpDone:
		case <-ctx.Done():
			// udpDone is deliberately left published. Clearing it with the
			// others would strand this generation: the handlers outlive the
			// timeout, and a later Stop with more time would find nothing to
			// join and report the transport down while they still run.
			return fmt.Errorf("nfs-udp: datagram handlers still running: %w", ctx.Err())
		}
	}
	// Joined (or never bound): retire the generation so a re-enable starts clean.
	u.a.sidecarMu.Lock()
	if u.a.udpDone == udpDone {
		u.a.udpDone = nil
	}
	u.a.sidecarMu.Unlock()
	return nil
}

// nsmSidecar wraps NSM startup: it loads persisted registrations and sends
// SM_NOTIFY to recover locks after a restart. It is a startup task with no
// long-running listener, so Stop is a no-op (its background notification
// goroutine is bounded by the adapter's Serve context).
type nsmSidecar struct{ a *NFSAdapter }

func (n nsmSidecar) Name() string { return "nsm" }
func (n nsmSidecar) Start(ctx context.Context) error {
	n.a.performNSMStartup(ctx)
	return nil
}
func (n nsmSidecar) Stop(context.Context) error { return nil }

// Compile-time assertions that every wrapper satisfies sidecar.Service. The mDNS
// sidecar is asserted here too so a signature drift in pkg/discovery/mdns (which
// satisfies the interface structurally, without importing it) breaks at build.
var (
	_ sidecar.Service = portmapSidecar{}
	_ sidecar.Service = sysregSidecar{}
	_ sidecar.Service = udpSidecar{}
	_ sidecar.Service = nsmSidecar{}
	_ sidecar.Service = (*mdns.Sidecar)(nil)
)
