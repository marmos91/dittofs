package handlers

import (
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/identity"
	"github.com/marmos91/dittofs/pkg/registry"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ErrShareAccessDenied is returned when a user doesn't have permission to access a share.
var ErrShareAccessDenied = errors.New("share access denied")

// BuildAuthContextWithMapping creates an AuthContext with share-level identity mapping applied.
//
// This is a shared helper function used by all NFS v3 handlers to ensure consistent
// identity mapping across all operations. It:
//  1. Uses the share name from the connection layer (already extracted from file handle)
//  2. Looks up the DittoFS user by UID if a UserStore is configured
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
	reg *registry.Registry,
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

	// Look up DittoFS user by UID and check share permissions
	var dittoUser *identity.User
	var sharePermission identity.SharePermission
	shareReadOnly := false

	userStore := reg.GetUserStore()
	share, shareErr := reg.GetShare(shareName)

	if userStore != nil && share != nil {
		// Try to find user by UID
		if nfsCtx.UID != nil {
			dittoUser, _ = userStore.GetUserByUID(*nfsCtx.UID)
		}

		// Get default permission from share config
		defaultPerm := identity.ParseSharePermission(share.DefaultPermission)

		if dittoUser != nil {
			// User found - resolve their permission
			sharePermission = userStore.ResolveSharePermission(dittoUser, shareName, defaultPerm)
			originalIdentity.Username = dittoUser.Username

			if sharePermission == identity.PermissionNone {
				logger.DebugCtx(ctx, "Share access denied", "share", shareName, "user", dittoUser.Username)
				return nil, ErrShareAccessDenied
			}

			// Set read-only if user only has read permission
			shareReadOnly = (sharePermission == identity.PermissionRead)
			logger.DebugCtx(ctx, "User permission resolved", "share", shareName, "user", dittoUser.Username, "permission", sharePermission)
		} else {
			// User not found - check if guest access is allowed
			if !share.AllowGuest {
				// No guest allowed and user not found
				logger.DebugCtx(ctx, "Share access denied (guest not allowed)", "share", shareName, "uid", nfsCtx.UID)
				return nil, ErrShareAccessDenied
			}

			// Guest access - use default permission
			sharePermission = defaultPerm
			if sharePermission == identity.PermissionNone {
				logger.DebugCtx(ctx, "Share access denied (no guest permission)", "share", shareName)
				return nil, ErrShareAccessDenied
			}

			shareReadOnly = (sharePermission == identity.PermissionRead)
			logger.DebugCtx(ctx, "Guest access granted", "share", shareName, "permission", sharePermission)
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
	origUID := "nil"
	if originalIdentity.UID != nil {
		origUID = fmt.Sprintf("%d", *originalIdentity.UID)
	}
	effUID := "nil"
	if effectiveIdentity.UID != nil {
		effUID = fmt.Sprintf("%d", *effectiveIdentity.UID)
	}
	origGID := "nil"
	if originalIdentity.GID != nil {
		origGID = fmt.Sprintf("%d", *originalIdentity.GID)
	}
	effGID := "nil"
	if effectiveIdentity.GID != nil {
		effGID = fmt.Sprintf("%d", *effectiveIdentity.GID)
	}

	if origUID != effUID || origGID != effGID {
		logger.DebugCtx(ctx, "Identity mapping applied", "share", shareName, "original_uid", origUID, "uid", effUID, "original_gid", origGID, "gid", effGID)
	} else {
		logger.DebugCtx(ctx, "Auth context created", "share", shareName, "uid", effUID, "gid", effGID)
	}

	return effectiveAuthCtx, nil
}
