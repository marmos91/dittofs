package registry

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// ApplyIdentityMapping applies share-level identity mapping rules to create effective credentials.
//
// This implements:
//   - all_squash: Maps all users to anonymous
//   - root_squash: Maps root (UID 0) to anonymous
//
// The effective identity is what should be used for all permission checks.
//
// Parameters:
//   - shareName: Name of the share
//   - identity: Original client identity (before mapping)
//
// Returns:
//   - *metadata.Identity: Effective identity after applying mapping rules
//   - error: If share not found
func (r *Registry) ApplyIdentityMapping(shareName string, identity *metadata.Identity) (*metadata.Identity, error) {
	r.mu.RLock()
	share, exists := r.shares[shareName]
	r.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("share %q not found", shareName)
	}

	// Create effective identity (copy of original)
	effective := &metadata.Identity{
		UID:      identity.UID,
		GID:      identity.GID,
		GIDs:     identity.GIDs,
		Username: identity.Username,
	}

	// Apply all_squash (map all users to anonymous)
	if share.MapAllToAnonymous {
		anonUID := share.AnonymousUID
		anonGID := share.AnonymousGID
		effective.UID = &anonUID
		effective.GID = &anonGID
		effective.GIDs = []uint32{anonGID}
		effective.Username = fmt.Sprintf("anonymous(%d)", anonUID)
		return effective, nil
	}

	// Apply root_squash (map root to anonymous)
	if share.MapPrivilegedToAnonymous && identity.UID != nil && *identity.UID == 0 {
		anonUID := share.AnonymousUID
		anonGID := share.AnonymousGID
		effective.UID = &anonUID
		effective.GID = &anonGID
		effective.GIDs = []uint32{anonGID}
		effective.Username = fmt.Sprintf("anonymous(%d)", anonUID)
		return effective, nil
	}

	// No mapping applied, return original identity
	return effective, nil
}

// GetShareNameForHandle extracts the share name from a file handle.
//
// This delegates to the metadata store which knows how to decode the handle format.
//
// Parameters:
//   - ctx: Context for cancellation
//   - handle: File handle to extract share name from
//
// Returns:
//   - string: Share name
//   - error: If handle is invalid or store lookup fails
func (r *Registry) GetShareNameForHandle(ctx context.Context, handle metadata.FileHandle) (string, error) {
	// Decode the share name from the handle (format: "shareName:/path")
	// For simplicity, we can just call DecodeShareHandle from metadata package
	shareName, _, err := metadata.DecodeShareHandle(handle)
	if err != nil {
		return "", fmt.Errorf("failed to decode share handle: %w", err)
	}

	// Verify share exists in registry
	r.mu.RLock()
	_, exists := r.shares[shareName]
	r.mu.RUnlock()

	if !exists {
		return "", fmt.Errorf("share %q not found in registry", shareName)
	}

	return shareName, nil
}
