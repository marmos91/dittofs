package xdr

import (
	"log/slog"
	"net"
)

// LazyClientIP wraps a raw "host:port" client address and defers the
// net.SplitHostPort extraction to log-emit time via slog.LogValuer. NFS
// handlers build one per op, but most ops never emit at their log level, so
// the split is skipped entirely on the hot path. It formats identically to
// ExtractClientIP in both text and JSON output.
type LazyClientIP string

// LogValue implements slog.LogValuer so the IP is resolved only when a record
// is actually emitted (skipped for filtered-out levels).
func (c LazyClientIP) LogValue() slog.Value {
	return slog.StringValue(ExtractClientIP(string(c)))
}

// String extracts the IP eagerly, for the rare non-logging callers (error
// paths passing the client into MapStoreErrorToNFSStatus).
func (c LazyClientIP) String() string {
	return ExtractClientIP(string(c))
}

// ExtractClientIP extracts the IP address from a client address string.
//
// Expected format: "IP:port" (e.g., "192.168.1.100:45678"). Falls back to
// returning the original string if parsing fails. Prefer LazyClientIP on hot
// per-op paths where the result only feeds logging.
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
