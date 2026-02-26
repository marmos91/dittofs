// Package auth provides centralized authentication abstractions for DittoFS.
//
// This package defines the core authentication interfaces and types used across
// all protocol adapters (NFS, SMB). It provides:
//
//   - AuthProvider interface for pluggable authentication mechanisms
//   - Authenticator that chains multiple AuthProviders
//   - AuthResult representing authentication outcomes
//   - Standard error types for authentication failures
//
// Protocol-specific authentication details (NTLM challenge-response, AUTH_UNIX
// credential parsing, RPCSEC_GSS context negotiation) remain in their respective
// adapter packages. This package provides the shared abstractions that allow
// the runtime to manage authentication uniformly across protocols.
//
// Sub-packages:
//   - kerberos/: Kerberos AuthProvider implementation with keytab management
package auth

import (
	"context"
	"errors"
)

// AuthProvider defines a pluggable authentication mechanism.
//
// Implementations handle specific authentication protocols (Kerberos, NTLM, etc.)
// and are chained together by the Authenticator. When a request arrives, the
// Authenticator iterates through providers in order, calling CanHandle to find
// the appropriate provider, then Authenticate to process the token.
//
// Thread safety: implementations must be safe for concurrent use.
type AuthProvider interface {
	// CanHandle returns true if this provider can process the given authentication token.
	//
	// The token format is protocol-specific:
	//   - Kerberos/SPNEGO: ASN.1 encoded SPNEGO token (OID 1.3.6.1.5.5.2)
	//   - NTLM: NTLMSSP message bytes
	//   - AUTH_UNIX: Encoded Unix credentials
	//
	// Implementations should perform a fast check (e.g., magic bytes or OID prefix)
	// without full token parsing.
	CanHandle(token []byte) bool

	// Authenticate processes an authentication token and returns the result.
	//
	// Returns:
	//   - (*AuthResult, nil) on successful authentication
	//   - (nil, error) on failure
	Authenticate(ctx context.Context, token []byte) (*AuthResult, error)

	// Name returns the provider name for logging and diagnostics.
	// Examples: "kerberos", "ntlm", "auth_unix"
	Name() string
}

// AuthResult contains the outcome of a successful authentication.
//
// The Identity field holds the authenticated identity in a protocol-neutral form.
// The Provider field indicates which AuthProvider handled the authentication,
// useful for audit logging and debugging.
type AuthResult struct {
	// Identity is the authenticated identity.
	Identity Identity

	// Authenticated indicates whether authentication succeeded.
	Authenticated bool

	// Provider is the name of the AuthProvider that handled this authentication.
	Provider string
}

// Authenticator chains multiple AuthProvider implementations and tries each in order.
//
// When Authenticate is called, the Authenticator iterates through providers:
//  1. Calls CanHandle(token) on each provider
//  2. Calls Authenticate(ctx, token) on the first provider that can handle the token
//  3. Returns the result from that provider
//
// If no provider can handle the token, ErrUnsupportedMechanism is returned.
//
// Thread safety: safe for concurrent use (providers are read-only after construction).
type Authenticator struct {
	providers []AuthProvider
}

// NewAuthenticator creates a new Authenticator with the given providers.
// Providers are tried in order; the first one whose CanHandle returns true
// processes the token.
func NewAuthenticator(providers ...AuthProvider) *Authenticator {
	return &Authenticator{providers: providers}
}

// Authenticate processes an authentication token by delegating to the first
// matching provider.
//
// Returns ErrUnsupportedMechanism if no provider can handle the token.
func (a *Authenticator) Authenticate(ctx context.Context, token []byte) (*AuthResult, error) {
	for _, p := range a.providers {
		if p.CanHandle(token) {
			return p.Authenticate(ctx, token)
		}
	}
	return nil, ErrUnsupportedMechanism
}

// Providers returns the list of registered auth providers.
// Useful for diagnostics and logging.
func (a *Authenticator) Providers() []AuthProvider {
	return a.providers
}

// Standard authentication errors.
var (
	// ErrAuthFailed indicates that authentication was attempted but failed
	// (e.g., bad password, expired ticket, invalid signature).
	ErrAuthFailed = errors.New("auth: authentication failed")

	// ErrUnsupportedMechanism indicates that no registered AuthProvider can
	// handle the presented authentication token.
	ErrUnsupportedMechanism = errors.New("auth: unsupported authentication mechanism")

	// ErrInvalidCredentials indicates that the credentials are malformed or
	// cannot be parsed (distinct from wrong credentials).
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
)
