//go:build windows

package bench

import "os"

// disableCache is a no-op on Windows — cache bypass is not supported.
func disableCache(_ *os.File) {}
