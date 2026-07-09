package smb

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/adapter/auxsvc"
	"github.com/marmos91/dittofs/pkg/discovery/hostinfo"
	"github.com/marmos91/dittofs/pkg/discovery/mdns"
	"github.com/marmos91/dittofs/pkg/discovery/wsd"
)

// This file wires the SMB adapter's network-discovery advertisers (issue #1609)
// into the shared auxsvc.Group: the mDNS advertiser here, and — in Phase 2 —
// the WS-Discovery responder. Each is gated by a live setting and can be toggled
// at runtime without an adapter restart.

// startEnabledDiscovery seeds the auxsvc group with the Serve context and starts
// whichever discovery advertisers are enabled. Called once from Serve.
func (s *Adapter) startEnabledDiscovery(ctx context.Context) {
	s.sidecars.SetBaseContext(ctx)

	if s.mdnsEnabled() {
		if err := s.sidecars.Start(s.newMDNSSidecar()); err != nil {
			logger.Warn("SMB mDNS advertiser failed to start", "error", err)
		}
	}
	if s.wsDiscoveryEnabled() {
		if err := s.sidecars.Start(s.newWSDSidecar()); err != nil {
			logger.Warn("SMB WS-Discovery advertiser failed to start", "error", err)
		}
	}
}

// mdnsEnabled reports whether the SMB mDNS advertiser should run, from live
// settings (defaults false when settings are unavailable).
func (s *Adapter) mdnsEnabled() bool {
	if s.Registry == nil {
		return false
	}
	st := s.Registry.GetSMBSettings()
	return st != nil && st.MDNSEnabled
}

// wsDiscoveryEnabled reports whether the SMB WS-Discovery advertiser should run,
// from live settings (defaults false when settings are unavailable).
func (s *Adapter) wsDiscoveryEnabled() bool {
	if s.Registry == nil {
		return false
	}
	st := s.Registry.GetSMBSettings()
	return st != nil && st.WSDiscoveryEnabled
}

// discoveryName is the name to advertise: the SMB NetBIOS/computer name when
// known (domain member), else the OS-derived server name.
func (s *Adapter) discoveryName() string {
	if s.handler != nil && s.handler.NetBIOSName != "" {
		return s.handler.NetBIOSName
	}
	return hostinfo.ServerName()
}

// newMDNSSidecar builds the SMB mDNS advertiser: an _smb._tcp instance on the
// adapter's real port, plus a _device-info._tcp record whose model= TXT makes
// Finder show a server icon rather than a generic one.
func (s *Adapter) newMDNSSidecar() auxsvc.Service {
	name := s.discoveryName()
	port := uint16(s.Port())
	return mdns.NewSidecar([]mdns.ServiceRecord{
		{Instance: name, Service: "_smb._tcp", Port: port},
		{Instance: name, Service: "_device-info._tcp", Port: port, TXT: []string{"model=RackMac"}},
	})
}

// newWSDSidecar builds the SMB WS-Discovery responder, advertising the computer
// name under its NetBIOS domain (or WORKGROUP when standalone). A fresh
// AppSequence InstanceId (process start time) makes Windows treat a restart as a
// new instance.
func (s *Adapter) newWSDSidecar() auxsvc.Service {
	workgroup := ""
	if s.handler != nil {
		workgroup = s.handler.NetBIOSDomain
	}
	return wsd.NewResponder(s.discoveryName(), workgroup, uint64(time.Now().Unix()))
}

// reconcileMDNS starts or stops the mDNS advertiser to match live settings. It
// is a no-op until Serve has started the auxsvc group, so a settings-apply that
// runs before Serve (during SetRuntime) does not race the initial start.
func (s *Adapter) reconcileMDNS() {
	if s.sidecars == nil || !s.sidecars.Ready() {
		return
	}
	want := s.mdnsEnabled()
	running := s.sidecars.IsRunning(mdns.SidecarName)
	switch {
	case want && !running:
		if err := s.sidecars.Start(s.newMDNSSidecar()); err != nil {
			logger.Warn("SMB mDNS advertiser failed to start", "error", err)
		}
	case !want && running:
		if err := s.sidecars.StopOne(mdns.SidecarName); err != nil {
			logger.Debug("SMB mDNS advertiser stop reported an error", "error", err)
		}
	}
}

// reconcileWSDiscovery starts or stops the WS-Discovery advertiser to match live
// settings. Like reconcileMDNS it is a no-op until Serve has started the group.
func (s *Adapter) reconcileWSDiscovery() {
	if s.sidecars == nil || !s.sidecars.Ready() {
		return
	}
	want := s.wsDiscoveryEnabled()
	running := s.sidecars.IsRunning(wsd.SidecarName)
	switch {
	case want && !running:
		if err := s.sidecars.Start(s.newWSDSidecar()); err != nil {
			logger.Warn("SMB WS-Discovery advertiser failed to start", "error", err)
		}
	case !want && running:
		if err := s.sidecars.StopOne(wsd.SidecarName); err != nil {
			logger.Debug("SMB WS-Discovery advertiser stop reported an error", "error", err)
		}
	}
}
