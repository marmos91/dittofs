package identity

import (
	"context"
	"strconv"
	"strings"
)

// UserLookupFunc is a callback that queries the control plane for a user by username.
//
// This indirection avoids importing the controlplane package from pkg/identity,
// preventing circular dependencies. The callback is provided by the runtime
// during initialization.
type UserLookupFunc func(ctx context.Context, username string) (*ResolvedIdentity, error)

// ConventionMapper resolves NFSv4 principals using convention-based mapping.
//
// The convention is: if the principal's domain matches the configured Kerberos
// realm (case-insensitive), strip the domain and look up the username in the
// control plane.
//
// Examples (with realm "EXAMPLE.COM"):
//   - "alice@EXAMPLE.COM" -> looks up "alice" in control plane
//   - "alice@example.com" -> looks up "alice" (case-insensitive match)
//   - "1000@EXAMPLE.COM"  -> returns UID=1000 directly (numeric interop)
//   - "alice@OTHER.COM"   -> Found=false (domain mismatch)
//   - "alice"             -> Found=false (no domain)
type ConventionMapper struct {
	configuredRealm string
	userLookup      UserLookupFunc
}

// NewConventionMapper creates a new convention-based identity mapper.
//
// Parameters:
//   - realm: The Kerberos realm to match against (e.g., "EXAMPLE.COM")
//   - userLookup: Callback to query the control plane for a user by username
func NewConventionMapper(realm string, userLookup UserLookupFunc) *ConventionMapper {
	return &ConventionMapper{
		configuredRealm: realm,
		userLookup:      userLookup,
	}
}

// Resolve maps an NFSv4 principal to a control plane identity using convention.
//
// Resolution steps:
//  1. Parse principal into name@domain
//  2. If domain is empty or doesn't match configuredRealm (case-insensitive), return Found=false
//  3. If name is numeric, return ResolvedIdentity with UID=parsed int (AUTH_SYS interop)
//  4. Call userLookup to resolve from control plane
//  5. If not found, return Found=false
func (m *ConventionMapper) Resolve(ctx context.Context, principal string) (*ResolvedIdentity, error) {
	name, domain := ParsePrincipal(principal)

	// Domain must be present and match the configured realm
	if domain == "" || !strings.EqualFold(domain, m.configuredRealm) {
		return &ResolvedIdentity{Found: false}, nil
	}

	// Numeric UID support for AUTH_SYS interop
	if uid, err := strconv.ParseUint(name, 10, 32); err == nil {
		return &ResolvedIdentity{
			Username: name,
			UID:      uint32(uid),
			GID:      uint32(uid), // Use same value as GID default
			Domain:   domain,
			Found:    true,
		}, nil
	}

	// Look up username in control plane
	resolved, err := m.userLookup(ctx, name)
	if err != nil {
		return nil, err
	}

	if resolved == nil || !resolved.Found {
		return &ResolvedIdentity{Found: false}, nil
	}

	// Ensure domain is set from the principal
	resolved.Domain = domain
	return resolved, nil
}
