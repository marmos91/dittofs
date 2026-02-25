package identity

import (
	"context"
)

// StaticIdentity represents a Unix identity for a specific principal.
//
// This is a local type to avoid importing pkg/config (which would create
// circular dependencies). It mirrors config.StaticIdentity.
type StaticIdentity struct {
	// UID is the Unix user ID.
	UID uint32

	// GID is the Unix primary group ID.
	GID uint32

	// GIDs is a list of supplementary group IDs.
	GIDs []uint32
}

// StaticMapperConfig configures a StaticMapper.
type StaticMapperConfig struct {
	// StaticMap maps "principal@realm" strings to Unix identities.
	StaticMap map[string]StaticIdentity

	// DefaultUID is the UID for principals not found in StaticMap.
	// Typically 65534 (nobody).
	DefaultUID uint32

	// DefaultGID is the GID for principals not found in StaticMap.
	// Typically 65534 (nogroup).
	DefaultGID uint32
}

// StaticMapper implements IdentityMapper using a static configuration map.
//
// Principals are looked up in the configured static map using the full
// principal string as the key (e.g., "alice@EXAMPLE.COM"). If found, the
// configured UID/GID/GIDs are returned. Otherwise, the default UID/GID is used.
//
// Unlike ConventionMapper and TableMapper, StaticMapper always returns
// Found=true -- it falls back to defaults for unknown principals.
//
// This is suitable for small deployments with a known set of users.
// For larger deployments, use ConventionMapper or TableMapper.
type StaticMapper struct {
	staticMap  map[string]StaticIdentity
	defaultUID uint32
	defaultGID uint32
}

// NewStaticMapper creates a new static identity mapper from configuration.
func NewStaticMapper(cfg *StaticMapperConfig) *StaticMapper {
	staticMap := cfg.StaticMap
	if staticMap == nil {
		staticMap = make(map[string]StaticIdentity)
	}

	return &StaticMapper{
		staticMap:  staticMap,
		defaultUID: cfg.DefaultUID,
		defaultGID: cfg.DefaultGID,
	}
}

// Resolve maps a principal to a Unix identity using the static map.
//
// If the principal is found in the static map, returns the configured identity.
// If not found, returns an identity with the default UID/GID.
// StaticMapper always returns Found=true (falls back to defaults).
func (m *StaticMapper) Resolve(_ context.Context, principal string) (*ResolvedIdentity, error) {
	name, domain := ParsePrincipal(principal)

	if entry, ok := m.staticMap[principal]; ok {
		var gids []uint32
		if len(entry.GIDs) > 0 {
			gids = make([]uint32, len(entry.GIDs))
			copy(gids, entry.GIDs)
		}
		return &ResolvedIdentity{
			Username: name,
			UID:      entry.UID,
			GID:      entry.GID,
			GIDs:     gids,
			Domain:   domain,
			Found:    true,
		}, nil
	}

	// Default mapping for unknown principals
	return &ResolvedIdentity{
		Username: name,
		UID:      m.defaultUID,
		GID:      m.defaultGID,
		Domain:   domain,
		Found:    true,
	}, nil
}
