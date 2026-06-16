// Package netutil holds small, dependency-free networking helpers shared across
// the control plane and protocol adapters.
package netutil

import "net"

// IsNonLoopbackHost reports whether host binds a listener to something other
// than loopback. An empty host is treated as loopback (callers default an empty
// bind address to 127.0.0.1 elsewhere). A wildcard bind (0.0.0.0 / ::) reaches
// off-host and counts as non-loopback. A named host that does not parse as an
// IP (e.g. a hostname) is conservatively treated as non-loopback so cleartext
// warnings are not silently skipped.
func IsNonLoopbackHost(host string) bool {
	switch host {
	case "", "127.0.0.1", "::1", "[::1]", "localhost":
		return false
	}
	// Strip brackets from an IPv6 literal like "[::]".
	h := host
	if len(h) >= 2 && h[0] == '[' && h[len(h)-1] == ']' {
		h = h[1 : len(h)-1]
	}
	if ip := net.ParseIP(h); ip != nil {
		return !ip.IsLoopback()
	}
	// Non-IP host (hostname): assume it resolves off-host.
	return true
}
