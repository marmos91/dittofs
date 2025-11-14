package badger

import (
	"context"
	"fmt"
	"net"
	"regexp"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata"
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
//
// This implements share-level access control including:
//   - IP-based access control (allowed/denied clients)
//   - Authentication method validation
//   - Identity mapping (squashing, anonymous access)
//
// The method uses a BadgerDB read transaction to retrieve the share configuration
// and perform all access control checks atomically.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Context for cancellation
//   - shareName: Name of the share being accessed
//   - clientAddr: IP address of the client
//   - authMethod: Authentication method used (e.g., "unix", "anonymous")
//   - identity: Client's claimed identity (before mapping)
//
// Returns:
//   - *AccessDecision: Contains allowed status, reason, and share properties
//   - *AuthContext: Contains effective identity after mapping (use for subsequent operations)
//   - error: ErrNotFound if share doesn't exist, or context errors
func (s *BadgerMetadataStore) CheckShareAccess(
	ctx context.Context,
	shareName string,
	clientAddr string,
	authMethod string,
	identity *metadata.Identity,
) (*metadata.AccessDecision, *metadata.AuthContext, error) {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var decision *metadata.AccessDecision
	var authCtx *metadata.AuthContext

	err := s.db.View(func(txn *badger.Txn) error {
		// Step 1: Verify share exists
		item, err := txn.Get(keyShare(shareName))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: fmt.Sprintf("share not found: %s", shareName),
				Path:    shareName,
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get share: %w", err)
		}

		var shareData *shareData
		err = item.Value(func(val []byte) error {
			sd, err := decodeShareData(val)
			if err != nil {
				return err
			}
			shareData = sd
			return nil
		})
		if err != nil {
			return err
		}

		share := &shareData.Share
		opts := share.Options

		// Step 2: Check authentication requirements
		if opts.RequireAuth && authMethod == "anonymous" {
			decision = &metadata.AccessDecision{
				Allowed: false,
				Reason:  "authentication required but anonymous access attempted",
			}
			return nil // No error - this is a business decision
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
				decision = &metadata.AccessDecision{
					Allowed:            false,
					Reason:             fmt.Sprintf("authentication method '%s' not allowed", authMethod),
					AllowedAuthMethods: opts.AllowedAuthMethods,
				}
				return nil // No error - this is a business decision
			}
		}

		// Step 4: Check denied list first (deny takes precedence)
		if len(opts.DeniedClients) > 0 {
			for _, denied := range opts.DeniedClients {
				// Check context during iteration for large lists
				if len(opts.DeniedClients) > 10 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}

				if matchesIPPattern(clientAddr, denied) {
					decision = &metadata.AccessDecision{
						Allowed: false,
						Reason:  fmt.Sprintf("client %s is explicitly denied", clientAddr),
					}
					return nil // No error - this is a business decision
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
						return err
					}
				}

				if matchesIPPattern(clientAddr, allowedPattern) {
					allowed = true
					break
				}
			}
			if !allowed {
				decision = &metadata.AccessDecision{
					Allowed: false,
					Reason:  fmt.Sprintf("client %s not in allowed list", clientAddr),
				}
				return nil // No error - this is a business decision
			}
		}

		// Step 6: Apply identity mapping
		effectiveIdentity := identity
		if identity != nil && opts.IdentityMapping != nil {
			effectiveIdentity = applyIdentityMapping(identity, opts.IdentityMapping)
		}

		// Step 7: Build successful access decision
		decision = &metadata.AccessDecision{
			Allowed:            true,
			Reason:             "",
			AllowedAuthMethods: opts.AllowedAuthMethods,
			ReadOnly:           opts.ReadOnly,
		}

		authCtx = &metadata.AuthContext{
			Context:    ctx,
			AuthMethod: authMethod,
			Identity:   effectiveIdentity,
			ClientAddr: clientAddr,
		}

		return nil
	})

	if err != nil {
		return nil, nil, err
	}

	return decision, authCtx, nil
}

// CheckPermissions performs file-level permission checking.
//
// This implements Unix-style permission checking based on file ownership,
// mode bits, and client credentials. The method uses a BadgerDB read transaction
// to retrieve file attributes and share configuration atomically.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Authentication context with client credentials
//   - handle: The file handle to check permissions for
//   - requested: Bitmap of requested permissions
//
// Returns:
//   - Permission: Bitmap of granted permissions (subset of requested)
//   - error: Only for internal failures (file not found) or context cancellation
func (s *BadgerMetadataStore) CheckPermissions(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
	requested metadata.Permission,
) (metadata.Permission, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return 0, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var granted metadata.Permission

	err := s.db.View(func(txn *badger.Txn) error {
		// Get file data
		item, err := txn.Get(keyFile(handle))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "file not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get file: %w", err)
		}

		var fileData *fileData
		err = item.Value(func(val []byte) error {
			fd, err := decodeFileData(val)
			if err != nil {
				return err
			}
			fileData = fd
			return nil
		})
		if err != nil {
			return err
		}

		attr := fileData.Attr
		identity := ctx.Identity

		// Handle anonymous/no identity case
		if identity == nil || identity.UID == nil {
			// Only grant "other" permissions for anonymous users
			granted = checkOtherPermissions(attr.Mode, requested)
			return nil
		}

		uid := *identity.UID
		gid := identity.GID

		// Root bypass: UID 0 gets all permissions EXCEPT on read-only shares
		if uid == 0 {
			// Check if share is read-only
			shareItem, err := txn.Get(keyShare(fileData.ShareName))
			if err == nil {
				err = shareItem.Value(func(val []byte) error {
					sd, err := decodeShareData(val)
					if err != nil {
						return err
					}
					if sd.Share.Options.ReadOnly {
						// Root gets all permissions except write on read-only shares
						granted = requested &^ (metadata.PermissionWrite | metadata.PermissionDelete)
					} else {
						// Root gets all permissions on normal shares
						granted = requested
					}
					return nil
				})
				if err != nil {
					return err
				}
			} else {
				// Share not found, grant all permissions (shouldn't happen)
				granted = requested
			}
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
		shareItem, err := txn.Get(keyShare(fileData.ShareName))
		if err == nil {
			err = shareItem.Value(func(val []byte) error {
				sd, err := decodeShareData(val)
				if err != nil {
					return err
				}
				if sd.Share.Options.ReadOnly {
					granted &= ^(metadata.PermissionWrite | metadata.PermissionDelete)
				}
				return nil
			})
			if err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return 0, err
	}

	return granted & requested, nil
}

// applyIdentityMapping applies identity transformation rules.
//
// This function implements identity mapping (squashing) rules for NFS shares:
//   - MapAllToAnonymous: All users mapped to anonymous (all_squash in NFS)
//   - MapPrivilegedToAnonymous: Root/administrator mapped to anonymous (root_squash in NFS)
//
// The function returns a copy of the identity with mappings applied, preserving
// the original identity unchanged.
//
// Parameters:
//   - identity: Original client identity
//   - mapping: Identity mapping rules to apply
//
// Returns:
//   - *Identity: Transformed identity (copy)
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
//   - sid: The Windows SID string to check
//
// Returns:
//   - bool: true if the SID represents an administrator, false otherwise
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
// Supports both IPv4 and IPv6 addresses.
//
// Parameters:
//   - clientIP: The client IP address to check
//   - pattern: Either a CIDR range (e.g., "192.168.1.0/24") or exact IP
//
// Returns:
//   - bool: true if the IP matches the pattern, false otherwise
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

// checkOtherPermissions extracts "other" permission bits from mode.
//
// This is used for anonymous users who only get world-readable/writable/executable
// permissions (the "other" bits in Unix mode).
//
// Parameters:
//   - mode: The Unix permission mode
//   - requested: The requested permissions
//
// Returns:
//   - Permission: Granted permissions (subset of requested based on "other" bits)
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
