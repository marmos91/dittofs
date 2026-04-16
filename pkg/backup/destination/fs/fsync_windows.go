//go:build windows

package fs

// fsyncDir is a no-op on Windows. Calling Sync on a directory handle
// returns ERROR_ACCESS_DENIED, and NTFS/ReFS rename is journaled — the
// Unix-style "fsync parent after rename to durably persist the new entry"
// ceremony is neither needed nor supported on Windows.
func fsyncDir(_ string) error { return nil }
