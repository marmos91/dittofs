package metadata

import (
	"net"
	"regexp"
)

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
		ContentID:    attr.ContentID,
		LinkTarget:   attr.LinkTarget,
		Rdev:         attr.Rdev,
		Hidden:       attr.Hidden,
	}
}

// ============================================================================
// High-Level Permission Checking Functions
// ============================================================================

// CheckFilePermissions performs Unix-style permission checking on a file.
//
// This implements the standard Unix permission model:
//   - Root (UID 0): Bypass all checks (all permissions granted), except on read-only shares
//   - Owner: Check owner permission bits (mode >> 6 & 0x7)
//   - Group member: Check group permission bits (mode >> 3 & 0x7)
//   - Other: Check other permission bits (mode & 0x7)
//   - Anonymous: Only world permissions
func CheckFilePermissions(
	store MetadataStore,
	ctx *AuthContext,
	handle FileHandle,
	requested Permission,
) (Permission, error) {
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

	return calculatePermissions(file, ctx.Identity, shareOpts, requested), nil
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

// CheckWritePermission is a convenience function that checks write permission.
func CheckWritePermission(store MetadataStore, ctx *AuthContext, handle FileHandle) error {
	granted, err := CheckFilePermissions(store, ctx, handle, PermissionWrite)
	if err != nil {
		return err
	}

	if granted&PermissionWrite == 0 {
		return &StoreError{
			Code:    ErrPermissionDenied,
			Message: "write permission denied",
		}
	}

	return nil
}

// CheckReadPermission is a convenience function that checks read permission.
func CheckReadPermission(store MetadataStore, ctx *AuthContext, handle FileHandle) error {
	granted, err := CheckFilePermissions(store, ctx, handle, PermissionRead)
	if err != nil {
		return err
	}

	if granted&PermissionRead == 0 {
		return &StoreError{
			Code:    ErrPermissionDenied,
			Message: "read permission denied",
		}
	}

	return nil
}

// CheckExecutePermission is a convenience function that checks execute/traverse permission.
func CheckExecutePermission(store MetadataStore, ctx *AuthContext, handle FileHandle) error {
	granted, err := CheckFilePermissions(store, ctx, handle, PermissionExecute)
	if err != nil {
		return err
	}

	if granted&PermissionExecute == 0 {
		return &StoreError{
			Code:    ErrPermissionDenied,
			Message: "execute permission denied",
		}
	}

	return nil
}

// CheckDirectoryWritePermission checks write permission on a directory.
func CheckDirectoryWritePermission(store MetadataStore, ctx *AuthContext, dirHandle FileHandle) error {
	// Get directory entry
	dir, err := store.GetFile(ctx.Context, dirHandle)
	if err != nil {
		return err
	}

	// Verify it's a directory
	if dir.Type != FileTypeDirectory {
		return &StoreError{
			Code:    ErrNotDirectory,
			Message: "not a directory",
			Path:    dir.Path,
		}
	}

	// Check write permission
	return CheckWritePermission(store, ctx, dirHandle)
}
