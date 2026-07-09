package nfs

import (
	"context"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/adapter/auxsvc"
	"github.com/marmos91/dittofs/pkg/discovery/hostinfo"
	"github.com/marmos91/dittofs/pkg/discovery/mdns"
)

// This file adapts the NFS auxiliary/companion services (portmapper, system
// rpcbind registration, the UDP lock-manager transport, and NSM startup) to the
// shared auxsvc.Service interface. Each wrapper is a thin, stateless shim over
// the adapter methods that already implement the behavior — the protocol logic
// is unchanged; only the lifecycle is unified so every companion is started and
// stopped uniformly through the adapter's auxsvc.Group (and, for #1609, so the
// mDNS / WS-Discovery advertisers can join the same pattern).
//
// startEnabledAuxServices registers every enabled companion with the group, in
// the order the adapter historically started them. The group tears them down in
// reverse order in Stop, which preserves the one ordering that matters:
// unregistering from the system rpcbind before the embedded portmapper closes.
func (s *NFSAdapter) startEnabledAuxServices(ctx context.Context) {
	s.sidecars.SetBaseContext(ctx)

	// Embedded portmapper (RFC 1057). Non-fatal: privileged ports may need root.
	if s.isPortmapperEnabled() {
		if err := s.sidecars.Start(portmapSidecar{s}); err != nil {
			logger.Warn("Portmapper failed to start (NFS will continue without it)", "error", err)
		}
	} else {
		logger.Info("Portmapper disabled by configuration")
	}

	// System rpcbind registration (port 111). Best-effort and time-bounded;
	// sysregSidecar.Start never returns an error, so the branch is defensive.
	if s.registerWithSystemEnabled() {
		if err := s.sidecars.Start(sysregSidecar{s}); err != nil {
			logger.Debug("System rpcbind registration sidecar failed to start", "error", err)
		}
	}

	// UDP transport for NLM/NSM/MOUNT (issue #1353). Non-fatal: TCP continues.
	if s.isUDPEnabled() {
		if err := s.sidecars.Start(udpSidecar{s}); err != nil {
			logger.Warn("NFS UDP transport failed to start (NLM/NSM/MOUNT over UDP unavailable)", "error", err)
		}
	}

	// NSM startup notifier (SM_NOTIFY on restart). No-op when uninitialized.
	if s.nsmNotifier != nil {
		_ = s.sidecars.Start(nsmSidecar{s})
	}

	// mDNS advertiser (_nfs._tcp) for macOS Finder / Linux Avahi (issue #1609).
	// Live-toggled via NFS settings; the initial start happens here.
	s.reconcileDiscovery()
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
// the first export so Finder mounts the right share.
func (s *NFSAdapter) newMDNSSidecar() auxsvc.Service {
	rec := mdns.ServiceRecord{
		Instance: hostinfo.ServerName(),
		Service:  "_nfs._tcp",
		Port:     uint16(s.Port()),
	}
	if s.Registry != nil {
		if shares := s.Registry.ListShares(); len(shares) > 0 {
			rec.TXT = []string{"path=/" + shares[0]}
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

// sysregSidecar wraps registration of DittoFS's services with the host rpcbind.
// Start registers; Stop unregisters. Both are best-effort and self-gating.
type sysregSidecar struct{ a *NFSAdapter }

func (r sysregSidecar) Name() string { return "sysreg" }
func (r sysregSidecar) Start(ctx context.Context) error {
	r.a.startSystemPortmapRegistration(ctx)
	return nil
}
func (r sysregSidecar) Stop(context.Context) error { r.a.stopSystemPortmapRegistration(); return nil }

// udpSidecar wraps the NLM/NSM/MOUNT-over-UDP transport. Start binds the socket
// and launches the read loop; Stop closes the socket so the loop unblocks and
// exits (startUDP also closes it on ctx cancel, so a double close is possible
// and harmless).
type udpSidecar struct{ a *NFSAdapter }

func (u udpSidecar) Name() string                    { return "nfs-udp" }
func (u udpSidecar) Start(ctx context.Context) error { return u.a.startUDP(ctx) }
func (u udpSidecar) Stop(context.Context) error {
	if u.a.udpConn != nil {
		_ = u.a.udpConn.Close()
	}
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

// Compile-time assertions that every wrapper satisfies auxsvc.Service. The mDNS
// sidecar is asserted here too so a signature drift in pkg/discovery/mdns (which
// satisfies the interface structurally, without importing it) breaks at build.
var (
	_ auxsvc.Service = portmapSidecar{}
	_ auxsvc.Service = sysregSidecar{}
	_ auxsvc.Service = udpSidecar{}
	_ auxsvc.Service = nsmSidecar{}
	_ auxsvc.Service = (*mdns.Sidecar)(nil)
)
