package handlers

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// NFSAuthContext represents common NFS authentication context used across all handlers.
// Different handlers have their own context types but they all share these common fields.
type NFSAuthContext interface {
	GetContext() context.Context
	GetClientAddr() string
	GetAuthFlavor() uint32
	GetUID() *uint32
	GetGID() *uint32
	GetGIDs() []uint32
}

// BuildAuthContextWithMapping creates an AuthContext with share-level identity mapping applied.
//
// This is a shared helper function used by all NFS v3 handlers to ensure consistent
// identity mapping across all operations. It:
//  1. Extracts the share name from the path-based file handle
//  2. Applies identity mapping rules from the registry (all_squash, root_squash)
//  3. Returns effective credentials for permission checking
//
// Parameters:
//   - nfsCtx: The NFS context (any handler's context type that implements NFSAuthContext)
//   - reg: Registry to get stores and apply identity mapping
//   - handle: File handle to extract share name from
//
// Returns:
//   - *metadata.AuthContext: Auth context with effective (mapped) credentials
//   - error: If share lookup fails or context is cancelled
func BuildAuthContextWithMapping(
	nfsCtx NFSAuthContext,
	reg RegistryAccessor,
	handle metadata.FileHandle,
) (*metadata.AuthContext, error) {
	ctx := nfsCtx.GetContext()
	clientAddr := nfsCtx.GetClientAddr()
	authFlavor := nfsCtx.GetAuthFlavor()

	// Get share name for this handle using the registry's method.
	// This works for both path-based and hash-based handles.
	shareName, err := reg.GetShareNameForHandle(ctx, handle)
	if err != nil {
		return nil, fmt.Errorf("failed to get share name for handle: %w", err)
	}

	// Map auth flavor to auth method string
	authMethod := "anonymous"
	if authFlavor == 1 {
		authMethod = "unix"
	}

	// Build identity from Unix credentials (before mapping)
	originalIdentity := &metadata.Identity{
		UID:  nfsCtx.GetUID(),
		GID:  nfsCtx.GetGID(),
		GIDs: nfsCtx.GetGIDs(),
	}

	// Set username from UID if available (for logging/auditing)
	if originalIdentity.UID != nil {
		originalIdentity.Username = fmt.Sprintf("uid:%d", *originalIdentity.UID)
	}

	// Apply share-level identity mapping (all_squash, root_squash)
	effectiveIdentity, err := reg.ApplyIdentityMapping(shareName, originalIdentity)
	if err != nil {
		return nil, fmt.Errorf("failed to apply identity mapping: %w", err)
	}

	// Create auth context with the effective (mapped) identity
	effectiveAuthCtx := &metadata.AuthContext{
		Context:    ctx,
		ClientAddr: clientAddr,
		AuthMethod: authMethod,
		Identity:   effectiveIdentity,
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
		logger.Debug("Identity mapping applied: share=%s original_uid=%s->%s original_gid=%s->%s",
			shareName, origUID, effUID, origGID, effGID)
	} else {
		logger.Debug("Auth context created: share=%s uid=%s gid=%s", shareName, effUID, effGID)
	}

	return effectiveAuthCtx, nil
}

// RegistryAccessor defines the minimal registry interface needed by auth_helper.
// This allows for easier testing and decoupling.
type RegistryAccessor interface {
	GetShareNameForHandle(ctx context.Context, handle metadata.FileHandle) (string, error)
	ApplyIdentityMapping(shareName string, identity *metadata.Identity) (*metadata.Identity, error)
}
