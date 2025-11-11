package metadata

import (
	"time"
)

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

	// AnonUID is the UID to use for anonymous access when AllSquash, RootSquash (for UID 0), or AUTH_NULL requests are enabled.
	// Default: 65534 (nobody)
	// Used when AllSquash is true, when RootSquash is true and the client UID is 0, or for AUTH_NULL requests.
	AnonUID *uint32

	// AnonGID is the GID to use for anonymous access when AllSquash, RootSquash (for GID 0), or AUTH_NULL requests are enabled.
	// Default: 65534 (nogroup)
	// Used when AllSquash is true, when RootSquash is true and the client GID is 0, or for AUTH_NULL requests.
	AnonGID *uint32

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
