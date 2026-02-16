package identity

import (
	"context"
	"time"
)

// IdentityMapping represents an explicit mapping between an NFSv4 principal
// and a control plane username.
//
// Stored in the control plane database via the MappingStore interface.
type IdentityMapping struct {
	// Principal is the full NFSv4 principal string (e.g., "alice@EXAMPLE.COM").
	Principal string

	// Username is the control plane username this principal maps to.
	Username string

	// CreatedAt is the time this mapping was created.
	CreatedAt time.Time

	// UpdatedAt is the time this mapping was last updated.
	UpdatedAt time.Time
}

// MappingStore provides CRUD operations for identity mappings.
//
// Implementations are provided by the control plane (Plan 05).
// For testing, use a simple in-memory implementation.
type MappingStore interface {
	// GetMapping looks up a single mapping by principal.
	// Returns nil if the principal is not mapped.
	GetMapping(ctx context.Context, principal string) (*IdentityMapping, error)

	// ListMappings returns all configured mappings.
	ListMappings(ctx context.Context) ([]*IdentityMapping, error)

	// CreateMapping creates a new identity mapping.
	CreateMapping(ctx context.Context, mapping *IdentityMapping) error

	// DeleteMapping deletes a mapping by principal.
	DeleteMapping(ctx context.Context, principal string) error
}

// TableMapper resolves NFSv4 principals using an explicit mapping table
// backed by a MappingStore.
//
// When a principal is found in the mapping store, the mapped username is
// resolved via the userLookup callback to get the full identity (UID/GID/GIDs).
//
// TableMapper returns Found=false for principals not in the mapping table.
// This makes it suitable as the first mapper in a chain: explicit overrides
// are checked first, then convention-based mapping as fallback.
type TableMapper struct {
	store      MappingStore
	userLookup UserLookupFunc
}

// NewTableMapper creates a new table-based identity mapper.
//
// Parameters:
//   - store: The mapping store to query for explicit mappings
//   - userLookup: Callback to resolve a username to a full identity
func NewTableMapper(store MappingStore, userLookup UserLookupFunc) *TableMapper {
	return &TableMapper{
		store:      store,
		userLookup: userLookup,
	}
}

// Resolve maps an NFSv4 principal using the explicit mapping table.
//
// Resolution steps:
//  1. Look up principal in MappingStore
//  2. If not found, return Found=false
//  3. If found, call userLookup with the mapped username
//  4. Return the resolved identity
func (m *TableMapper) Resolve(ctx context.Context, principal string) (*ResolvedIdentity, error) {
	mapping, err := m.store.GetMapping(ctx, principal)
	if err != nil {
		return nil, err
	}

	if mapping == nil {
		return &ResolvedIdentity{Found: false}, nil
	}

	// Resolve the mapped username to a full identity
	resolved, err := m.userLookup(ctx, mapping.Username)
	if err != nil {
		return nil, err
	}

	if resolved == nil || !resolved.Found {
		return &ResolvedIdentity{Found: false}, nil
	}

	// Set the domain from the original principal
	_, domain := ParsePrincipal(principal)
	resolved.Domain = domain

	return resolved, nil
}
