//go:build !unix

package mdns

import "syscall"

// reusePort is a no-op on platforms without SO_REUSEPORT (e.g. Windows). The
// server does not run its protocol adapters there — this only keeps the package
// building for cross-platform release artifacts.
func reusePort(_, _ string, _ syscall.RawConn) error { return nil }
