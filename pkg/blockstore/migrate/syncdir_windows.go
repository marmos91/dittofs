//go:build windows

package migrate

// syncDir is a no-op on Windows. NTFS doesn't require parent-dir fsync
// for atomic-rename durability the way POSIX filesystems do, and
// opening a directory handle here held a lock that conflicted with the
// file truncate following in snapshotLocked (#498 Windows CI).
func syncDir(dir string) {}
