package identity

import (
	"context"
	"errors"
)

// Common errors for AuthProvider operations.
var (
	ErrAuthenticationFailed = errors.New("authentication failed")
	ErrUnsupportedCredType  = errors.New("unsupported credential type")
)

// Credentials represents authentication credentials.
// Different implementations support different credential types.
type Credentials interface {
	// Type returns the credential type identifier.
	// Examples: "password", "ntlm", "kerberos", "certificate"
	Type() string
}

// PasswordCredentials represents username/password authentication.
type PasswordCredentials struct {
	Username string
	Password string
}

// Type returns "password".
func (c *PasswordCredentials) Type() string {
	return "password"
}

// UserIdentifier represents different ways to identify a user.
type UserIdentifier struct {
	Username string
	UID      *uint32
	SID      string
}

// AuthProvider defines the interface for authentication providers.
//
// Different providers can authenticate users against different backends:
//   - LocalAuthProvider: Uses ConfigUserStore for local users
//   - LDAPAuthProvider: Authenticates against LDAP/Active Directory (future)
//   - KerberosAuthProvider: Uses Kerberos tickets (future)
//
// Multiple providers can be chained to support multiple authentication methods.
type AuthProvider interface {
	// Name returns the provider identifier.
	// Examples: "local", "ldap", "kerberos"
	Name() string

	// Authenticate validates credentials and returns the authenticated user.
	// Returns ErrAuthenticationFailed if credentials are invalid.
	// Returns ErrUnsupportedCredType if the credential type is not supported.
	Authenticate(ctx context.Context, creds Credentials) (*User, error)

	// LookupUser finds a user by various identifiers.
	// This is used for mapping protocol-specific identities to DittoFS users.
	LookupUser(ctx context.Context, identifier UserIdentifier) (*User, error)

	// SupportsCredentialType returns true if the provider supports the given type.
	SupportsCredentialType(credType string) bool
}

// LocalAuthProvider implements AuthProvider using a UserStore.
//
// This is the default provider that authenticates users against
// the local user database (typically loaded from configuration).
type LocalAuthProvider struct {
	store UserStore
}

// NewLocalAuthProvider creates a new LocalAuthProvider with the given UserStore.
func NewLocalAuthProvider(store UserStore) *LocalAuthProvider {
	return &LocalAuthProvider{store: store}
}

// Name returns "local".
func (p *LocalAuthProvider) Name() string {
	return "local"
}

// Authenticate validates credentials against the local user store.
//
// Supported credential types:
//   - PasswordCredentials: Username/password authentication
func (p *LocalAuthProvider) Authenticate(ctx context.Context, creds Credentials) (*User, error) {
	switch c := creds.(type) {
	case *PasswordCredentials:
		return p.authenticatePassword(ctx, c)
	default:
		return nil, ErrUnsupportedCredType
	}
}

func (p *LocalAuthProvider) authenticatePassword(ctx context.Context, creds *PasswordCredentials) (*User, error) {
	user, err := p.store.ValidateCredentials(creds.Username, creds.Password)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) || errors.Is(err, ErrUserDisabled) {
			return nil, ErrAuthenticationFailed
		}
		return nil, err
	}
	return user, nil
}

// LookupUser finds a user by various identifiers.
// Note: UID and SID lookups are no longer supported by the UserStore interface.
// For protocol-specific identity resolution, use IdentityStore with ShareIdentityMapping.
func (p *LocalAuthProvider) LookupUser(ctx context.Context, identifier UserIdentifier) (*User, error) {
	// Try username first (the only lookup method supported by UserStore)
	if identifier.Username != "" {
		user, err := p.store.GetUser(identifier.Username)
		if err == nil {
			return user, nil
		}
	}

	// UID and SID lookups are not supported by the UserStore interface.
	// Protocol-specific identity (UID/GID/SID) is now per-share via ShareIdentityMapping.
	// Use IdentityStore.GetShareIdentityMapping() for UID/SID resolution.

	return nil, ErrUserNotFound
}

// SupportsCredentialType returns true for "password".
func (p *LocalAuthProvider) SupportsCredentialType(credType string) bool {
	return credType == "password"
}

// AuthProviderChain chains multiple auth providers together.
//
// When authenticating, providers are tried in order until one succeeds.
// This allows mixing local and external authentication.
type AuthProviderChain struct {
	providers []AuthProvider
}

// NewAuthProviderChain creates a new chain with the given providers.
func NewAuthProviderChain(providers ...AuthProvider) *AuthProviderChain {
	return &AuthProviderChain{providers: providers}
}

// Name returns "chain".
func (c *AuthProviderChain) Name() string {
	return "chain"
}

// Authenticate tries each provider in order until one succeeds.
func (c *AuthProviderChain) Authenticate(ctx context.Context, creds Credentials) (*User, error) {
	var lastErr error

	for _, provider := range c.providers {
		if !provider.SupportsCredentialType(creds.Type()) {
			continue
		}

		user, err := provider.Authenticate(ctx, creds)
		if err == nil {
			return user, nil
		}
		lastErr = err
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrUnsupportedCredType
}

// LookupUser tries each provider in order until one succeeds.
func (c *AuthProviderChain) LookupUser(ctx context.Context, identifier UserIdentifier) (*User, error) {
	for _, provider := range c.providers {
		user, err := provider.LookupUser(ctx, identifier)
		if err == nil {
			return user, nil
		}
	}
	return nil, ErrUserNotFound
}

// SupportsCredentialType returns true if any provider supports the type.
func (c *AuthProviderChain) SupportsCredentialType(credType string) bool {
	for _, provider := range c.providers {
		if provider.SupportsCredentialType(credType) {
			return true
		}
	}
	return false
}

// AddProvider adds a provider to the chain.
func (c *AuthProviderChain) AddProvider(provider AuthProvider) {
	c.providers = append(c.providers, provider)
}
