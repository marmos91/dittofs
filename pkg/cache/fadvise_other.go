//go:build !linux

package cache

import "os"

// dropPageCache is a no-op on non-Linux platforms (macOS, Windows).
func dropPageCache(_ *os.File) {}
