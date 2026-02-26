package metadata

import (
	"context"
	"net"
	"regexp"

	"github.com/marmos91/dittofs/pkg/metadata/acl"
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

	// Map all users to anonymous -- no need to deep copy slices
	if mapping.MapAllToAnonymous {
		return &Identity{
			UID:      mapping.AnonymousUID,
			GID:      mapping.AnonymousGID,
			SID:      mapping.AnonymousSID,
			Username: identity.Username,
			Domain:   identity.Domain,
		}
	}

	// Deep copy for other mapping operations
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

// CopyFileAttr creates a deep copy of a FileAttr structure.
//
// Useful when returning file attributes to callers to prevent
// external modification of internal state.
func CopyFileAttr(attr *FileAttr) *FileAttr {
	if attr == nil {
		return nil
	}

	// Deep copy ACL if present
	var aclCopy *acl.ACL
	if attr.ACL != nil {
		aces := make([]acl.ACE, len(attr.ACL.ACEs))
		copy(aces, attr.ACL.ACEs)
		aclCopy = &acl.ACL{ACEs: aces}
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
		ACL:          aclCopy,
	}
}
