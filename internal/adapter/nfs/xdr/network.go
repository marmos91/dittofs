package xdr

import "net"

// ExtractClientIP extracts the IP address from a client address string.
//
// Expected format: "IP:port" (e.g., "192.168.1.100:45678")
// Falls back to returning the original string if parsing fails.
//
// This is used for audit logging and access control decisions.
//
// Parameters:
//   - clientAddr: Client address string from network connection
//
// Returns:
//   - string: IP address portion, or original string if parsing fails
func ExtractClientIP(clientAddr string) string {
	if clientAddr == "" {
		return "unknown"
	}

	ip, _, err := net.SplitHostPort(clientAddr)
	if err != nil {
		// Parsing failed: might be just IP without port, or invalid format
		// Return as-is rather than failing
		return clientAddr
	}
	return ip
}
