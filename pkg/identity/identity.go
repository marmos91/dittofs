// Package identity provides pluggable identity resolution for DittoFS.
//
// This package defines the core abstractions for mapping external identity
// claims (Kerberos principals, OIDC tokens, AD SIDs, etc.) to DittoFS users.
// Each external identity system is implemented as a Provider in its own
// sub-package (e.g., identity/kerberos, identity/oidc).
//
// Protocol adapters (NFS, SMB) extract credentials from protocol-specific
// auth data and pass them to the Resolver, which chains Providers to find
// a matching DittoFS user. The control plane is the single source of truth
// for user identity (username, UID, GID).
//
// This package is intentionally decoupled from both protocol adapters and
// the control plane. Dependencies are injected via interfaces (LinkStore)
// and callbacks (UserLookup) to prevent circular imports.
package identity

import "context"

// Credential represents an identity claim from an external system.
// Protocol adapters construct this from protocol-specific auth data.
type Credential struct {
	// Provider identifies which identity provider should handle this credential
	// (e.g., "kerberos", "oidc", "ad"). When empty, the Resolver tries all providers.
	Provider string

	// ExternalID is the canonical identifier in the external system.
	// Format is provider-specific:
	//   - Kerberos: "alice@EXAMPLE.COM" (principal@realm)
	//   - OIDC:     "https://accounts.google.com|sub123" (issuer|subject)
	//   - AD:       "S-1-5-21-...-1001" (SID)
	ExternalID string

	// Attributes holds provider-specific metadata needed for resolution.
	Attributes map[string]string
}

// ResolvedIdentity is the result of resolving an external credential
// to a DittoFS user.
type ResolvedIdentity struct {
	Username string
	UID      uint32
	GID      uint32
	GIDs     []uint32
	Domain   string
	Found    bool
}

// IdentityProvider resolves external credentials to DittoFS users.
// Each identity system (Kerberos, OIDC, AD, LDAP) implements this interface.
//
// Providers are stateless resolvers — they do not manage authentication.
// Protocol adapters handle authentication; providers handle identity mapping.
//
// Thread safety: implementations must be safe for concurrent use.
type IdentityProvider interface {
	// Name returns the provider identifier (e.g., "kerberos", "oidc", "ad").
	Name() string

	// CanResolve returns true if this provider can handle the given credential.
	CanResolve(cred *Credential) bool

	// Resolve maps an external credential to a DittoFS user.
	// Returns Found=false if the credential is valid but no mapping exists.
	// Returns error only for infrastructure failures.
	Resolve(ctx context.Context, cred *Credential) (*ResolvedIdentity, error)
}

// UserLookup resolves a DittoFS username to UID/GID/groups.
// Injected by the control plane at wiring time to avoid circular imports.
type UserLookup func(ctx context.Context, username string) (*ResolvedIdentity, error)

// NobodyIdentity returns a ResolvedIdentity for the "nobody" user (UID/GID 65534).
func NobodyIdentity() *ResolvedIdentity {
	return &ResolvedIdentity{
		Username: "nobody",
		UID:      65534,
		GID:      65534,
		Found:    true,
	}
}
