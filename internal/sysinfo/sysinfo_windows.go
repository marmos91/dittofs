//go:build windows

package sysinfo

import (
	"syscall"
	"unsafe"
)

// memoryStatusEx matches the Win32 MEMORYSTATUSEX struct layout.
// See: https://learn.microsoft.com/en-us/windows/win32/api/sysinfoapi/ns-sysinfoapi-memorystatusex
type memoryStatusEx struct {
	dwLength                uint32
	dwMemoryLoad            uint32
	ullTotalPhys            uint64
	ullAvailPhys            uint64
	ullTotalPageFile        uint64
	ullAvailPageFile        uint64
	ullTotalVirtual         uint64
	ullAvailVirtual         uint64
	ullAvailExtendedVirtual uint64
}

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

// availableMemory detects total physical memory on Windows via GlobalMemoryStatusEx.
func availableMemory() (uint64, string, error) {
	var ms memoryStatusEx
	ms.dwLength = uint32(unsafe.Sizeof(ms))

	r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms)))
	if r == 0 {
		return 0, "GlobalMemoryStatusEx", err
	}
	return ms.ullTotalPhys, "GlobalMemoryStatusEx", nil
}
