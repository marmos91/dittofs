package postgres

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// CheckShareAccess verifies if a client can access a share and returns effective credentials
// Note: This is a placeholder implementation since shares are managed at the registry level
// PostgreSQL store doesn't maintain share configurations - those are in the registry
func (s *PostgresMetadataStore) CheckShareAccess(
	ctx context.Context,
	shareName string,
	clientAddr string,
	authMethod string,
	identity *metadata.Identity,
) (*metadata.AccessDecision, *metadata.AuthContext, error) {
	// Verify share exists in database
	var exists bool
	query := `SELECT EXISTS(SELECT 1 FROM shares WHERE share_name = $1)`
	err := s.pool.QueryRow(ctx, query, shareName).Scan(&exists)
	if err != nil {
		return nil, nil, mapPgError(err, "CheckShareAccess", shareName)
	}

	if !exists {
		return nil, nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: fmt.Sprintf("share not found: %s", shareName),
			Path:    shareName,
		}
	}

	// Share exists - return success
	// Access control policies (IP filtering, auth requirements) are handled at registry level
	decision := &metadata.AccessDecision{
		Allowed: true,
		Reason:  "",
	}

	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: authMethod,
		Identity:   identity,
		ClientAddr: clientAddr,
	}

	return decision, authCtx, nil
}

// CheckPermissions performs file-level permission checking.
// Returns the intersection of requested and granted permissions (never errors for permission denied).
// Per RFC 1813, ACCESS procedure should return granted permissions, not error when some are denied.
func (s *PostgresMetadataStore) CheckPermissions(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
	requested metadata.Permission,
) (metadata.Permission, error) {
	// Check context
	if err := ctx.Context.Err(); err != nil {
		return 0, err
	}

	// Get file
	file, err := s.GetFile(ctx.Context, handle)
	if err != nil {
		return 0, err
	}

	// Calculate granted permissions
	granted := s.calculateGrantedPermissions(file, ctx)

	// Return intersection of requested and granted (per RFC 1813)
	return granted & requested, nil
}

// calculateGrantedPermissions determines what permissions are granted for a file/user combination
func (s *PostgresMetadataStore) calculateGrantedPermissions(file *metadata.File, ctx *metadata.AuthContext) metadata.Permission {
	attr := &file.FileAttr
	identity := ctx.Identity

	// Handle anonymous/no identity case
	if identity == nil || identity.UID == nil {
		// Only grant "other" permissions for anonymous users
		return metadata.CalculatePermissionsFromBits(attr.Mode & 0x7)
	}

	uid := *identity.UID
	gid := identity.GID

	// Root bypass: UID 0 gets all permissions
	if uid == 0 {
		return metadata.PermissionRead | metadata.PermissionWrite | metadata.PermissionExecute |
			metadata.PermissionDelete | metadata.PermissionListDirectory | metadata.PermissionTraverse |
			metadata.PermissionChangePermissions | metadata.PermissionChangeOwnership
	}

	// Determine which permission bits apply
	var permBits uint32

	if uid == attr.UID {
		// Owner permissions (bits 6-8)
		permBits = (attr.Mode >> 6) & 0x7
	} else if gid != nil && (*gid == attr.GID || identity.HasGID(attr.GID)) {
		// Group permissions (bits 3-5)
		permBits = (attr.Mode >> 3) & 0x7
	} else {
		// Other permissions (bits 0-2)
		permBits = attr.Mode & 0x7
	}

	granted := metadata.CalculatePermissionsFromBits(permBits)

	// Owner gets additional privileges
	if uid == attr.UID {
		granted |= metadata.PermissionChangePermissions | metadata.PermissionChangeOwnership
	}

	return granted
}

// checkAccess is an internal helper that performs permission checking
// Returns nil if access is granted, StoreError if denied
func (s *PostgresMetadataStore) checkAccess(file *metadata.File, ctx *metadata.AuthContext, requested metadata.Permission) error {
	attr := &file.FileAttr
	identity := ctx.Identity

	// Handle anonymous/no identity case
	if identity == nil || identity.UID == nil {
		// Only grant "other" permissions for anonymous users
		granted := metadata.CheckOtherPermissions(attr.Mode, requested)
		if granted != requested {
			return &metadata.StoreError{
				Code:    metadata.ErrPermissionDenied,
				Message: "permission denied for anonymous user",
				Path:    file.Path,
			}
		}
		return nil
	}

	uid := *identity.UID
	gid := identity.GID

	// Root bypass: UID 0 gets all permissions
	// (Note: Read-only share check would be done at registry level)
	if uid == 0 {
		return nil
	}

	// Determine which permission bits apply
	var permBits uint32

	if uid == attr.UID {
		// Owner permissions (bits 6-8)
		permBits = (attr.Mode >> 6) & 0x7
	} else if gid != nil && (*gid == attr.GID || identity.HasGID(attr.GID)) {
		// Group permissions (bits 3-5)
		permBits = (attr.Mode >> 3) & 0x7
	} else {
		// Other permissions (bits 0-2)
		permBits = attr.Mode & 0x7
	}

	// Map Unix permission bits to Permission flags
	granted := metadata.CalculatePermissionsFromBits(permBits)

	// Owner gets additional privileges
	if uid == attr.UID {
		granted |= metadata.PermissionChangePermissions | metadata.PermissionChangeOwnership
	}

	// Check if requested permissions are granted
	if (granted & requested) != requested {
		return &metadata.StoreError{
			Code:    metadata.ErrPermissionDenied,
			Message: fmt.Sprintf("permission denied (uid=%d, mode=%04o)", uid, attr.Mode),
			Path:    file.Path,
		}
	}

	return nil
}
