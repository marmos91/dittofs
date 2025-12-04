//go:build linux

package logger

import (
	"syscall"
	"unsafe"
)

// TCGETS is the ioctl number for getting terminal attributes on Linux
const TCGETS = 0x5401

// isTerminal checks if the file descriptor is a terminal on Linux
func isTerminal(fd uintptr) bool {
	var termios syscall.Termios
	_, _, err := syscall.Syscall6(
		syscall.SYS_IOCTL,
		fd,
		TCGETS, // Linux uses TCGETS
		uintptr(unsafe.Pointer(&termios)),
		0, 0, 0,
	)
	return err == 0
}
