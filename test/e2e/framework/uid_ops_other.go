//go:build e2e && !linux

package framework

import (
	"errors"
	"os"
	"testing"
)

// uid_ops_other.go provides compile-only stubs for the uid-scoped operations.
// Dropping privileges via SysProcAttr.Credential and the coreutils semantics
// these helpers rely on are Linux-specific, and the e2e permission suites that
// use them are gated to Linux. These stubs exist so `go build -tags e2e`
// succeeds on other platforms (e.g. developer macOS machines).

var errUIDOpsUnsupported = errors.New("uid-scoped filesystem ops are only supported on linux")

// ExecAsUID is unsupported off Linux.
func ExecAsUID(t *testing.T, uid, gid uint32, name string, args ...string) ([]byte, error) {
	t.Helper()
	return nil, errUIDOpsUnsupported
}

// WriteFileAsUID is unsupported off Linux.
func WriteFileAsUID(t *testing.T, uid, gid uint32, path string, data []byte) error {
	t.Helper()
	return errUIDOpsUnsupported
}

// ReadFileAsUID is unsupported off Linux.
func ReadFileAsUID(t *testing.T, uid, gid uint32, path string) ([]byte, error) {
	t.Helper()
	return nil, errUIDOpsUnsupported
}

// WriteRandomFileAsUID is unsupported off Linux.
func WriteRandomFileAsUID(t *testing.T, uid, gid uint32, path string, size int64) (string, error) {
	t.Helper()
	return "", errUIDOpsUnsupported
}

// VerifyFileChecksumAsUID is unsupported off Linux.
func VerifyFileChecksumAsUID(t *testing.T, uid, gid uint32, path, want string) error {
	t.Helper()
	return errUIDOpsUnsupported
}

// MkdirAsUID is unsupported off Linux.
func MkdirAsUID(t *testing.T, uid, gid uint32, path string) error {
	t.Helper()
	return errUIDOpsUnsupported
}

// MkdirAllAsUID is unsupported off Linux.
func MkdirAllAsUID(t *testing.T, uid, gid uint32, path string) error {
	t.Helper()
	return errUIDOpsUnsupported
}

// RemoveAsUID is unsupported off Linux.
func RemoveAsUID(t *testing.T, uid, gid uint32, path string) error {
	t.Helper()
	return errUIDOpsUnsupported
}

// RemoveAllAsUID is unsupported off Linux.
func RemoveAllAsUID(t *testing.T, uid, gid uint32, path string) error {
	t.Helper()
	return errUIDOpsUnsupported
}

// ChmodAsUID is unsupported off Linux.
func ChmodAsUID(t *testing.T, uid, gid uint32, mode os.FileMode, path string) error {
	t.Helper()
	return errUIDOpsUnsupported
}

// FileExistsAsUID is unsupported off Linux.
func FileExistsAsUID(t *testing.T, uid, gid uint32, path string) bool {
	t.Helper()
	return false
}

// ListDirAsUID is unsupported off Linux.
func ListDirAsUID(t *testing.T, uid, gid uint32, path string) ([]string, error) {
	t.Helper()
	return nil, errUIDOpsUnsupported
}

// StatAsUID is unsupported off Linux.
func StatAsUID(t *testing.T, uid, gid uint32, path string) (FileInfo, error) {
	t.Helper()
	return FileInfo{}, errUIDOpsUnsupported
}
