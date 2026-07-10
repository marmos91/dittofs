//go:build unix

package mdns

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// reusePort sets SO_REUSEADDR + SO_REUSEPORT on the socket before bind so the
// responder can share UDP 5353 with the OS mDNS stack (macOS mDNSResponder /
// Linux Avahi) and still receive inbound multicast. SO_REUSEADDR alone binds on
// Linux but, on macOS, the kernel then delivers no inbound multicast to us.
func reusePort(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
			serr = err
			return
		}
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	}); err != nil {
		return err
	}
	return serr
}
