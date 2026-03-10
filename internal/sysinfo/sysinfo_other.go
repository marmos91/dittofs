//go:build !linux && !darwin && !windows

package sysinfo

func availableMemory() (uint64, string, error) {
	return defaultMemory, "fallback (unsupported platform)", nil
}
