//go:build !linux

package fs

// detectFilesystemType is a best-effort stub on non-Linux platforms
// (D-14). macOS does not expose a stable /proc/mounts equivalent, and
// Windows is not a supported server platform. Returning "" falls through
// to "no warning emitted" in ValidateConfig — the remote-FS warning is
// a Linux-only diagnostic.
func detectFilesystemType(_ string) string { return "" }
