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
	Authenticate(ctx context.Context, token []byte) (*AuthResult, error)

	// Name returns the provider name for logging and diagnostics.
	// Examples: "kerberos", "ntlm", "auth_unix"
	Name() string
}

// AuthResult contains the outcome of an authentication attempt.
type AuthResult struct {
	// Identity is the authenticated identity in protocol-neutral form.
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
//  2. Calls Authenticate(ctx, token) on each provider that can handle the token
//  3. If a provider returns ErrUnsupportedMechanism, the next provider is tried
//  4. Otherwise, returns the result from that provider
//
// If no provider can handle the token (or all return ErrUnsupportedMechanism),
// ErrUnsupportedMechanism is returned.
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
		if !p.CanHandle(token) {
			continue
		}

		res, err := p.Authenticate(ctx, token)
		// If the provider indicates that, upon deeper inspection, it does not
		// support the specific mechanism (e.g., inner mechanism of SPNEGO),
		// try the next provider in the chain.
		if errors.Is(err, ErrUnsupportedMechanism) {
			continue
		}

		return res, err
	}
	return nil, ErrUnsupportedMechanism
}

// Providers returns the list of registered auth providers.
// Useful for diagnostics and logging.
func (a *Authenticator) Providers() []AuthProvider {
	if a == nil || len(a.providers) == 0 {
		return nil
	}
	copied := make([]AuthProvider, len(a.providers))
	copy(copied, a.providers)
	return copied
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
