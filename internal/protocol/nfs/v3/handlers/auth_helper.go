package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/registry"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// BuildAuthContextWithMapping creates an AuthContext with share-level identity mapping applied.
//
// This is a shared helper function used by all NFS v3 handlers to ensure consistent
// identity mapping across all operations. It:
//  1. Uses the share name from the connection layer (already extracted from file handle)
//  2. Applies identity mapping rules from the registry (all_squash, root_squash)
//  3. Returns effective credentials for permission checking
//
// Parameters:
//   - nfsCtx: The NFS handler context with client and auth information
//   - reg: Registry to apply identity mapping
//   - shareName: Share name (extracted at connection layer from file handle)
//
// Returns:
//   - *metadata.AuthContext: Auth context with effective (mapped) credentials
//   - error: If identity mapping fails or context is cancelled
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
