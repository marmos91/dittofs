package framework

import (
	"os"
	"path/filepath"
	"testing"
)

// TestContext holds the context for a test run
type TestContext struct {
	T          *testing.T
	Server     *TestServer
	Mount      *NFSMount
	MountPoint string
}

// NewTestContext creates a complete test context with server and mount
func NewTestContext(t *testing.T, storeType StoreType) *TestContext {
	t.Helper()

	ctx := &TestContext{
		T: t,
	}

	// Register cleanup immediately so it's available if anything fails
	t.Cleanup(func() {
		ctx.Cleanup()
	})

	// Create and start server
	server := NewTestServer(t, TestServerConfig{
		ContentStore: storeType,
		LogLevel:     "ERROR",
	})
	ctx.Server = server

	if err := server.Start(); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}

	// Create mount
	mount := NewNFSMount(t, MountConfig{
		ServerAddr: "localhost",
		ServerPort: server.Port(),
		ExportPath: server.ShareName(),
	})
	ctx.Mount = mount

	if err := mount.Mount(); err != nil {
		t.Fatalf("Failed to mount NFS: %v", err)
	}

	ctx.MountPoint = mount.MountPoint()
	return ctx
}

// Cleanup cleans up all resources
func (tc *TestContext) Cleanup() {
	tc.T.Helper()
	if tc.Mount != nil {
		tc.Mount.Cleanup()
	}
	if tc.Server != nil {
		tc.Server.Stop()
	}
}

// Path returns the full path within the mount point
func (tc *TestContext) Path(relativePath string) string {
	return filepath.Join(tc.MountPoint, relativePath)
}

// WriteFile writes content to a file in the mount
func (tc *TestContext) WriteFile(relativePath string, content []byte, mode os.FileMode) error {
	tc.T.Helper()
	path := tc.Path(relativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, content, mode)
}

// ReadFile reads content from a file in the mount
func (tc *TestContext) ReadFile(relativePath string) ([]byte, error) {
	tc.T.Helper()
	return os.ReadFile(tc.Path(relativePath))
}

// Mkdir creates a directory in the mount
func (tc *TestContext) Mkdir(relativePath string, mode os.FileMode) error {
	tc.T.Helper()
	return os.Mkdir(tc.Path(relativePath), mode)
}

// MkdirAll creates a directory and all parents in the mount
func (tc *TestContext) MkdirAll(relativePath string, mode os.FileMode) error {
	tc.T.Helper()
	return os.MkdirAll(tc.Path(relativePath), mode)
}

// Remove removes a file or empty directory in the mount
func (tc *TestContext) Remove(relativePath string) error {
	tc.T.Helper()
	return os.Remove(tc.Path(relativePath))
}

// RemoveAll removes a path and all children in the mount
func (tc *TestContext) RemoveAll(relativePath string) error {
	tc.T.Helper()
	return os.RemoveAll(tc.Path(relativePath))
}

// Stat returns file info for a path in the mount
func (tc *TestContext) Stat(relativePath string) (os.FileInfo, error) {
	tc.T.Helper()
	return os.Stat(tc.Path(relativePath))
}

// Lstat returns file info without following symlinks
func (tc *TestContext) Lstat(relativePath string) (os.FileInfo, error) {
	tc.T.Helper()
	return os.Lstat(tc.Path(relativePath))
}

// Symlink creates a symbolic link in the mount
func (tc *TestContext) Symlink(oldname, newname string) error {
	tc.T.Helper()
	return os.Symlink(oldname, tc.Path(newname))
}

// Readlink reads the target of a symbolic link
func (tc *TestContext) Readlink(relativePath string) (string, error) {
	tc.T.Helper()
	return os.Readlink(tc.Path(relativePath))
}

// Link creates a hard link in the mount
func (tc *TestContext) Link(oldname, newname string) error {
	tc.T.Helper()
	return os.Link(tc.Path(oldname), tc.Path(newname))
}

// Rename renames a file or directory in the mount
func (tc *TestContext) Rename(oldpath, newpath string) error {
	tc.T.Helper()
	return os.Rename(tc.Path(oldpath), tc.Path(newpath))
}

// ReadDir reads a directory in the mount
func (tc *TestContext) ReadDir(relativePath string) ([]os.DirEntry, error) {
	tc.T.Helper()
	return os.ReadDir(tc.Path(relativePath))
}

// FileExists checks if a file exists in the mount
func (tc *TestContext) FileExists(relativePath string) bool {
	tc.T.Helper()
	_, err := tc.Stat(relativePath)
	return err == nil
}

// AssertFileExists asserts that a file exists
func (tc *TestContext) AssertFileExists(relativePath string) {
	tc.T.Helper()
	if !tc.FileExists(relativePath) {
		tc.T.Fatalf("File does not exist: %s", relativePath)
	}
}

// AssertFileNotExists asserts that a file does not exist
func (tc *TestContext) AssertFileNotExists(relativePath string) {
	tc.T.Helper()
	if tc.FileExists(relativePath) {
		tc.T.Fatalf("File exists but should not: %s", relativePath)
	}
}

// AssertFileContent asserts that a file has expected content
func (tc *TestContext) AssertFileContent(relativePath string, expected []byte) {
	tc.T.Helper()
	actual, err := tc.ReadFile(relativePath)
	if err != nil {
		tc.T.Fatalf("Failed to read file %s: %v", relativePath, err)
	}
	if string(actual) != string(expected) {
		tc.T.Fatalf("File content mismatch for %s:\nExpected: %q\nGot: %q",
			relativePath, string(expected), string(actual))
	}
}

// AssertDirExists asserts that a directory exists
func (tc *TestContext) AssertDirExists(relativePath string) {
	tc.T.Helper()
	info, err := tc.Stat(relativePath)
	if err != nil {
		tc.T.Fatalf("Directory does not exist: %s", relativePath)
	}
	if !info.IsDir() {
		tc.T.Fatalf("Path exists but is not a directory: %s", relativePath)
	}
}

// AssertIsSymlink asserts that a path is a symbolic link
func (tc *TestContext) AssertIsSymlink(relativePath string) {
	tc.T.Helper()
	info, err := tc.Lstat(relativePath)
	if err != nil {
		tc.T.Fatalf("Path does not exist: %s", relativePath)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		tc.T.Fatalf("Path exists but is not a symlink: %s", relativePath)
	}
}
