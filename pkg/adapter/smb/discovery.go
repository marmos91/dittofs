package smb

import (
	"context"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/adapter/auxsvc"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
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
	s.reconcileDiscovery()
}

// reconcileDiscovery starts or stops the SMB discovery advertisers to match live
// settings. Called from Serve (initial start) and from applySMBSettings (live
// toggle); Group.Reconcile is a no-op until Serve has seeded the group.
func (s *Adapter) reconcileDiscovery() {
	if err := s.sidecars.Reconcile(mdns.SidecarName, s.mdnsEnabled(), s.newMDNSSidecar); err != nil {
		logger.Warn("SMB mDNS advertiser failed to start", "error", err)
	}
	if err := s.sidecars.Reconcile(wsd.SidecarName, s.wsDiscoveryEnabled(), s.newWSDSidecar); err != nil {
		logger.Warn("SMB WS-Discovery advertiser failed to start", "error", err)
	}
}

// smbSettings returns the live SMB adapter settings, or nil when unavailable.
func (s *Adapter) smbSettings() *models.SMBAdapterSettings {
	if s.Registry == nil {
		return nil
	}
	return s.Registry.GetSMBSettings()
}

// mdnsEnabled reports whether the SMB mDNS advertiser should run (default false).
func (s *Adapter) mdnsEnabled() bool {
	st := s.smbSettings()
	return st != nil && st.MDNSEnabled
}

// wsDiscoveryEnabled reports whether the SMB WS-Discovery advertiser should run
// (default false).
func (s *Adapter) wsDiscoveryEnabled() bool {
	st := s.smbSettings()
	return st != nil && st.WSDiscoveryEnabled
}

// discoveryName is the instance-wide name to advertise, resolved from the
// control plane (the `discovery.name` setting, defaulting to
// "DittoFS-<hostname>"). mDNS uses it verbatim; WS-Discovery folds it to a
// NetBIOS-legal computer name (see newWSDSidecar).
func (s *Adapter) discoveryName() string {
	if s.Registry != nil {
		if n := s.Registry.DiscoveryName(); n != "" {
			return n
		}
	}
	return hostinfo.DefaultDiscoveryName()
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
	// A non-empty NetBIOS domain means this server is an AD member, so Windows
	// should group it under Domain: rather than Workgroup:.
	isDomain := workgroup != ""
	// WS-Discovery renders the value as a Windows computer name, so fold it to a
	// NetBIOS-legal form (the raw instance name may contain characters or a
	// length Explorer rejects). mDNS keeps the raw name.
	name := hostinfo.NetBIOSSafe(s.discoveryName())
	// Distinct, strictly-increasing InstanceId per responder build (see the
	// wsdInstanceID field) so a live toggle never rewinds the AppSequence.
	return wsd.NewResponder(name, workgroup, isDomain, s.wsdInstanceID.Add(1))
}

// Compile-time assertions that the discovery advertisers satisfy auxsvc.Service.
// They satisfy it structurally (pkg/discovery does not import the adapter
// layer), so asserting here breaks a signature drift at build time.
var (
	_ auxsvc.Service = (*mdns.Sidecar)(nil)
	_ auxsvc.Service = (*wsd.Responder)(nil)
)
