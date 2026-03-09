//go:build !linux

package fs

import "os"

// dropPageCache is a no-op on non-Linux platforms (macOS, Windows).
func dropPageCache(_ *os.File) {}
