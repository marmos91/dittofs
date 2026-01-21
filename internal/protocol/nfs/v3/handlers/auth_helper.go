package handlers

import (
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// ErrShareAccessDenied is returned when a user doesn't have permission to access a share.
var ErrShareAccessDenied = errors.New("share access denied")

// formatUID formats an optional UID for logging.
// Returns "nil" if the pointer is nil, otherwise the numeric value as string.
func formatUID(uid *uint32) string {
	if uid == nil {
		return "nil"
	}
	return fmt.Sprintf("%d", *uid)
}

// BuildAuthContextWithMapping creates an AuthContext with share-level identity mapping applied.
//
// This is a shared helper function used by all NFS v3 handlers to ensure consistent
// identity mapping across all operations. It:
//  1. Uses the share name from the connection layer (already extracted from file handle)
//  2. Looks up the DittoFS user by UID using reverse lookup (GetUserByUID)
//  3. Checks share-level permissions for the user
//  4. Applies identity mapping rules from the registry (all_squash, root_squash)
//  5. Returns effective credentials for permission checking
//
// Parameters:
//   - nfsCtx: The NFS handler context with client and auth information
//   - reg: Registry to apply identity mapping
//   - shareName: Share name (extracted at connection layer from file handle)
//
// Returns:
//   - *metadata.AuthContext: Auth context with effective (mapped) credentials
//   - error: If identity mapping fails, access is denied, or context is cancelled
func BuildAuthContextWithMapping(
	nfsCtx *NFSHandlerContext,
	reg *runtime.Runtime,
	shareName string,
) (*metadata.AuthContext, error) {
	ctx := nfsCtx.Context
	clientAddr := nfsCtx.ClientAddr
	authFlavor := nfsCtx.AuthFlavor

	// Map auth flavor to auth method string
	authMethod := "anonymous"
	if authFlavor == 1 {
		authMethod = "unix"
	}

	// Build identity from Unix credentials (before mapping)
	originalIdentity := &metadata.Identity{
		UID:  nfsCtx.UID,
		GID:  nfsCtx.GID,
		GIDs: nfsCtx.GIDs,
	}

	// Set username from UID if available (for logging/auditing)
	if originalIdentity.UID != nil {
		originalIdentity.Username = fmt.Sprintf("uid:%d", *originalIdentity.UID)
	}

	// Look up DittoFS user by UID (reverse lookup) and check share permissions
	var dittoUser *models.User
	var sharePermission models.SharePermission
	shareReadOnly := false

	identityStore := reg.GetIdentityStore()
	share, shareErr := reg.GetShare(shareName)

	if identityStore != nil && share != nil && nfsCtx.UID != nil {
		// Try reverse lookup: find user by UID
		user, err := identityStore.GetUserByUID(ctx, *nfsCtx.UID)
		if err == nil && user != nil {
			dittoUser = user
			logger.DebugCtx(ctx, "NFS UID reverse lookup succeeded",
				"share", shareName, "uid", *nfsCtx.UID, "username", user.Username, "client", clientAddr)
		} else {
			logger.DebugCtx(ctx, "NFS UID reverse lookup failed, treating as guest",
				"share", shareName, "uid", *nfsCtx.UID, "client", clientAddr, "error", err)
		}

		// Get default permission from share config
		defaultPerm := models.ParseSharePermission(share.DefaultPermission)

		if dittoUser != nil {
			// User found - resolve their permission
			perm, permErr := identityStore.ResolveSharePermission(ctx, dittoUser, shareName)
			if permErr != nil {
				// Fall back to default permission if resolution fails
				sharePermission = defaultPerm
				logger.DebugCtx(ctx, "Permission resolution failed, using default",
					"share", shareName, "user", dittoUser.Username, "error", permErr, "default", defaultPerm)
			} else {
				sharePermission = perm
			}
			originalIdentity.Username = dittoUser.Username

			if sharePermission == models.PermissionNone {
				logger.DebugCtx(ctx, "Share access denied", "share", shareName, "user", dittoUser.Username)
				return nil, ErrShareAccessDenied
			}

			// Read-only if share is read-only OR user only has read permission
			shareReadOnly = share.ReadOnly || sharePermission == models.PermissionRead
			logger.DebugCtx(ctx, "User permission resolved", "share", shareName, "user", dittoUser.Username, "permission", sharePermission, "readOnly", shareReadOnly)
		} else {
			// User not found - use default permission
			// If defaultPerm is "none", block access (no guest access)
			if defaultPerm == models.PermissionNone {
				logger.DebugCtx(ctx, "Share access denied (unknown UID, default permission is none)",
					"share", shareName, "uid", nfsCtx.UID)
				return nil, ErrShareAccessDenied
			}

			// Allow with default permission
			sharePermission = defaultPerm
			// Read-only if share is read-only OR permission is read-only
			shareReadOnly = share.ReadOnly || sharePermission == models.PermissionRead
			logger.DebugCtx(ctx, "Guest access granted", "share", shareName, "permission", sharePermission, "readOnly", shareReadOnly)
		}
	} else if shareErr != nil {
		return nil, fmt.Errorf("failed to get share: %w", shareErr)
	}

	// Apply share-level identity mapping (all_squash, root_squash)
	effectiveIdentity, err := reg.ApplyIdentityMapping(shareName, originalIdentity)
	if err != nil {
		return nil, fmt.Errorf("failed to apply identity mapping: %w", err)
	}

	// Create auth context with the effective (mapped) identity
	effectiveAuthCtx := &metadata.AuthContext{
		Context:       ctx,
		ClientAddr:    clientAddr,
		AuthMethod:    authMethod,
		Identity:      effectiveIdentity,
		ShareReadOnly: shareReadOnly,
	}

	// Log identity mapping
	origUID, effUID := formatUID(originalIdentity.UID), formatUID(effectiveIdentity.UID)
	origGID, effGID := formatUID(originalIdentity.GID), formatUID(effectiveIdentity.GID)

	if origUID != effUID || origGID != effGID {
		logger.DebugCtx(ctx, "Identity mapping applied", "share", shareName, "original_uid", origUID, "uid", effUID, "original_gid", origGID, "gid", effGID)
	} else {
		logger.DebugCtx(ctx, "Auth context created", "share", shareName, "uid", effUID, "gid", effGID)
	}

	return effectiveAuthCtx, nil
}
