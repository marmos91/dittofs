// Package types provides portmapper protocol types and constants.
//
// The portmapper (portmap v2) is a service that maps RPC program/version/protocol
// tuples to port numbers. NFS clients use it to discover which port the NFS server
// and related services (MOUNT, NLM, NSM) listen on.
//
// This implementation follows RFC 1057 Section A (Portmapper Protocol).
//
// References:
//   - RFC 1057 Section A (Port Mapper Program Protocol)
//   - RFC 1833 (Binding Protocols for ONC RPC Version 2)
package types

// ============================================================================
// Portmap RPC Program and Version
// ============================================================================

const (
	// ProgramPortmap is the portmapper RPC program number.
	// Per RFC 1057, the portmapper uses program number 100000.
	ProgramPortmap uint32 = 100000

	// PortmapVersion2 is the portmap protocol version 2.
	// This is the version defined in RFC 1057 and used by rpcinfo/showmount.
	PortmapVersion2 uint32 = 2

	// PortmapVersion3 and PortmapVersion4 are the RPCBIND protocol versions
	// (RFC 1833). They use string-based universal addresses instead of the v2
	// (prog, vers, prot) -> port mapping. macOS/BSD NFS lock clients query NLM
	// via RPCBIND v3/v4 (over the mount's address family) and do not fall back
	// to v2, so the embedded portmapper must answer them to enable NFSv3 locking.
	PortmapVersion3 uint32 = 3
	PortmapVersion4 uint32 = 4
)

// ============================================================================
// RPCBIND v3/v4 Procedure Numbers (RFC 1833)
// ============================================================================

const (
	// RpcbProcGetaddr looks up the universal address for a (prog, vers, netid)
	// tuple. Returns the uaddr string, or "" if not registered. (v3 and v4.)
	RpcbProcGetaddr uint32 = 3

	// RpcbProcDump returns all registrations as a list of rpcb entries. (v3/v4.)
	RpcbProcDump uint32 = 4

	// RpcbProcGetversaddr (v4) is like GETADDR but matches the exact version.
	// Our lookup is already version-exact, so it shares the GETADDR handler.
	RpcbProcGetversaddr uint32 = 9
)

// ============================================================================
// Portmap v2 Procedure Numbers (RFC 1057 Section A)
// ============================================================================

const (
	// ProcNull is the NULL procedure for connection testing (ping).
	// No authentication required, always succeeds.
	ProcNull uint32 = 0

	// ProcSet registers a mapping of (prog, vers, prot) -> port.
	// Returns true on success, false on failure.
	ProcSet uint32 = 1

	// ProcUnset removes a mapping for (prog, vers, prot).
	// Returns true if the mapping existed and was removed.
	ProcUnset uint32 = 2

	// ProcGetport looks up the port for a given (prog, vers, prot) tuple.
	// Returns the port number, or 0 if not registered.
	ProcGetport uint32 = 3

	// ProcDump returns a list of all registered mappings.
	// Uses XDR optional-data linked list encoding.
	ProcDump uint32 = 4

	// ProcCallit is the indirect call procedure.
	// It forwards an RPC call to another program and returns the result.
	// DittoFS does NOT implement this procedure (security risk per modern best practices).
	ProcCallit uint32 = 5
)

// ============================================================================
// Protocol Constants (IPPROTO values per RFC 1057)
// ============================================================================

const (
	// ProtoTCP is the TCP protocol identifier (IPPROTO_TCP = 6).
	ProtoTCP uint32 = 6

	// ProtoUDP is the UDP protocol identifier (IPPROTO_UDP = 17).
	ProtoUDP uint32 = 17
)
