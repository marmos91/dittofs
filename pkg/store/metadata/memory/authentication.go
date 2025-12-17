package memory

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// CheckShareAccess verifies if a client can access a share and returns effective credentials.
func (store *MemoryMetadataStore) CheckShareAccess(
	ctx context.Context,
	shareName string,
	clientAddr string,
	authMethod string,
	identity *metadata.Identity,
) (*metadata.AccessDecision, *metadata.AuthContext, error) {
	// Check context before acquiring lock
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	// Step 1: Verify share exists (this IS a system error)
	shareData, exists := store.shares[shareName]
	if !exists {
		return nil, nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: fmt.Sprintf("share not found: %s", shareName),
			Path:    shareName,
		}
	}

	share := &shareData.Share
	opts := share.Options

	// Step 2: Check authentication requirements
	if opts.RequireAuth && authMethod == "anonymous" {
		return &metadata.AccessDecision{
			Allowed: false,
			Reason:  "authentication required but anonymous access attempted",
		}, nil, nil // ✅ No error - this is a business decision
	}

	// Step 3: Validate authentication method
	if len(opts.AllowedAuthMethods) > 0 {
		methodAllowed := false
		for _, allowed := range opts.AllowedAuthMethods {
			if authMethod == allowed {
				methodAllowed = true
				break
			}
		}
		if !methodAllowed {
			return &metadata.AccessDecision{
				Allowed:            false,
				Reason:             fmt.Sprintf("authentication method '%s' not allowed", authMethod),
				AllowedAuthMethods: opts.AllowedAuthMethods,
			}, nil, nil // ✅ No error - this is a business decision
		}
	}

	// Step 4: Check denied list first (deny takes precedence)
	if len(opts.DeniedClients) > 0 {
		for _, denied := range opts.DeniedClients {
			// Check context during iteration for large lists
			if len(opts.DeniedClients) > 10 {
				if err := ctx.Err(); err != nil {
					return nil, nil, err
				}
			}

			if metadata.MatchesIPPattern(clientAddr, denied) {
				return &metadata.AccessDecision{
					Allowed: false,
					Reason:  fmt.Sprintf("client %s is explicitly denied", clientAddr),
				}, nil, nil // ✅ No error - this is a business decision
			}
		}
	}

	// Step 5: Check allowed list (if specified)
	if len(opts.AllowedClients) > 0 {
		allowed := false
		for _, allowedPattern := range opts.AllowedClients {
			// Check context during iteration for large lists
			if len(opts.AllowedClients) > 10 {
				if err := ctx.Err(); err != nil {
					return nil, nil, err
				}
			}

			if metadata.MatchesIPPattern(clientAddr, allowedPattern) {
				allowed = true
				break
			}
		}
		if !allowed {
			return &metadata.AccessDecision{
				Allowed: false,
				Reason:  fmt.Sprintf("client %s not in allowed list", clientAddr),
			}, nil, nil // ✅ No error - this is a business decision
		}
	}

	// Step 6: Apply identity mapping
	effectiveIdentity := identity
	if identity != nil && opts.IdentityMapping != nil {
		effectiveIdentity = metadata.ApplyIdentityMapping(identity, opts.IdentityMapping)
	}

	// Step 7: Build successful access decision
	decision := &metadata.AccessDecision{
		Allowed:            true,
		Reason:             "",
		AllowedAuthMethods: opts.AllowedAuthMethods,
		ReadOnly:           opts.ReadOnly,
	}

	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: authMethod,
		Identity:   effectiveIdentity,
		ClientAddr: clientAddr,
	}

	return decision, authCtx, nil
}

// CheckPermissions performs file-level permission checking.
func (store *MemoryMetadataStore) CheckPermissions(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
	requested metadata.Permission,
) (metadata.Permission, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return 0, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	// Get file data
	key := handleToKey(handle)
	fileData, exists := store.files[key]
	if !exists {
		return 0, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	attr := fileData.Attr
	identity := ctx.Identity

	// Handle anonymous/no identity case
	if identity == nil || identity.UID == nil {
		// Only grant "other" permissions for anonymous users
		return metadata.CheckOtherPermissions(attr.Mode, requested), nil
	}

	uid := *identity.UID
	gid := identity.GID

	// Root bypass: UID 0 gets all permissions EXCEPT on read-only shares
	// (root can do operations, but should respect read-only for data integrity)
	if uid == 0 {
		// Check if share is read-only
		if shareData, exists := store.shares[fileData.ShareName]; exists {
			if shareData.Share.Options.ReadOnly {
				// Root gets all permissions except write on read-only shares
				return requested &^ (metadata.PermissionWrite | metadata.PermissionDelete), nil
			}
		}
		// Root gets all permissions on normal shares
		return requested, nil
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

	// Apply read-only share restriction for all non-root users
	if shareData, exists := store.shares[fileData.ShareName]; exists {
		if shareData.Share.Options.ReadOnly {
			granted &= ^(metadata.PermissionWrite | metadata.PermissionDelete)
		}
	}

	return granted & requested, nil
}
