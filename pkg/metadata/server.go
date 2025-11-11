package metadata

// ServerConfig contains server-wide configuration that applies to all exports
type ServerConfig struct {
	// DumpAccessControl restricts who can call the DUMP procedure
	// If empty, all clients can call DUMP (RFC 1813 default behavior)
	DumpAllowedClients []string // IP addresses or CIDR ranges
	DumpDeniedClients  []string // IP addresses or CIDR ranges
}
