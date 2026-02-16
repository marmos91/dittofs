package kerberos

import (
	"fmt"

	"github.com/marmos91/dittofs/pkg/config"
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
// Principals are looked up in the configured static map using the key
// format "principal@realm". If a match is found, the configured UID/GID/GIDs
// are returned. Otherwise, the default UID/GID is used.
//
// This is suitable for small deployments with a known set of users.
// For larger deployments, consider LDAP or nsswitch-based mappers.
type StaticMapper struct {
	staticMap  map[string]config.StaticIdentity
	defaultUID uint32
	defaultGID uint32
}

// NewStaticMapper creates a new static identity mapper from configuration.
//
// Parameters:
//   - cfg: Identity mapping configuration containing the static map and defaults
//
// Returns:
//   - *StaticMapper: Initialized mapper
func NewStaticMapper(cfg *config.IdentityMappingConfig) *StaticMapper {
	staticMap := cfg.StaticMap
	if staticMap == nil {
		staticMap = make(map[string]config.StaticIdentity)
	}

	return &StaticMapper{
		staticMap:  staticMap,
		defaultUID: cfg.DefaultUID,
		defaultGID: cfg.DefaultGID,
	}
}

// MapPrincipal maps a Kerberos principal to a Unix identity.
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
	key := fmt.Sprintf("%s@%s", principal, realm)

	if entry, ok := m.staticMap[key]; ok {
		uid := entry.UID
		gid := entry.GID
		var gids []uint32
		if len(entry.GIDs) > 0 {
			gids = make([]uint32, len(entry.GIDs))
			copy(gids, entry.GIDs)
		}
		return &metadata.Identity{
			UID:      &uid,
			GID:      &gid,
			GIDs:     gids,
			Username: principal,
			Domain:   realm,
		}, nil
	}

	// Default mapping for unknown principals
	uid := m.defaultUID
	gid := m.defaultGID
	return &metadata.Identity{
		UID:      &uid,
		GID:      &gid,
		Username: principal,
		Domain:   realm,
	}, nil
}
