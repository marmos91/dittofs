package rpc

// RPC Program Numbers
//
// These identifiers specify which RPC program a message belongs to.
// Program numbers are assigned by IANA and identify different services
// that can be accessed via RPC. Each program can have multiple versions.
//
// Reference: RFC 1057 (RPC Protocol Specification Version 2)
const (
	// ProgramPortmap is the port mapper program number (RFC 1833).
	// The portmapper service allows clients to discover which port
	// a particular RPC program is listening on. It typically runs on
	// port 111 and maintains a mapping of program/version to port numbers.
	ProgramPortmap = 100000

	// ProgramNFS is the NFS version 3 program number (RFC 1813).
	// This identifies RPC messages as belonging to the Network File System
	// protocol. NFS v3 uses program number 100003 across all implementations.
	ProgramNFS = 100003

	// ProgramMount is the Mount protocol program number (RFC 1813 Appendix I).
	// The Mount protocol is a separate RPC program used by NFS clients to
	// obtain initial file handles and verify export permissions before
	// performing NFS operations. It handles the mount negotiation and
	// authentication that precedes NFS access.
	ProgramMount = 100005
)

// RPC Message Types
//
// These constants identify whether an RPC message is a call (request)
// from a client or a reply (response) from a server.
//
// Reference: RFC 5531 Section 9 (RPC Message Protocol)
const (
	// RPCCall indicates an RPC call message.
	// This is a request from the client to the server, containing:
	//   - Transaction ID (XID)
	//   - Program, version, and procedure numbers
	//   - Authentication credentials
	//   - Procedure-specific parameters
	RPCCall = 0

	// RPCReply indicates an RPC reply message.
	// This is a response from the server to the client, containing:
	//   - Transaction ID (XID) matching the call
	//   - Reply status (accepted or denied)
	//   - Authentication verifier
	//   - Procedure results (if accepted)
	RPCReply = 1
)

// RPC Reply States
//
// After receiving an RPC call, the server sends a reply that can be
// in one of two states: accepted or denied.
//
// Reference: RFC 5531 Section 9 (RPC Message Protocol)
const (
	// RPCMsgAccepted indicates the RPC call was accepted.
	// This means the server recognized the program and version,
	// and attempted to execute the requested procedure.
	// The reply will include an accept_stat that indicates
	// whether execution was successful.
	RPCMsgAccepted = 0

	// RPCMsgDenied indicates the RPC call was denied.
	// This means the server rejected the call, typically due to:
	//   - RPC version mismatch (not version 2)
	//   - Authentication failure
	// The reply will include a reject_stat with the reason.
	RPCMsgDenied = 1
)

// RPC Accept Status
//
// When an RPC call is accepted (RPCMsgAccepted), the accept_stat field
// indicates whether the procedure executed successfully or why it failed.
//
// Reference: RFC 5531 Section 9 (RPC Message Protocol)
const (
	// RPCSuccess indicates successful RPC execution.
	// The procedure completed without errors, and the reply
	// contains the procedure's return values.
	RPCSuccess = 0

	// RPCProgMismatch indicates program version mismatch.
	// The server supports the requested program number but not
	// the requested version. The reply includes the range of
	// supported versions (low and high).
	// Note: Despite the name, this is value 2 in the protocol.
	RPCProgMismatch = 2

	// RPCProcUnavail indicates the procedure is unavailable.
	// The server supports the program and version but doesn't
	// implement the requested procedure number. This typically
	// indicates an invalid procedure number or unimplemented feature.
	RPCProcUnavail = 3

	// RPCSystemErr indicates a system error on the server.
	// This is used when the server encounters an unexpected error
	// that prevents it from processing the request, such as:
	//   - Resource exhaustion
	//   - Rate limiting
	//   - Internal server errors
	RPCSystemErr = 5
)
