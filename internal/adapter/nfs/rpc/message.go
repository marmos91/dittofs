package rpc

// RPCCallMessage represents an RPC call (request) message.
//
// This structure defines the header of every RPC request sent from client
// to server. It contains routing information (which program/procedure to call),
// transaction tracking (XID), and authentication credentials.
//
// Wire Format (XDR encoding):
//   - XID:        4 bytes (transaction identifier)
//   - MsgType:    4 bytes (must be 0 for CALL)
//   - RPCVersion: 4 bytes (must be 2 for RPC version 2)
//   - Program:    4 bytes (program number)
//   - Version:    4 bytes (program version)
//   - Procedure:  4 bytes (procedure number within program)
//   - Cred:       variable (authentication credentials)
//   - Verf:       variable (authentication verifier)
//   - [procedure-specific parameters follow]
//
// Reference: RFC 5531 Section 9 (RPC Protocol Specification)
type RPCCallMessage struct {
	// XID (Transaction ID) uniquely identifies this RPC call.
	// The client generates this value and the server must echo it back
	// in the reply. This allows the client to match replies to requests,
	// which is essential for handling concurrent RPC calls and retries.
	XID uint32

	// MsgType indicates whether this is a call or reply.
	// For RPCCallMessage, this must always be 0 (RPCCall).
	MsgType uint32

	// RPCVersion is the RPC protocol version number.
	// This implementation supports version 2 (RFC 5531).
	// Must be 2 for all calls.
	RPCVersion uint32

	// Program identifies which RPC service this call is for.
	// Common values:
	//   - 100003: NFS
	//   - 100005: Mount protocol
	//   - 100000: Portmapper
	Program uint32

	// Version is the version number of the requested program.
	// Different versions of the same program may have different
	// procedures or parameters. For NFS, this is typically 3.
	Version uint32

	// Procedure identifies which operation to perform within the program.
	// Each program defines its own set of procedure numbers.
	// For example, in NFS v3:
	//   - 0:  NULL (ping)
	//   - 1:  GETATTR (get file attributes)
	//   - 6:  READ (read from file)
	//   - 8:  WRITE (write to file)
	Procedure uint32

	// Cred contains authentication credentials from the client.
	// This proves the client's identity to the server. Common types:
	//   - AUTH_NULL: No authentication
	//   - AUTH_UNIX: Unix UID/GID credentials
	// The flavor field indicates the credential type, and the body
	// contains the type-specific credential data.
	Cred OpaqueAuth

	// Verf contains authentication verifier from the client.
	// This provides additional authentication information, often used
	// for challenge-response or encryption-based authentication schemes.
	// For AUTH_UNIX, this is typically AUTH_NULL (empty).
	Verf OpaqueAuth
}

// RPCReplyMessage represents an RPC reply (response) message.
//
// This structure defines the header of every RPC response sent from server
// to client. It contains the transaction ID (matching the call), the reply
// status, and authentication information.
//
// Wire Format (XDR encoding):
//   - XID:        4 bytes (transaction identifier, echoed from call)
//   - MsgType:    4 bytes (must be 1 for REPLY)
//   - ReplyState: 4 bytes (0=MSG_ACCEPTED, 1=MSG_DENIED)
//   - [if MSG_ACCEPTED:]
//   - Verf:       variable (authentication verifier)
//   - AcceptStat: 4 bytes (0=SUCCESS, 1=PROG_UNAVAIL, etc.)
//   - [if SUCCESS: procedure results follow]
//   - [if PROG_MISMATCH: version range follows]
//   - [if MSG_DENIED: reject information follows]
//
// Reference: RFC 5531 Section 9 (RPC Protocol Specification)
type RPCReplyMessage struct {
	// XID (Transaction ID) matches the XID from the call.
	// This allows the client to correlate this reply with the
	// corresponding request, enabling concurrent operations.
	XID uint32

	// MsgType indicates this is a reply message.
	// Must be 1 (RPCReply) for all replies.
	MsgType uint32

	// ReplyState indicates whether the call was accepted or denied.
	// Values:
	//   - 0 (MSG_ACCEPTED): Server accepted and processed the call
	//   - 1 (MSG_DENIED): Server rejected the call (auth failure or RPC version)
	ReplyState uint32

	// Verf contains authentication verifier from the server.
	// Used by some authentication schemes to prove the server's identity
	// or provide session information. For AUTH_UNIX, typically AUTH_NULL.
	Verf OpaqueAuth

	// AcceptStat indicates the acceptance status (only if ReplyState is MSG_ACCEPTED).
	// Values:
	//   - 0 (SUCCESS): Procedure executed successfully
	//   - 1 (PROG_UNAVAIL): Program not available
	//   - 2 (PROG_MISMATCH): Program version not supported
	//   - 3 (PROC_UNAVAIL): Procedure not available
	//   - 4 (GARBAGE_ARGS): Invalid procedure arguments
	//   - 5 (SYSTEM_ERR): System error on server
	AcceptStat uint32

	// Reply data follows this header (not included in struct)
	// The content depends on the AcceptStat:
	//   - SUCCESS: procedure-specific results
	//   - PROG_MISMATCH: low/high version numbers
	//   - Others: no additional data
}

// OpaqueAuth represents authentication credentials or verifiers.
//
// This structure is used for both credentials (proving client identity)
// and verifiers (proving message authenticity). The "opaque" nature means
// the RPC layer doesn't interpret the body - it just passes it through.
// The interpretation depends on the flavor (authentication type).
//
// Reference: RFC 5531 Section 8 (Authentication)
type OpaqueAuth struct {
	// Flavor identifies the authentication scheme.
	// Common values:
	//   - 0 (AUTH_NULL): No authentication
	//   - 1 (AUTH_UNIX): Unix-style UID/GID authentication
	//   - 2 (AUTH_SHORT): Short-hand Unix credential
	//   - 3 (AUTH_DES): DES encryption-based authentication
	Flavor uint32

	// Body contains the authentication data.
	// The format depends on the Flavor:
	//   - AUTH_NULL: empty (no data)
	//   - AUTH_UNIX: Unix credentials (stamp, machine, UID, GID, GIDs)
	//   - AUTH_SHORT: short-hand credential
	//   - AUTH_DES: DES-encrypted authentication data
	//
	// The xdr:"opaque" tag indicates this is a variable-length opaque byte array
	// in XDR encoding (length prefix followed by data and padding).
	Body []byte `xdr:"opaque"`
}

// GetAuthFlavor returns the authentication flavor from the RPC call credentials.
//
// This method provides convenient access to the authentication type being used
// by the client. The server should check this to determine how to interpret
// the authentication body and what level of trust to place in the client.
//
// Common authentication flavors:
//   - 0: AUTH_NULL (no authentication) - Use for testing or when security
//     is provided by other means (e.g., trusted network, VPN)
//   - 1: AUTH_UNIX (Unix-style authentication) - Standard for NFS, provides
//     UID/GID information for access control
//   - 2: AUTH_SHORT (short hand Unix credential) - Optimization to reduce
//     overhead after initial AUTH_UNIX authentication
//   - 3: AUTH_DES (DES encryption-based) - Stronger authentication using
//     DES encryption, also known as AUTH_DH (Diffie-Hellman)
//
// Returns:
//   - uint32: The authentication flavor constant (AuthNull, AuthUnix, etc.)
//
// Example usage:
//
//	switch call.GetAuthFlavor() {
//	case rpc.AuthNull:
//	    // No authentication - decide whether to accept
//	case rpc.AuthUnix:
//	    // Parse Unix credentials for UID/GID
//	    auth, err := rpc.ParseUnixAuth(call.GetAuthBody())
//	default:
//	    // Unsupported authentication type
//	}
func (c *RPCCallMessage) GetAuthFlavor() uint32 {
	return c.Cred.Flavor
}

// GetAuthBody returns the authentication body data.
//
// This method provides access to the raw authentication credentials sent
// by the client. The format of this data depends on the authentication
// flavor (see GetAuthFlavor).
//
// Body contents by flavor:
//   - AUTH_NULL: Empty byte slice (no authentication data)
//   - AUTH_UNIX: XDR-encoded Unix credentials containing:
//   - Stamp (timestamp/ID)
//   - Machine name (client hostname)
//   - UID (user ID)
//   - GID (primary group ID)
//   - GIDs (supplementary group IDs)
//     Use ParseUnixAuth() to decode this data.
//   - AUTH_SHORT: Server-issued short credential from previous auth
//   - AUTH_DES: DES-encrypted authentication data
//
// Returns:
//   - []byte: Raw authentication body (may be empty for AUTH_NULL)
//
// Example usage:
//
//	if call.GetAuthFlavor() == rpc.AuthUnix {
//	    unixAuth, err := rpc.ParseUnixAuth(call.GetAuthBody())
//	    if err != nil {
//	        return fmt.Errorf("parse auth: %w", err)
//	    }
//	    // Use unixAuth.UID and unixAuth.GID for access control
//	}
func (c *RPCCallMessage) GetAuthBody() []byte {
	return c.Cred.Body
}

// GetVerifierBody returns the verifier body data from the RPC call.
//
// For RPCSEC_GSS DATA requests, this contains the MIC of the RPC header
// computed by the client. For AUTH_UNIX/AUTH_NULL, this is typically empty.
//
// Returns:
//   - []byte: Raw verifier body (may be empty)
func (c *RPCCallMessage) GetVerifierBody() []byte {
	return c.Verf.Body
}
