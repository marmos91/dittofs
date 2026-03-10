//go:build !linux && !darwin && !windows

package sysinfo

import "errors"

func availableMemory() (uint64, string, error) {
	return defaultMemory, "fallback (unsupported platform)", errors.New("memory detection not supported on this platform")
}
