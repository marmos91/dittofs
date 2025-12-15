package metadata

import (
	"net"
	"regexp"
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

// ApplyIdentityMapping applies identity transformation rules.
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
func ApplyIdentityMapping(identity *Identity, mapping *IdentityMapping) *Identity {
	if mapping == nil {
		return identity
	}

	// Create a copy to avoid modifying the original
	result := &Identity{
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
func IsAdministratorSID(sid string) bool {
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

// MatchesIPPattern checks if an IP address matches a pattern (CIDR or exact IP).
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
// This maps the 3-bit Unix permission pattern to the internal Permission type:
//   - Bit 2 (0x4): Read → PermissionRead | PermissionListDirectory
//   - Bit 1 (0x2): Write → PermissionWrite | PermissionDelete
//   - Bit 0 (0x1): Execute → PermissionExecute | PermissionTraverse
//
// Parameters:
//   - bits: Unix permission bits (0-7, representing rwx)
//
// Returns:
//   - Permission: Corresponding Permission flags
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
// This is used for anonymous users who only get world-readable/writable/executable
// permissions (the "other" bits in Unix mode).
//
// Parameters:
//   - mode: The Unix permission mode (e.g., 0755)
//   - requested: The requested permissions
//
// Returns:
//   - Permission: Granted permissions (intersection of "other" bits and requested)
func CheckOtherPermissions(mode uint32, requested Permission) Permission {
	// Other bits are bits 0-2 (0o007)
	otherBits := mode & 0x7
	granted := CalculatePermissionsFromBits(otherBits)
	return granted & requested
}

// GetInitialLinkCount returns the initial link count for a new file or directory.
//
// For Unix filesystems:
//   - Regular files start with link count 1
//   - Directories start with link count 2 (for "." and ".." entries)
//
// Parameters:
//   - fileType: The type of file being created
//
// Returns:
//   - uint32: Initial link count (1 for files, 2 for directories)
func GetInitialLinkCount(fileType FileType) uint32 {
	if fileType == FileTypeDirectory {
		return 2 // "." and ".."
	}
	return 1
}

// CopyFileAttr creates a deep copy of a FileAttr structure.
//
// This is useful when returning file attributes to callers to prevent
// external modification of internal state.
//
// Parameters:
//   - attr: The FileAttr to copy
//
// Returns:
//   - *FileAttr: A new FileAttr with the same values
func CopyFileAttr(attr *FileAttr) *FileAttr {
	if attr == nil {
		return nil
	}

	return &FileAttr{
		Type:       attr.Type,
		Mode:       attr.Mode,
		UID:        attr.UID,
		GID:        attr.GID,
		Size:       attr.Size,
		Atime:      attr.Atime,
		Mtime:      attr.Mtime,
		Ctime:      attr.Ctime,
		ContentID:  attr.ContentID,
		LinkTarget: attr.LinkTarget,
	}
}
