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

// ProcedureName returns a human-readable name for a portmap procedure number.
func ProcedureName(proc uint32) string {
	switch proc {
	case ProcNull:
		return "NULL"
	case ProcSet:
		return "SET"
	case ProcUnset:
		return "UNSET"
	case ProcGetport:
		return "GETPORT"
	case ProcDump:
		return "DUMP"
	case ProcCallit:
		return "CALLIT"
	default:
		return "UNKNOWN"
	}
}
