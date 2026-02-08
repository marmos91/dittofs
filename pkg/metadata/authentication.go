package metadata

import (
	"context"
	"net"
	"regexp"
)

// ============================================================================
// Authentication Context
// ============================================================================

// AuthContext contains authentication information for access control checks.
//
// This is passed to all operations that require permission checking. It contains
// the client's identity after applying share-level identity mapping rules.
//
// The Context field should be checked for cancellation at appropriate points
// during long-running operations.
type AuthContext struct {
	// Context carries cancellation signals and deadlines
	Context context.Context

	// AuthMethod is the authentication method used by the client
	// Examples: "anonymous", "unix", "kerberos", "ntlm", "oauth"
	AuthMethod string

	// Identity contains the effective client identity after applying mapping rules
	// This is what should be used for all permission checks
	Identity *Identity

	// ClientAddr is the network address of the client
	// Format: "IP:port" or just "IP"
	ClientAddr string

	// ShareReadOnly indicates whether the user has read-only access to the share
	// This is determined by share-level user permissions (identity.SharePermission)
	// When true, all write operations to this share should be denied
	ShareReadOnly bool

	// ShareWritable indicates whether the user has share-level write permission.
	// When true, the user can write to files in the share regardless of file-level
	// Unix permissions. This is used to implement share-based access control where
	// authenticated users with share write permission bypass file-level permission checks.
	// This is similar to how root bypass works, but applies to any user with share
	// write permission.
	ShareWritable bool
}

// ============================================================================
// Identity Types
// ============================================================================

// Identity represents a client's identity across different authentication systems.
//
// This structure supports multiple identity systems to accommodate different protocols:
//   - Unix-style: UID/GID (used by NFS, FTP, SSH, etc.)
//   - Windows-style: SID (used by SMB/CIFS)
//   - Generic: Username/Domain (used by HTTP, WebDAV, etc.)
//
// Not all fields need to be populated - it depends on the authentication method
// and protocol in use.
type Identity struct {
	// Unix-style identity
	// Used by protocols that follow POSIX permission models

	// UID is the user ID
	// nil for anonymous or non-Unix authentication
	UID *uint32

	// GID is the primary group ID
	// nil for anonymous or non-Unix authentication
	GID *uint32

	// GIDs is a list of supplementary group IDs
	// Used for group membership checks
	// Empty for anonymous or simple authentication
	GIDs []uint32

	// gidSet is a cached map for O(1) group membership lookups
	// Automatically populated from GIDs on first use
	// Not exported - internal optimization detail
	gidSet map[uint32]struct{}

	// Windows-style identity
	// Used by SMB/CIFS and Windows-based protocols

	// SID is the Security Identifier
	// Example: "S-1-5-21-3623811015-3361044348-30300820-1013"
	// nil for non-Windows authentication
	SID *string

	// GroupSIDs is a list of group Security Identifiers
	// Used for group membership checks in Windows
	// Empty for non-Windows authentication
	GroupSIDs []string

	// Generic identity
	// Used across all protocols

	// Username is the authenticated username
	// Empty for anonymous access
	Username string

	// Domain is the authentication domain
	// Examples: "WORKGROUP", "EXAMPLE.COM", "example.com"
	// Empty for local authentication
	Domain string
}

// HasGID checks if the identity has the specified group ID in its supplementary groups.
//
// This method provides O(1) group membership lookup by lazily building and caching
// a map on first use. For users with many supplementary groups (e.g., 50-100+),
// this is significantly faster than linear search.
//
// Thread safety: This method is NOT thread-safe. Identity objects should not be
// shared across goroutines, or callers must provide their own synchronization.
//
// Parameters:
//   - gid: The group ID to check for
//
// Returns:
//   - bool: true if the GID is in the supplementary groups list, false otherwise
func (i *Identity) HasGID(gid uint32) bool {
	if len(i.GIDs) == 0 {
		return false
	}

	// Lazy initialization of the GID set
	if i.gidSet == nil {
		i.gidSet = make(map[uint32]struct{}, len(i.GIDs))
		for _, g := range i.GIDs {
			i.gidSet[g] = struct{}{}
		}
	}

	_, exists := i.gidSet[gid]
	return exists
}

// IdentityMapping defines how client identities are transformed.
//
// This supports various identity mapping scenarios:
//   - Anonymous access (map all users to anonymous)
//   - Root squashing (map privileged users to anonymous for security)
//   - Custom mappings (future: map specific users/groups)
type IdentityMapping struct {
	// MapAllToAnonymous maps all users to the anonymous user
	// When true, all authenticated users are treated as anonymous
	// Useful for world-accessible shares
	MapAllToAnonymous bool

	// MapPrivilegedToAnonymous maps privileged users (root/admin) to anonymous
	// Security feature to prevent root on clients from having root on server
	// In Unix: Maps UID 0 to AnonymousUID
	// In Windows: Maps Administrator to anonymous
	MapPrivilegedToAnonymous bool

	// AnonymousUID is the UID to use for anonymous or mapped users
	// Typically 65534 (nobody) in Unix systems
	AnonymousUID *uint32

	// AnonymousGID is the GID to use for anonymous or mapped users
	// Typically 65534 (nogroup) in Unix systems
	AnonymousGID *uint32

	// AnonymousSID is the SID to use for anonymous users in Windows
	// Example: "S-1-5-7" (ANONYMOUS LOGON)
	AnonymousSID *string
}

// ============================================================================
// Access Control
// ============================================================================

// AccessDecision contains the result of a share-level access control check.
//
// This is returned by CheckShareAccess to inform the protocol handler whether
// access is allowed and what restrictions apply.
type AccessDecision struct {
	// Allowed indicates whether access is granted
	Allowed bool

	// Reason provides a human-readable explanation for denial
	// Examples: "Client IP not in allowed list", "Authentication required"
	// Empty when Allowed is true
	Reason string

	// AllowedAuthMethods lists authentication methods the client may use
	// Only populated when access is allowed or when suggesting alternatives
	AllowedAuthMethods []string

	// ReadOnly indicates whether the client has read-only access
	// When true, all write operations should be denied
	ReadOnly bool
}

// CheckShareAccess verifies if a client can access a share and returns effective credentials.
//
// This implements share-level access control including:
//   - Authentication method validation
//   - IP-based access control (allowed/denied clients)
//   - Identity mapping (squashing, anonymous access)
func (s *MetadataService) CheckShareAccess(ctx context.Context, shareName, clientAddr, authMethod string, identity *Identity) (*AccessDecision, *AuthContext, error) {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return nil, nil, err
	}

	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	// Get share options using CRUD operation
	opts, err := store.GetShareOptions(ctx, shareName)
	if err != nil {
		return nil, nil, err
	}

	// Step 1: Check authentication requirements
	if opts.RequireAuth && authMethod == "anonymous" {
		return &AccessDecision{
			Allowed: false,
			Reason:  "authentication required but anonymous access attempted",
		}, nil, nil
	}

	// Step 2: Validate authentication method
	if len(opts.AllowedAuthMethods) > 0 {
		methodAllowed := false
		for _, allowed := range opts.AllowedAuthMethods {
			if authMethod == allowed {
				methodAllowed = true
				break
			}
		}
		if !methodAllowed {
			return &AccessDecision{
				Allowed:            false,
				Reason:             "authentication method '" + authMethod + "' not allowed",
				AllowedAuthMethods: opts.AllowedAuthMethods,
			}, nil, nil
		}
	}

	// Step 3: Check denied list first (deny takes precedence)
	for _, denied := range opts.DeniedClients {
		// Check context during iteration for large lists
		if len(opts.DeniedClients) > 10 {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
		}

		if MatchesIPPattern(clientAddr, denied) {
			return &AccessDecision{
				Allowed: false,
				Reason:  "client " + clientAddr + " is explicitly denied",
			}, nil, nil
		}
	}

	// Step 4: Check allowed list (if specified)
	if len(opts.AllowedClients) > 0 {
		allowed := false
		for _, allowedPattern := range opts.AllowedClients {
			// Check context during iteration for large lists
			if len(opts.AllowedClients) > 10 {
				if err := ctx.Err(); err != nil {
					return nil, nil, err
				}
			}

			if MatchesIPPattern(clientAddr, allowedPattern) {
				allowed = true
				break
			}
		}
		if !allowed {
			return &AccessDecision{
				Allowed: false,
				Reason:  "client " + clientAddr + " not in allowed list",
			}, nil, nil
		}
	}

	// Step 5: Apply identity mapping
	effectiveIdentity := identity
	if identity != nil && opts.IdentityMapping != nil {
		effectiveIdentity = ApplyIdentityMapping(identity, opts.IdentityMapping)
	}

	// Step 6: Build successful access decision
	decision := &AccessDecision{
		Allowed:            true,
		Reason:             "",
		AllowedAuthMethods: opts.AllowedAuthMethods,
		ReadOnly:           opts.ReadOnly,
	}

	authCtx := &AuthContext{
		Context:    ctx,
		AuthMethod: authMethod,
		Identity:   effectiveIdentity,
		ClientAddr: clientAddr,
	}

	return decision, authCtx, nil
}

// ============================================================================
// Permission Types
// ============================================================================

// Permission represents filesystem permission flags.
//
// These are generic permission flags that map to different protocol-specific
// permission models. Protocol handlers translate between Permission and
// protocol-specific permission bits (e.g., NFS ACCESS bits, SMB access masks).
type Permission uint32

const (
	// PermissionRead allows reading file data or listing directory contents
	PermissionRead Permission = 1 << iota

	// PermissionWrite allows modifying file data or directory contents
	PermissionWrite

	// PermissionExecute allows executing files or traversing directories
	PermissionExecute

	// PermissionDelete allows deleting files or directories
	PermissionDelete

	// PermissionListDirectory allows listing directory entries (read for directories)
	PermissionListDirectory

	// PermissionTraverse allows searching/traversing directories (execute for directories)
	PermissionTraverse

	// PermissionChangePermissions allows changing file/directory permissions (chmod)
	PermissionChangePermissions

	// PermissionChangeOwnership allows changing file/directory ownership (chown)
	PermissionChangeOwnership
)

// ============================================================================
// Identity Mapping
// ============================================================================

// Pre-compiled regular expression for Administrator SID validation.
var (
	// domainAdminSIDPattern matches domain/local administrator accounts.
	// Format: S-1-5-21-<domain identifier (3 parts)>-500
	domainAdminSIDPattern = regexp.MustCompile(`^S-1-5-21-\d+-\d+-\d+-500$`)
)

// ApplyIdentityMapping applies identity transformation rules.
//
// This function implements identity mapping (squashing) rules for NFS shares:
//   - MapAllToAnonymous: All users mapped to anonymous (all_squash in NFS)
//   - MapPrivilegedToAnonymous: Root/administrator mapped to anonymous (root_squash in NFS)
//
// When mapping is nil, returns the original identity pointer unchanged.
// When mapping is applied, returns a new Identity with the transformations applied.
func ApplyIdentityMapping(identity *Identity, mapping *IdentityMapping) *Identity {
	if mapping == nil {
		return identity
	}

	// Create a deep copy to avoid modifying the original
	var gidsCopy []uint32
	if identity.GIDs != nil {
		gidsCopy = make([]uint32, len(identity.GIDs))
		copy(gidsCopy, identity.GIDs)
	}

	var groupSIDsCopy []string
	if identity.GroupSIDs != nil {
		groupSIDsCopy = make([]string, len(identity.GroupSIDs))
		copy(groupSIDsCopy, identity.GroupSIDs)
	}

	result := &Identity{
		UID:       identity.UID,
		GID:       identity.GID,
		GIDs:      gidsCopy,
		SID:       identity.SID,
		GroupSIDs: groupSIDsCopy,
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

		// Windows: Check for Administrator SID
		if result.SID != nil && IsAdministratorSID(*result.SID) {
			result.SID = mapping.AnonymousSID
			result.GroupSIDs = nil
		}
	}

	return result
}

// IsAdministratorSID checks if a Windows SID represents an administrator account.
//
// This validates against well-known administrator SID patterns:
//   - Domain/Local Administrator: S-1-5-21-<3 sub-authorities>-500
//   - Built-in Administrators group: S-1-5-32-544
func IsAdministratorSID(sid string) bool {
	if sid == "" {
		return false
	}

	// S-1-5-32-544: BUILTIN\Administrators group
	if sid == "S-1-5-32-544" {
		return true
	}

	return domainAdminSIDPattern.MatchString(sid)
}

// MatchesIPPattern checks if an IP address matches a pattern (CIDR or exact IP).
//
// Supports both IPv4 and IPv6 addresses.
func MatchesIPPattern(clientIP string, pattern string) bool {
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

// ============================================================================
// Permission Checking
// ============================================================================

// CalculatePermissionsFromBits converts Unix permission bits (rwx) to Permission flags.
//
// Maps the 3-bit Unix permission pattern to the internal Permission type:
//   - Bit 2 (0x4): Read -> PermissionRead | PermissionListDirectory
//   - Bit 1 (0x2): Write -> PermissionWrite | PermissionDelete
//   - Bit 0 (0x1): Execute -> PermissionExecute | PermissionTraverse
func CalculatePermissionsFromBits(bits uint32) Permission {
	var granted Permission

	if bits&0x4 != 0 { // Read bit
		granted |= PermissionRead | PermissionListDirectory
	}
	if bits&0x2 != 0 { // Write bit
		granted |= PermissionWrite | PermissionDelete
	}
	if bits&0x1 != 0 { // Execute bit
		granted |= PermissionExecute | PermissionTraverse
	}

	return granted
}

// CheckOtherPermissions extracts "other" permission bits from mode and returns granted permissions.
//
// Used for anonymous users who only get world-readable/writable/executable
// permissions (the "other" bits in Unix mode).
func CheckOtherPermissions(mode uint32, requested Permission) Permission {
	// Other bits are bits 0-2 (0o007)
	otherBits := mode & 0x7
	granted := CalculatePermissionsFromBits(otherBits)
	return granted & requested
}

// CopyFileAttr creates a deep copy of a FileAttr structure.
//
// Useful when returning file attributes to callers to prevent
// external modification of internal state.
func CopyFileAttr(attr *FileAttr) *FileAttr {
	if attr == nil {
		return nil
	}

	return &FileAttr{
		Type:         attr.Type,
		Mode:         attr.Mode,
		UID:          attr.UID,
		GID:          attr.GID,
		Nlink:        attr.Nlink,
		Size:         attr.Size,
		Atime:        attr.Atime,
		Mtime:        attr.Mtime,
		Ctime:        attr.Ctime,
		CreationTime: attr.CreationTime,
		PayloadID:    attr.PayloadID,
		LinkTarget:   attr.LinkTarget,
		Rdev:         attr.Rdev,
		Hidden:       attr.Hidden,
	}
}

// ============================================================================
// Permission Checking Methods (MetadataService)
// ============================================================================

// checkFilePermissions performs Unix-style permission checking on a file.
//
// This implements the standard Unix permission model:
//   - Root (UID 0): Bypass all checks (all permissions granted), except on read-only shares
//   - Owner: Check owner permission bits (mode >> 6 & 0x7)
//   - Group member: Check group permission bits (mode >> 3 & 0x7)
//   - Other: Check other permission bits (mode & 0x7)
//   - Anonymous: Only world permissions
func (s *MetadataService) checkFilePermissions(ctx *AuthContext, handle FileHandle, requested Permission) (Permission, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return 0, err
	}

	// Check context
	if err := ctx.Context.Err(); err != nil {
		return 0, err
	}

	// Get file data using CRUD method
	file, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return 0, err
	}

	// Get share options for read-only check
	shareOpts, err := store.GetShareOptions(ctx.Context, file.ShareName)
	if err != nil {
		// If we can't get share options, continue without read-only check
		shareOpts = nil
	}

	// Calculate base permissions from file mode and identity
	basePerms := calculatePermissions(file, ctx.Identity, shareOpts, requested)

	// Share-level write permission bypass:
	// If the user has share-level write permission (ctx.ShareWritable), grant write-
	// related permissions on files in the share, bypassing file-level Unix permission
	// checks. This allows authenticated users with share write access to create/modify
	// files even if the file's Unix permissions would normally deny access.
	//
	// Note: ShareReadOnly takes precedence - if the share is read-only for this user,
	// write permission is denied regardless of ShareWritable.
	if ctx.ShareWritable && !ctx.ShareReadOnly {
		// Grant write permissions via share-level bypass, combined with base permissions
		writePerms := requested & (PermissionWrite | PermissionDelete)
		return basePerms | writePerms, nil
	}

	return basePerms, nil
}

// calculatePermissions computes granted permissions based on file attributes and identity.
func calculatePermissions(
	file *File,
	identity *Identity,
	shareOpts *ShareOptions,
	requested Permission,
) Permission {
	attr := &file.FileAttr

	// Handle anonymous/no identity case
	if identity == nil || identity.UID == nil {
		// Only grant "other" permissions for anonymous users
		return CheckOtherPermissions(attr.Mode, requested)
	}

	uid := *identity.UID
	gid := identity.GID

	// Root bypass: UID 0 gets all permissions EXCEPT on read-only shares
	if uid == 0 {
		if shareOpts != nil && shareOpts.ReadOnly {
			// Root gets all permissions except write on read-only shares
			return requested &^ (PermissionWrite | PermissionDelete)
		}
		// Root gets all permissions on normal shares
		return requested
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
	granted := CalculatePermissionsFromBits(permBits)

	// Owner gets additional privileges
	if uid == attr.UID {
		granted |= PermissionChangePermissions | PermissionChangeOwnership
	}

	// Apply read-only share restriction for all non-root users
	if shareOpts != nil && shareOpts.ReadOnly {
		granted &= ^(PermissionWrite | PermissionDelete)
	}

	return granted & requested
}

// checkWritePermission checks write permission on a file.
func (s *MetadataService) checkWritePermission(ctx *AuthContext, handle FileHandle) error {
	granted, err := s.checkFilePermissions(ctx, handle, PermissionWrite)
	if err != nil {
		return err
	}

	if granted&PermissionWrite == 0 {
		return &StoreError{
			Code:    ErrAccessDenied,
			Message: "write permission denied",
		}
	}

	return nil
}

// checkReadPermission checks read permission on a file.
func (s *MetadataService) checkReadPermission(ctx *AuthContext, handle FileHandle) error {
	granted, err := s.checkFilePermissions(ctx, handle, PermissionRead)
	if err != nil {
		return err
	}

	if granted&PermissionRead == 0 {
		return &StoreError{
			Code:    ErrAccessDenied,
			Message: "read permission denied",
		}
	}

	return nil
}

// checkExecutePermission checks execute/traverse permission on a file.
func (s *MetadataService) checkExecutePermission(ctx *AuthContext, handle FileHandle) error {
	granted, err := s.checkFilePermissions(ctx, handle, PermissionExecute)
	if err != nil {
		return err
	}

	if granted&PermissionExecute == 0 {
		return &StoreError{
			Code:    ErrAccessDenied,
			Message: "execute permission denied",
		}
	}

	return nil
}
