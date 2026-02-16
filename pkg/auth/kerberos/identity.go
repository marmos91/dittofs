package kerberos

import (
	"github.com/marmos91/dittofs/pkg/config"
	"github.com/marmos91/dittofs/pkg/identity"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// IdentityMapper converts a Kerberos principal to a local identity.
//
// Implementations map authenticated Kerberos principals (e.g., "alice@EXAMPLE.COM")
// to Unix-style identities (UID/GID) for NFS permission checks.
//
// The mapping result is a *metadata.Identity which integrates directly
// with the existing authentication and permission checking infrastructure.
type IdentityMapper interface {
	// MapPrincipal maps a Kerberos principal and realm to a local identity.
	//
	// Parameters:
	//   - principal: The Kerberos principal name (e.g., "alice")
	//   - realm: The Kerberos realm (e.g., "EXAMPLE.COM")
	//
	// Returns:
	//   - *metadata.Identity: The mapped local identity with UID/GID
	//   - error: If mapping fails (should be rare for static mapper)
	MapPrincipal(principal string, realm string) (*metadata.Identity, error)
}

// StaticMapper implements IdentityMapper using a static configuration map.
//
// This is a backward-compatible wrapper that delegates to identity.StaticMapper
// from the new pkg/identity package. All new code should use
// identity.StaticMapper directly.
type StaticMapper struct {
	inner *identity.StaticMapper
}

// NewStaticMapper creates a new static identity mapper from configuration.
//
// Converts the config.IdentityMappingConfig to an identity.StaticMapperConfig
// and delegates to identity.NewStaticMapper.
//
// Parameters:
//   - cfg: Identity mapping configuration containing the static map and defaults
//
// Returns:
//   - *StaticMapper: Initialized mapper wrapping identity.StaticMapper
func NewStaticMapper(cfg *config.IdentityMappingConfig) *StaticMapper {
	// Convert config.StaticIdentity map to identity.StaticIdentity map
	staticMap := make(map[string]identity.StaticIdentity, len(cfg.StaticMap))
	for k, v := range cfg.StaticMap {
		staticMap[k] = identity.StaticIdentity{
			UID:  v.UID,
			GID:  v.GID,
			GIDs: v.GIDs,
		}
	}

	innerCfg := &identity.StaticMapperConfig{
		StaticMap:  staticMap,
		DefaultUID: cfg.DefaultUID,
		DefaultGID: cfg.DefaultGID,
	}

	return &StaticMapper{
		inner: identity.NewStaticMapper(innerCfg),
	}
}

// MapPrincipal maps a Kerberos principal to a Unix identity.
//
// Delegates to the embedded identity.StaticMapper and converts the result
// to a *metadata.Identity for backward compatibility.
//
// Lookup key format: "principal@realm" (e.g., "alice@EXAMPLE.COM").
//
// If found in the static map:
//   - Returns Identity with the configured UID, GID, and supplementary GIDs
//   - Username is set to the principal name
//   - Domain is set to the realm
//
// If not found:
//   - Returns Identity with DefaultUID/DefaultGID (typically 65534/nobody)
//   - Username is still set to the principal name
//   - Domain is still set to the realm
func (m *StaticMapper) MapPrincipal(principal string, realm string) (*metadata.Identity, error) {
	uid, gid, gids, err := m.inner.MapPrincipal(principal, realm)
	if err != nil {
		return nil, err
	}

	return &metadata.Identity{
		UID:      &uid,
		GID:      &gid,
		GIDs:     gids,
		Username: principal,
		Domain:   realm,
	}, nil
}
