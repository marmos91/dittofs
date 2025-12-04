//go:build !windows

package logger

import (
	"syscall"
	"unsafe"
)

// isTerminal checks if the file descriptor is a terminal on Unix systems
func isTerminal(fd uintptr) bool {
	var termios syscall.Termios
	_, _, err := syscall.Syscall6(
		syscall.SYS_IOCTL,
		fd,
		syscall.TIOCGETA, // macOS uses TIOCGETA, Linux uses TCGETS
		uintptr(unsafe.Pointer(&termios)),
		0, 0, 0,
	)
	return err == 0
}
