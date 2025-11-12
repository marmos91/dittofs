package metadata

import "context"

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
