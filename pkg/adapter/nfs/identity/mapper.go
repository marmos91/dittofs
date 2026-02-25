// Package identity provides pluggable identity resolution for NFSv4 principals
// and Kerberos authentication.
//
// The IdentityMapper interface is the central abstraction: it takes an NFSv4-style
// principal string (e.g., "alice@EXAMPLE.COM") and resolves it to a ResolvedIdentity
// containing Unix UID/GID credentials.
//
// Implementations:
//   - ConventionMapper: Maps user@REALM to control plane user when domain matches
//   - TableMapper: Resolves explicit mappings from a MappingStore
//   - StaticMapper: Maps from a static config map (migrated from pkg/auth/kerberos)
//   - CachedMapper: TTL-based caching wrapper for any IdentityMapper
//
// GroupResolver is a separate interface for group membership queries, used during
// ACL evaluation of group@domain principals.
package identity

import (
	"context"
	"strings"
)

// IdentityMapper converts an NFSv4 principal string to a resolved identity.
//
// Implementations map principals (e.g., "alice@EXAMPLE.COM", "1000@localdomain",
// "OWNER@") to Unix-style identities for permission checks and ACL evaluation.
//
// The Resolve method returns Found=false when a principal cannot be resolved.
// This is not an error -- unknown principals are stored as-is in ACEs per the
// NFSv4 ACL model. An error return indicates an infrastructure failure (e.g.,
// database unavailable).
type IdentityMapper interface {
	// Resolve maps a principal string to a local identity.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - principal: Full principal string (e.g., "alice@EXAMPLE.COM", "OWNER@")
	//
	// Returns:
	//   - *ResolvedIdentity: The resolved identity (Found=false if not resolved)
	//   - error: Infrastructure error (not "user not found")
	Resolve(ctx context.Context, principal string) (*ResolvedIdentity, error)
}

// ResolvedIdentity contains the result of identity resolution.
//
// Found=false means the principal could not be resolved. This is distinct from
// an error -- it simply means the mapper does not know this principal. Unknown
// principals are preserved as-is in ACEs and skipped during evaluation.
type ResolvedIdentity struct {
	// Username is the resolved username in the control plane.
	Username string

	// UID is the Unix user ID.
	UID uint32

	// GID is the Unix primary group ID.
	GID uint32

	// GIDs contains supplementary group IDs.
	GIDs []uint32

	// Domain is the authentication domain (e.g., "EXAMPLE.COM").
	Domain string

	// Found indicates whether the principal was successfully resolved.
	// false means the mapper could not resolve this principal (not an error).
	Found bool
}

// GroupResolver provides group membership queries for ACL evaluation.
//
// When evaluating group@domain ACE principals, the ACL engine needs to check
// whether the requesting user is a member of the specified group.
type GroupResolver interface {
	// GetGroupMembers returns all usernames that are members of the given group.
	GetGroupMembers(ctx context.Context, groupName string) ([]string, error)

	// IsGroupMember checks whether a username is a member of the given group.
	IsGroupMember(ctx context.Context, username string, groupName string) (bool, error)
}

// ParsePrincipal splits an NFSv4 principal string into name and domain parts.
//
// Examples:
//   - "alice@EXAMPLE.COM" -> ("alice", "EXAMPLE.COM")
//   - "1000@localdomain" -> ("1000", "localdomain")
//   - "alice" -> ("alice", "")
//   - "OWNER@" -> ("OWNER@", "")
//   - "GROUP@" -> ("GROUP@", "")
//   - "EVERYONE@" -> ("EVERYONE@", "")
//
// Special identifiers (OWNER@, GROUP@, EVERYONE@) are returned as-is with
// empty domain because they are resolved dynamically at evaluation time.
func ParsePrincipal(principal string) (name, domain string) {
	// Special identifiers are returned as-is
	switch principal {
	case "OWNER@", "GROUP@", "EVERYONE@":
		return principal, ""
	}

	// Split on the last "@" to handle edge cases like "user@host@REALM"
	idx := strings.LastIndex(principal, "@")
	if idx < 0 {
		return principal, ""
	}

	// If "@" is the last character (e.g., "OWNER@" not caught above), treat as special
	if idx == len(principal)-1 {
		return principal, ""
	}

	return principal[:idx], principal[idx+1:]
}

// NobodyIdentity returns a ResolvedIdentity for the "nobody" user.
//
// This is commonly used as a fallback identity for anonymous or unmapped
// principals. Uses the standard UID/GID 65534.
func NobodyIdentity() *ResolvedIdentity {
	return &ResolvedIdentity{
		Username: "nobody",
		UID:      65534,
		GID:      65534,
		Found:    true,
	}
}
