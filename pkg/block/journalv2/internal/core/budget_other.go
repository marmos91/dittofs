//go:build !unix

package core

// platformOpenFileLimit returns no limit: the platform reports no open-file
// limit to read, so the default bound is the fixed fallback.
func platformOpenFileLimit() (uint64, bool) { return 0, false }
