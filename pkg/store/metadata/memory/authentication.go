package memory

import (
	"context"
	"fmt"
	"net"
	"regexp"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// Pre-compiled regular expressions for Administrator SID validation.
// These patterns match well-known Windows administrator SID formats to avoid
// false positives from SIDs that merely end in "-500".
var (
	// domainAdminSIDPattern matches domain administrator accounts.
	// Format: S-1-5-21-<domain identifier (3 parts)>-500
	// Example: S-1-5-21-3623811015-3361044348-30300820-500
	domainAdminSIDPattern = regexp.MustCompile(`^S-1-5-21-\d+-\d+-\d+-500$`)

	// localAdminSIDPattern matches local administrator accounts.
	// Format: S-1-5-<authority>-500
	// Less common but valid for some local administrator accounts
	localAdminSIDPattern = regexp.MustCompile(`^S-1-5-\d+-500$`)
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

			if matchesIPPattern(clientAddr, denied) {
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

			if matchesIPPattern(clientAddr, allowedPattern) {
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
		effectiveIdentity = applyIdentityMapping(identity, opts.IdentityMapping)
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
		return checkOtherPermissions(attr.Mode, requested), nil
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
	var granted metadata.Permission

	if permBits&0x4 != 0 { // Read bit
		granted |= metadata.PermissionRead | metadata.PermissionListDirectory
	}
	if permBits&0x2 != 0 { // Write bit
		granted |= metadata.PermissionWrite | metadata.PermissionDelete
	}
	if permBits&0x1 != 0 { // Execute bit
		granted |= metadata.PermissionExecute | metadata.PermissionTraverse
	}

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

// applyIdentityMapping applies identity transformation rules.
func applyIdentityMapping(identity *metadata.Identity, mapping *metadata.IdentityMapping) *metadata.Identity {
	if mapping == nil {
		return identity
	}

	// Create a copy to avoid modifying the original
	result := &metadata.Identity{
		UID:       identity.UID,
		GID:       identity.GID,
		GIDs:      identity.GIDs,
		SID:       identity.SID,
		GroupSIDs: identity.GroupSIDs,
		Username:  identity.Username,
		Domain:    identity.Domain,
	}

	// Map all users to anonymous
	if mapping.MapAllToAnonymous {
		result.UID = mapping.AnonymousUID
		result.GID = mapping.AnonymousGID
		result.SID = mapping.AnonymousSID
		result.GIDs = nil
		result.GroupSIDs = nil
		return result
	}

	// Map privileged users to anonymous (root squashing)
	if mapping.MapPrivilegedToAnonymous {
		// Unix: Check for root (UID 0)
		if result.UID != nil && *result.UID == 0 {
			result.UID = mapping.AnonymousUID
			result.GID = mapping.AnonymousGID
			result.GIDs = nil
		}

		// Windows: Check for Administrator SID using proper validation
		if result.SID != nil && isAdministratorSID(*result.SID) {
			result.SID = mapping.AnonymousSID
			result.GroupSIDs = nil
		}
	}

	return result
}

// isAdministratorSID checks if a Windows SID represents an administrator account.
//
// This validates against well-known administrator SID patterns:
//   - Built-in Administrator: S-1-5-21-<domain>-500 (domain administrator)
//   - Local Administrator: S-1-5-<authority>-500
//   - Built-in Administrators group: S-1-5-32-544
//
// The function uses proper regex validation to avoid false positives from
// SIDs that happen to end in "-500" but are not actually administrator accounts.
//
// Performance: Uses pre-compiled regex patterns for efficiency on repeated calls.
//
// Parameters:
//   - sid: The Windows SID string to check (e.g., "S-1-5-21-123456789-987654321-111111111-500")
//
// Returns:
//   - bool: true if the SID represents an administrator, false otherwise
//
// References:
//   - https://learn.microsoft.com/en-us/windows-server/identity/ad-ds/manage/understand-security-identifiers
func isAdministratorSID(sid string) bool {
	if sid == "" {
		return false
	}

	// S-1-5-32-544: BUILTIN\Administrators group (well-known SID)
	if sid == "S-1-5-32-544" {
		return true
	}

	// Check against pre-compiled patterns for domain and local administrators
	return domainAdminSIDPattern.MatchString(sid) || localAdminSIDPattern.MatchString(sid)
}

// matchesIPPattern checks if an IP address matches a pattern (CIDR or exact IP).
//
// This uses proper net package parsing for robust IP and CIDR matching.
func matchesIPPattern(clientIP string, pattern string) bool {
	// Try parsing as CIDR first
	_, ipNet, err := net.ParseCIDR(pattern)
	if err == nil {
		ip := net.ParseIP(clientIP)
		if ip != nil {
			return ipNet.Contains(ip)
		}
		return false
	}

	// Otherwise, exact IP match
	return clientIP == pattern
}

// checkOtherPermissions extracts other permission bits from mode.
func checkOtherPermissions(mode uint32, requested metadata.Permission) metadata.Permission {
	var granted metadata.Permission

	// Other bits are bits 0-2 (0o007)
	otherBits := mode & 0x7

	if otherBits&0x4 != 0 { // Read bit
		granted |= metadata.PermissionRead | metadata.PermissionListDirectory
	}
	if otherBits&0x2 != 0 { // Write bit
		granted |= metadata.PermissionWrite | metadata.PermissionDelete
	}
	if otherBits&0x1 != 0 { // Execute bit
		granted |= metadata.PermissionExecute | metadata.PermissionTraverse
	}

	return granted & requested
}
