package handlers

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
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
//  2. Looks up the share configuration
//  3. Applies identity mapping rules (all_squash, root_squash)
//  4. Returns effective credentials for permission checking
//
// Parameters:
//   - nfsCtx: The NFS context (any handler's context type that implements NFSAuthContext)
//   - store: Metadata store to query share configuration
//   - handle: File handle to extract share name from
//
// Returns:
//   - *metadata.AuthContext: Auth context with effective (mapped) credentials
//   - error: If share lookup fails or context is cancelled
func BuildAuthContextWithMapping(
	nfsCtx NFSAuthContext,
	store metadata.MetadataStore,
	handle metadata.FileHandle,
) (*metadata.AuthContext, error) {
	ctx := nfsCtx.GetContext()
	clientAddr := nfsCtx.GetClientAddr()
	authFlavor := nfsCtx.GetAuthFlavor()

	// Extract share name from path-based file handle
	// Format: "shareName:/path" or "shareName:/"
	handleStr := string(handle)
	shareName := handleStr
	if colonIdx := len(handleStr); colonIdx > 0 {
		for i, ch := range handleStr {
			if ch == ':' {
				shareName = handleStr[:i]
				break
			}
		}
	}

	// Map auth flavor to auth method string
	authMethod := "anonymous"
	if authFlavor == 1 {
		authMethod = "unix"
	}

	// Build identity from Unix credentials (before mapping)
	identity := &metadata.Identity{
		UID:  nfsCtx.GetUID(),
		GID:  nfsCtx.GetGID(),
		GIDs: nfsCtx.GetGIDs(),
	}

	// Set username from UID if available (for logging/auditing)
	if identity.UID != nil {
		identity.Username = fmt.Sprintf("uid:%d", *identity.UID)
	}

	// Extract client IP for CheckShareAccess
	clientIP := xdr.ExtractClientIP(clientAddr)

	// Apply share-level identity mapping via CheckShareAccess
	// This applies all_squash, root_squash, and other identity mapping rules
	_, effectiveAuthCtx, err := store.CheckShareAccess(
		ctx,
		shareName,
		clientIP,
		authMethod,
		identity,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to check share access for identity mapping: %w", err)
	}

	// Log identity mapping (dereference pointers for readable output)
	origUID := "nil"
	if identity.UID != nil {
		origUID = fmt.Sprintf("%d", *identity.UID)
	}
	effUID := "nil"
	if effectiveAuthCtx.Identity.UID != nil {
		effUID = fmt.Sprintf("%d", *effectiveAuthCtx.Identity.UID)
	}
	origGID := "nil"
	if identity.GID != nil {
		origGID = fmt.Sprintf("%d", *identity.GID)
	}
	effGID := "nil"
	if effectiveAuthCtx.Identity.GID != nil {
		effGID = fmt.Sprintf("%d", *effectiveAuthCtx.Identity.GID)
	}

	logger.Debug("Identity mapping: share=%s original_uid=%s effective_uid=%s original_gid=%s effective_gid=%s",
		shareName, origUID, effUID, origGID, effGID)

	return effectiveAuthCtx, nil
}
