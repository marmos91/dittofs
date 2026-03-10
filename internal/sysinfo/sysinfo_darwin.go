//go:build darwin

package sysinfo

import "golang.org/x/sys/unix"

// availableMemory detects total physical memory on macOS via sysctl hw.memsize.
// Uses unix.SysctlUint64 (NOT syscall.Sysctl) to avoid 64-bit truncation
// on Apple Silicon (Go issue #21614).
func availableMemory() (uint64, string, error) {
	mem, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, "sysctl hw.memsize", err
	}
	return mem, "sysctl hw.memsize", nil
}
