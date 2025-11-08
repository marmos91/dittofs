package metadata

import "time"

type Export struct {
	Path    string
	Options ExportOptions
}

type ExportOptions struct {
	ReadOnly bool
	Async    bool

	// Access control lists
	AllowedClients []string // IP addresses or CIDR ranges, empty = allow all
	DeniedClients  []string // IP addresses or CIDR ranges

	// Authentication requirements
	RequireAuth        bool     // If true, AUTH_NULL is not allowed
	AllowedAuthFlavors []uint32 // Allowed auth flavors, empty = all allowed

	// UID/GID mapping options (squashing)
	// AllSquash maps all UIDs and GIDs to the anonymous user (anonuid/anongid)
	// This is useful for providing world-accessible exports where all users
	// are treated as a single anonymous user regardless of their credentials.
	// When true, all AUTH_UNIX credentials are replaced with AnonUID/AnonGID.
	AllSquash bool

	// AnonUID is the UID to use for anonymous access when AllSquash is enabled.
	// Default: 65534 (nobody)
	// Only used when AllSquash is true or for AUTH_NULL requests.
	AnonUID uint32

	// AnonGID is the GID to use for anonymous access when AllSquash is enabled.
	// Default: 65534 (nogroup)
	// Only used when AllSquash is true or for AUTH_NULL requests.
	AnonGID uint32

	// RootSquash maps root (UID 0) to the anonymous user (anonuid/anongid)
	// This is a security feature to prevent root on NFS clients from having
	// root privileges on the NFS server. When true, UID 0 is mapped to AnonUID.
	// Note: If AllSquash is true, RootSquash is redundant (all users are squashed).
	RootSquash bool
}

// MountEntry represents an active mount by a client
type MountEntry struct {
	ExportPath string
	ClientAddr string
	MountedAt  time.Time
	AuthFlavor uint32

	// Unix authentication details (only present if AuthFlavor == 1)
	// Note: These are the ORIGINAL credentials before squashing
	UnixUID     *uint32
	UnixGID     *uint32
	MachineName string
}

// AccessDecision represents the result of an access control check
type AccessDecision struct {
	Allowed     bool
	Reason      string   // Human-readable reason for denial
	AllowedAuth []uint32 // Auth flavors the client may use
	ReadOnly    bool     // If true, client can only read
}

// AuthContext contains authentication credentials for access control checks.
// This is extracted from the RPC authentication layer and passed to the repository.
//
// IMPORTANT: The UID/GID fields in this structure may have been modified by
// squashing options (AllSquash, RootSquash) and should be used for all
// permission checks. The original credentials are not preserved here.
type AuthContext struct {
	// AuthFlavor is the authentication method used
	// 0 = AUTH_NULL, 1 = AUTH_UNIX, etc.
	AuthFlavor uint32

	// UID is the effective user ID after applying squashing rules.
	// For AUTH_NULL: Set to AnonUID
	// For AUTH_UNIX with AllSquash: Set to AnonUID
	// For AUTH_UNIX with RootSquash and UID==0: Set to AnonUID
	// For AUTH_UNIX otherwise: Original UID from credentials
	// Only valid when AuthFlavor == AUTH_UNIX (or squashed from AUTH_UNIX)
	UID *uint32

	// GID is the effective group ID after applying squashing rules.
	// For AUTH_NULL: Set to AnonGID
	// For AUTH_UNIX with AllSquash: Set to AnonGID
	// For AUTH_UNIX with RootSquash and GID==0: Set to AnonGID
	// For AUTH_UNIX otherwise: Original GID from credentials
	// Only valid when AuthFlavor == AUTH_UNIX (or squashed from AUTH_UNIX)
	GID *uint32

	// GIDs is a list of supplementary group IDs after applying squashing rules.
	// For AUTH_NULL: Empty
	// For AUTH_UNIX with AllSquash: Empty (only AnonGID is used)
	// For AUTH_UNIX with RootSquash: Original GIDs (root squashing doesn't affect supplementary groups)
	// For AUTH_UNIX otherwise: Original supplementary GIDs
	// Only valid when AuthFlavor == AUTH_UNIX (or squashed from AUTH_UNIX)
	GIDs []uint32

	// ClientAddr is the network address of the client
	// Format: "IP:port" or just "IP"
	ClientAddr string
}

// ExportError represents an error that occurred during export operations
type ExportError struct {
	Code    ExportErrorCode
	Message string
	Export  string
}

// Error implements the error interface for ExportError
func (e *ExportError) Error() string {
	return e.Message
}

type ExportErrorCode int

const (
	ExportErrNotFound ExportErrorCode = iota
	ExportErrAccessDenied
	ExportErrAuthRequired
	ExportErrServerFault
)

// DefaultAnonUID is the default anonymous UID (nobody)
const DefaultAnonUID = 65534

// DefaultAnonGID is the default anonymous GID (nogroup)
const DefaultAnonGID = 65534
