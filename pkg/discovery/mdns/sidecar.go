package mdns

import (
	"context"
	"sync"
)

// SidecarName is the auxsvc.Group key used for the mDNS advertiser on every
// adapter.
const SidecarName = "mdns"

// Sidecar advertises a fixed set of services through the shared Responder for
// the lifetime of one adapter. It satisfies the adapter auxsvc.Service interface
// structurally (Name/Start/Stop) so the discovery package needs no dependency on
// the adapter layer.
//
// Start/Stop ignore the context: the shared Responder's socket lifetime is
// reference-counted across all registrations, not tied to any single adapter's
// context. The adapter's auxsvc.Group calls Stop (Unregister) on shutdown or a
// live disable.
type Sidecar struct {
	services []ServiceRecord

	mu     sync.Mutex
	handle *Handle
}

// NewSidecar builds an mDNS advertiser for the given services. It registers with
// the process-global Responder on Start.
func NewSidecar(services []ServiceRecord) *Sidecar {
	return &Sidecar{services: services}
}

// Name implements auxsvc.Service.
func (s *Sidecar) Name() string { return SidecarName }

// Start registers the services with the shared Responder (idempotent).
func (s *Sidecar) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle != nil {
		return nil
	}
	h, err := Shared().Register(s.services)
	if err != nil {
		return err
	}
	s.handle = h
	return nil
}

// Stop withdraws the services (idempotent).
func (s *Sidecar) Stop(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle != nil {
		s.handle.Unregister()
		s.handle = nil
	}
	return nil
}
