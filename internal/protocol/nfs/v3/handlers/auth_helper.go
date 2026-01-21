package handlers

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
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

// permissionResult holds the result of permission resolution.
type permissionResult struct {
	permission models.SharePermission
	readOnly   bool
	username   string
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

	// Get share and identity store
	share, shareErr := reg.GetShare(shareName)
	if shareErr != nil {
		return nil, fmt.Errorf("failed to get share: %w", shareErr)
	}

	// Resolve permissions
	identityStore := reg.GetIdentityStore()
	permResult, err := resolveNFSSharePermission(ctx, nfsCtx, identityStore, share, shareName, clientAddr)
	if err != nil {
		return nil, err
	}
	if permResult.username != "" {
		originalIdentity.Username = permResult.username
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
		ShareReadOnly: permResult.readOnly,
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

// resolveNFSSharePermission resolves the share permission for an NFS request.
// Returns the permission result or an error if access is denied.
func resolveNFSSharePermission(
	ctx context.Context,
	nfsCtx *NFSHandlerContext,
	identityStore models.IdentityStore,
	share *runtime.Share,
	shareName string,
	clientAddr string,
) (*permissionResult, error) {
	result := &permissionResult{}

	// No identity store or UID - allow with share defaults
	if identityStore == nil || share == nil || nfsCtx.UID == nil {
		return result, nil
	}

	defaultPerm := models.ParseSharePermission(share.DefaultPermission)

	// Try reverse lookup: find user by UID
	user, err := identityStore.GetUserByUID(ctx, *nfsCtx.UID)
	if err != nil || user == nil {
		logger.DebugCtx(ctx, "NFS UID reverse lookup failed, treating as guest",
			"share", shareName, "uid", *nfsCtx.UID, "client", clientAddr, "error", err)

		// Guest access - check default permission
		if defaultPerm == models.PermissionNone {
			logger.DebugCtx(ctx, "Share access denied (unknown UID, default permission is none)",
				"share", shareName, "uid", nfsCtx.UID)
			return nil, ErrShareAccessDenied
		}

		result.permission = defaultPerm
		result.readOnly = share.ReadOnly || defaultPerm == models.PermissionRead
		logger.DebugCtx(ctx, "Guest access granted", "share", shareName, "permission", defaultPerm, "readOnly", result.readOnly)
		return result, nil
	}

	// User found - resolve their permission
	logger.DebugCtx(ctx, "NFS UID reverse lookup succeeded",
		"share", shareName, "uid", *nfsCtx.UID, "username", user.Username, "client", clientAddr)

	perm, permErr := identityStore.ResolveSharePermission(ctx, user, shareName)
	if permErr != nil {
		logger.DebugCtx(ctx, "Permission resolution failed, using default",
			"share", shareName, "user", user.Username, "error", permErr, "default", defaultPerm)
		perm = defaultPerm
	}

	if perm == models.PermissionNone {
		logger.DebugCtx(ctx, "Share access denied", "share", shareName, "user", user.Username)
		return nil, ErrShareAccessDenied
	}

	result.permission = perm
	result.readOnly = share.ReadOnly || perm == models.PermissionRead
	result.username = user.Username
	logger.DebugCtx(ctx, "User permission resolved", "share", shareName, "user", user.Username, "permission", perm, "readOnly", result.readOnly)

	return result, nil
}
