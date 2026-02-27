// Package portmap provides an embedded portmapper (RFC 1057) service registry.
//
// The Registry stores program/version/protocol -> port mappings that NFS clients
// query via rpcinfo or showmount. DittoFS runs this on a dedicated port (default
// 10111), eliminating the need for a system-level rpcbind/portmap daemon.
//
// References:
//   - RFC 1057 Section A (Port Mapper Program Protocol)
package portmap

import (
	"cmp"
	"slices"
	"sync"

	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/xdr"
)

// registryKey uniquely identifies a service registration.
// Per RFC 1057, a mapping is keyed by (program, version, protocol).
type registryKey struct {
	prog uint32
	vers uint32
	prot uint32
}

// Registry is a thread-safe in-memory store for portmap service registrations.
// It maps (program, version, protocol) tuples to port numbers.
type Registry struct {
	mu       sync.RWMutex
	mappings map[registryKey]*xdr.Mapping
}

// NewRegistry creates a new empty portmap registry.
func NewRegistry() *Registry {
	return &Registry{
		mappings: make(map[registryKey]*xdr.Mapping),
	}
}

// Set adds or replaces a mapping in the registry.
// Returns true on success, false if the port is invalid (0).
// The mapping key is (prog, vers, prot).
func (r *Registry) Set(m *xdr.Mapping) bool {
	if m.Port == 0 {
		return false
	}

	key := registryKey{prog: m.Prog, vers: m.Vers, prot: m.Prot}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Store a copy to prevent external mutation
	entry := &xdr.Mapping{
		Prog: m.Prog,
		Vers: m.Vers,
		Prot: m.Prot,
		Port: m.Port,
	}
	r.mappings[key] = entry

	return true
}

// Unset removes a mapping for the given (prog, vers, prot) tuple.
// Returns true if the mapping existed and was removed, false otherwise.
func (r *Registry) Unset(prog, vers, prot uint32) bool {
	key := registryKey{prog: prog, vers: vers, prot: prot}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.mappings[key]; !exists {
		return false
	}

	delete(r.mappings, key)
	return true
}

// Getport returns the port for a given (prog, vers, prot) tuple.
// Returns 0 if no mapping is registered (per RFC 1057, 0 means not registered).
func (r *Registry) Getport(prog, vers, prot uint32) uint32 {
	key := registryKey{prog: prog, vers: vers, prot: prot}

	r.mu.RLock()
	defer r.mu.RUnlock()

	if m, ok := r.mappings[key]; ok {
		return m.Port
	}
	return 0
}

// Dump returns all registered mappings as a snapshot.
// The returned slice is sorted by (prog, vers, prot) for deterministic output.
// Order does not matter per RFC 1057, but deterministic ordering aids testing.
func (r *Registry) Dump() []*xdr.Mapping {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*xdr.Mapping, 0, len(r.mappings))
	for _, m := range r.mappings {
		// Return copies to prevent external mutation
		entry := &xdr.Mapping{
			Prog: m.Prog,
			Vers: m.Vers,
			Prot: m.Prot,
			Port: m.Port,
		}
		result = append(result, entry)
	}

	// Sort for deterministic output: by prog, then vers, then prot
	slices.SortFunc(result, func(a, b *xdr.Mapping) int {
		if c := cmp.Compare(a.Prog, b.Prog); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Vers, b.Vers); c != 0 {
			return c
		}
		return cmp.Compare(a.Prot, b.Prot)
	})

	return result
}

// RegisterDittoFSServices registers all DittoFS RPC services on the given port.
// This populates the portmap registry with NFS, MOUNT, NLM, and NSM services
// on TCP only (DittoFS NFS adapter is TCP-only, no UDP transport).
//
// Registered services:
//   - NFS (100003) v3 and v4 on TCP
//   - MOUNT (100005) v1, v2, and v3 on TCP (v1/v2 for client compatibility)
//   - NLM (100021) v4 on TCP
//   - NSM (100024) v1 on TCP
//
// This results in 7 mappings total (7 program/version pairs x TCP).
func (r *Registry) RegisterDittoFSServices(nfsPort int) {
	port := uint32(nfsPort)

	type svc struct {
		prog uint32
		vers uint32
	}

	services := []svc{
		{100003, 3}, // NFS v3
		{100003, 4}, // NFS v4
		{100005, 1}, // MOUNT v1 (client compatibility)
		{100005, 2}, // MOUNT v2 (client compatibility)
		{100005, 3}, // MOUNT v3
		{100021, 4}, // NLM v4
		{100024, 1}, // NSM v1
	}

	for _, s := range services {
		r.Set(&xdr.Mapping{Prog: s.prog, Vers: s.vers, Prot: types.ProtoTCP, Port: port})
	}
}

// RegisterPortmapper registers the portmapper itself in the service registry.
// Per RFC 1057, the portmapper should advertise its own presence so that
// clients querying via DUMP or GETPORT can discover it.
//
// Registered mappings:
//   - Portmapper (100000) v2 on TCP and UDP
func (r *Registry) RegisterPortmapper(portmapPort int) {
	port := uint32(portmapPort)
	r.Set(&xdr.Mapping{Prog: types.ProgramPortmap, Vers: types.PortmapVersion2, Prot: types.ProtoTCP, Port: port})
	r.Set(&xdr.Mapping{Prog: types.ProgramPortmap, Vers: types.PortmapVersion2, Prot: types.ProtoUDP, Port: port})
}

// Clear removes all mappings from the registry.
// Used during shutdown cleanup.
func (r *Registry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.mappings = make(map[registryKey]*xdr.Mapping)
}

// Count returns the number of registered mappings.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return len(r.mappings)
}
