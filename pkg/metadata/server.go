package metadata

// ServerConfig contains server-wide configuration settings.
//
// This includes settings that apply across all shares and operations.
// The structure is intentionally flexible to accommodate protocol-specific
// settings without coupling the repository to specific protocols.
type ServerConfig struct {
	// CustomSettings allows protocol-specific configurations
	// Examples:
	//   - "nfs.mount.allowed_clients": []string{"192.168.1.0/24"}
	//   - "nfs.mount.denied_clients": []string{"192.168.1.50"}
	//   - "smb.signing_required": true
	//   - "ftp.passive_ports": "10000-10100"
	CustomSettings map[string]interface{}
}
