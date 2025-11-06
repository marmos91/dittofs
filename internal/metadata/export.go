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
}

// MountEntry represents an active mount by a client
type MountEntry struct {
	ExportPath string
	ClientAddr string
	MountedAt  time.Time
	AuthFlavor uint32

	// Unix authentication details (only present if AuthFlavor == 1)
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

// Common error types for export operations
type ExportError struct {
	Code    ExportErrorCode
	Message string
	Export  string
}

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
