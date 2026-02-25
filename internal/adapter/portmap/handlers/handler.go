// Package handlers provides portmapper procedure handler implementations.
//
// Each handler corresponds to a portmap v2 procedure (RFC 1057 Section A).
// Handlers delegate to a PortmapRegistry interface for data access,
// avoiding an import cycle with the root portmap package.
package handlers

import (
	"net"

	"github.com/marmos91/dittofs/internal/adapter/portmap/xdr"
)

// PortmapRegistry defines the operations the handler needs from a service registry.
// The portmap.Registry type satisfies this interface.
type PortmapRegistry interface {
	Set(m *xdr.Mapping) bool
	Unset(prog, vers, prot uint32) bool
	Getport(prog, vers, prot uint32) uint32
	Dump() []*xdr.Mapping
}

// Handler processes portmap procedure calls using a shared registry.
type Handler struct {
	Registry PortmapRegistry
}

// NewHandler creates a new Handler with the given service registry.
func NewHandler(registry PortmapRegistry) *Handler {
	return &Handler{
		Registry: registry,
	}
}

// IsLocalhost returns true if the given address is a loopback address.
// This is used to restrict SET/UNSET procedures to localhost only,
// per standard portmapper security practices.
func IsLocalhost(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Try treating the whole string as an IP (no port)
		host = addr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
