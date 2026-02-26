package auth

import "context"

// Identity represents an authenticated identity in a protocol-neutral form.
//
// Identity is designed to be generic enough to represent any authentication
// outcome: Unix credentials (NFS AUTH_UNIX), Kerberos principals (RPCSEC_GSS),
// NTLM sessions (SMB), or anonymous access. Protocol adapters convert their
// protocol-specific auth results into Identity instances for uniform handling.
//
// Fields:
//   - Username/UID/GID/Groups: Unix-style identity (NFS AUTH_UNIX, mapped Kerberos)
//   - Principal: Kerberos principal name (e.g., "user@REALM")
//   - SessionID: Protocol session identifier (e.g., SMB session ID)
//   - Anonymous: True for unauthenticated/guest access
//   - Attributes: Extensible key-value pairs for protocol-specific metadata
type Identity struct {
	// Username is the DittoFS username, if resolved.
	// May be empty for unmapped Unix UIDs or anonymous access.
	Username string

	// UID is the numeric Unix user ID.
	// Set from AUTH_UNIX credentials or Kerberos-to-UID mapping.
	UID uint32

	// GID is the primary Unix group ID.
	GID uint32

	// Groups contains supplementary Unix group IDs.
	Groups []uint32

	// Principal is the Kerberos principal name (e.g., "alice@EXAMPLE.COM").
	// Empty for non-Kerberos authentication.
	Principal string

	// SessionID is a protocol-specific session identifier.
	// Used by SMB for NTLM session tracking. Empty for NFS.
	SessionID string

	// Anonymous indicates this is an unauthenticated or guest identity.
	// When true, Username may be empty and UID/GID may be default values.
	Anonymous bool

	// Attributes holds extensible protocol-specific metadata.
	// Examples: "auth_flavor" -> "AUTH_SYS", "krb5_level" -> "krb5i"
	Attributes map[string]string
}

// IdentityMapper converts authentication results into protocol-specific
// identity contexts.
//
// Each protocol adapter implements IdentityMapper to handle its unique
// identity mapping requirements:
//
//   - NFS: Maps AUTH_UNIX UIDs, AUTH_NULL, RPCSEC_GSS Kerberos principals
//     across v3, v4.0, and v4.1 auth mechanisms
//   - SMB: Maps NTLM sessions and SPNEGO/Kerberos negotiations
//
// The mapper bridges the gap between DittoFS's central auth result and
// the protocol's native identity representation.
//
// Thread safety: implementations must be safe for concurrent use.
type IdentityMapper interface {
	// MapIdentity converts an AuthResult into a protocol-specific Identity.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeout
	//   - authResult: The authentication outcome from an AuthProvider
	//
	// Returns:
	//   - (*Identity, nil) on successful mapping
	//   - (nil, error) if the identity cannot be mapped (e.g., unknown principal)
	MapIdentity(ctx context.Context, authResult *AuthResult) (*Identity, error)
}
