// Package kerberos provides shared Kerberos authentication for DittoFS.
//
// This file implements principal-to-username resolution for mapping Kerberos
// principals (e.g., "alice@EXAMPLE.COM") to DittoFS usernames (e.g., "alice").
package kerberos

import (
	"strings"
)

// IdentityConfig configures how Kerberos principals are mapped to DittoFS usernames.
type IdentityConfig struct {
	// StripRealm controls whether the realm is stripped from principal names.
	// Default: true ("alice@REALM" -> "alice")
	StripRealm bool

	// ExplicitMappings maps full Kerberos principals to DittoFS usernames.
	// These take precedence over the strip-realm default.
	// Example: {"admin@CORP.COM": "superadmin", "svc/host@CORP.COM": "service-account"}
	ExplicitMappings map[string]string
}

// DefaultIdentityConfig returns the default identity configuration.
// By default, the realm is stripped from principal names ("alice@REALM" -> "alice").
func DefaultIdentityConfig() *IdentityConfig {
	return &IdentityConfig{
		StripRealm: true,
	}
}

// ResolvePrincipal resolves a Kerberos principal to a DittoFS username.
//
// Lookup order:
//  1. Explicit mapping for "principal@realm" (if configured)
//  2. Strip realm: "alice@REALM" -> "alice" (if StripRealm is true, which is default)
//  3. Strip service prefix: "svc/host" -> "svc" (for service principals)
//  4. If StripRealm is false and no explicit mapping: return "principal@realm"
//
// The principalName parameter is the raw principal from the Kerberos ticket,
// which may contain a "/" for service principals (e.g., "svc/host.example.com").
func ResolvePrincipal(principalName, realm string, config *IdentityConfig) string {
	if config == nil {
		config = DefaultIdentityConfig()
	}

	if principalName == "" {
		return ""
	}

	// Step 1: Check explicit mappings for "principal@realm"
	if len(config.ExplicitMappings) > 0 && realm != "" {
		fullPrincipal := principalName + "@" + realm
		if mapped, ok := config.ExplicitMappings[fullPrincipal]; ok {
			return mapped
		}
	}

	// Step 2: Strip realm if enabled
	if config.StripRealm || realm == "" {
		username := principalName
		// Step 3: Strip service prefix for service principals ("svc/host" -> "svc")
		if idx := strings.Index(username, "/"); idx >= 0 {
			username = username[:idx]
		}
		return username
	}

	// Step 4: Return full principal@realm when StripRealm is disabled
	return principalName + "@" + realm
}
