package metadata

// Share represents a filesystem path made available to clients with specific access rules.
//
// A share is the root point that clients connect to. It contains the name being
// shared and the access control rules that apply to all files within the share.
type Share struct {
	// Name is the unique identifier for the share
	// Format is implementation-specific (e.g., "/export/data", "projects")
	Name string

	// Options contains access control and authentication settings for the share
	Options ShareOptions
}

// ShareOptions defines access control and authentication settings for a share.
//
// These options apply to all files and directories within the share and are
// checked before any file-level permission checks.
type ShareOptions struct {
	// ReadOnly makes the share read-only
	// All write operations are denied regardless of file permissions
	ReadOnly bool

	// Async allows asynchronous write operations
	// When true, write operations may return success before data is fully persisted
	// When false, write operations are synchronous (data committed before success)
	Async bool

	// AllowedClients is a list of IP addresses or CIDR ranges allowed to access the share
	// Empty list means all clients are allowed (unless in DeniedClients)
	// Examples: ["192.168.1.0/24", "10.0.0.100"]
	AllowedClients []string

	// DeniedClients is a list of IP addresses or CIDR ranges explicitly denied access
	// Denial takes precedence over AllowedClients
	// Examples: ["192.168.1.50", "10.0.0.0/8"]
	DeniedClients []string

	// RequireAuth requires clients to authenticate
	// When true, anonymous access is not allowed
	RequireAuth bool

	// AllowedAuthMethods is a list of allowed authentication methods
	// Empty list means all methods are allowed
	// Examples: ["unix", "kerberos", "ntlm"]
	AllowedAuthMethods []string

	// IdentityMapping defines how client identities are mapped
	// Used for squashing (mapping users to anonymous) and other transformations
	IdentityMapping *IdentityMapping
}
