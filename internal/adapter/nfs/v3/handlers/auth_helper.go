package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/auth"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ErrShareAccessDenied is returned when a user doesn't have permission to
// access a share. It aliases the shared auth sentinel so existing v3 callers
// (and errors.Is checks) keep working.
var ErrShareAccessDenied = auth.ErrShareAccessDenied

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
	if authFlavor == rpc.AuthUnix {
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

	// Resolve permissions (export-squash policy, shared with NFSv4).
	identityStore := reg.GetIdentityStore()
	permResult, err := auth.ResolveSharePermission(ctx, identityStore, share, shareName, clientAddr, nfsCtx.UID)
	if err != nil {
		return nil, err
	}
	if permResult.Username != "" {
		originalIdentity.Username = permResult.Username
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
		ShareReadOnly: permResult.ReadOnly,
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
