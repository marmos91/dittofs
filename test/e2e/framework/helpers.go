//go:build e2e

package framework

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// IsLargeFile returns true if the file size is considered "large" (>5MB).
func IsLargeFile(sizeBytes int64) bool {
	return sizeBytes > 5*1024*1024
}

// SkipIfShort skips the test if running with -short flag.
func SkipIfShort(t *testing.T, reason string) {
	t.Helper()
	if testing.Short() {
		t.Skipf("Skipping in short mode: %s", reason)
	}
}

// GenerateRandomData generates random data of the specified size.
func GenerateRandomData(t *testing.T, size int64) []byte {
	t.Helper()

	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("Failed to generate random data: %v", err)
	}
	return data
}

// WriteRandomFile creates a file with random content and returns its checksum.
func WriteRandomFile(t *testing.T, path string, size int64) string {
	t.Helper()

	data := GenerateRandomData(t, size)

	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}

	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// VerifyFileChecksum verifies that a file has the expected checksum.
func VerifyFileChecksum(t *testing.T, path string, expectedChecksum string) {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Failed to open file: %v", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	actualChecksum := hex.EncodeToString(h.Sum(nil))
	if actualChecksum != expectedChecksum {
		t.Errorf("Checksum mismatch: expected %s, got %s", expectedChecksum, actualChecksum)
	}
}

// FileExists checks if a file exists.
func FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// DirExists checks if a directory exists.
func DirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// ReadFile reads and returns the contents of a file.
func ReadFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read file %s: %v", path, err)
	}
	return data
}

// WriteFile writes data to a file.
func WriteFile(t *testing.T, path string, data []byte) {
	t.Helper()

	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("Failed to write file %s: %v", path, err)
	}
}

// CreateDir creates a directory with mode 0777.
// Note: os.Mkdir is subject to umask, so we explicitly chmod after creation
// to ensure the full permissions are set for cross-protocol access.
func CreateDir(t *testing.T, path string) {
	t.Helper()

	if err := os.Mkdir(path, 0777); err != nil {
		t.Fatalf("Failed to create directory %s: %v", path, err)
	}

	// Explicitly set mode to 0777 to override umask effects
	// This is necessary for NFS/SMB cross-protocol access
	if err := os.Chmod(path, 0777); err != nil {
		t.Fatalf("Failed to chmod directory %s: %v", path, err)
	}
}

// CreateDirAll creates a directory and all parents.
func CreateDirAll(t *testing.T, path string) {
	t.Helper()

	// Use 0777 for test directories to allow cross-protocol access (NFS/SMB)
	if err := os.MkdirAll(path, 0777); err != nil {
		t.Fatalf("Failed to create directory %s: %v", path, err)
	}
}

// RemoveAll removes a file or directory.
func RemoveAll(t *testing.T, path string) {
	t.Helper()

	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("Failed to remove %s: %v", path, err)
	}
}

// FileInfo contains file metadata for assertions.
type FileInfo struct {
	Size    int64
	Mode    os.FileMode
	IsDir   bool
	ModTime int64 // Unix timestamp
}

// GetFileInfo returns file metadata.
func GetFileInfo(t *testing.T, path string) FileInfo {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Failed to stat file %s: %v", path, err)
	}

	return FileInfo{
		Size:    info.Size(),
		Mode:    info.Mode(),
		IsDir:   info.IsDir(),
		ModTime: info.ModTime().Unix(),
	}
}

// ListDir returns the names of files in a directory.
func ListDir(t *testing.T, path string) []string {
	t.Helper()

	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("Failed to read directory %s: %v", path, err)
	}

	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	return names
}

// CountFiles returns the number of files in a directory.
func CountFiles(t *testing.T, path string) int {
	t.Helper()

	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("Failed to read directory %s: %v", path, err)
	}

	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			count++
		}
	}
	return count
}

// CountDirs returns the number of subdirectories in a directory.
func CountDirs(t *testing.T, path string) int {
	t.Helper()

	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("Failed to read directory %s: %v", path, err)
	}

	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			count++
		}
	}
	return count
}

// SkipIfDarwin skips the test on macOS with an explanatory message.
// NFSv4 feature tests require Linux because macOS NFSv4 client has known
// limitations and reliability issues.
func SkipIfDarwin(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("Skipping: NFSv4 feature tests require Linux")
	}
}

// SkipIfNoNFS4ACLTools skips the test if nfs4_setfacl and nfs4_getfacl
// are not found in PATH. These tools are provided by nfs4-acl-tools and
// are required for NFSv4 ACL manipulation tests.
func SkipIfNoNFS4ACLTools(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("nfs4_setfacl"); err != nil {
		t.Skip("Skipping: nfs4_setfacl not found in PATH (install nfs4-acl-tools)")
	}
	if _, err := exec.LookPath("nfs4_getfacl"); err != nil {
		t.Skip("Skipping: nfs4_getfacl not found in PATH (install nfs4-acl-tools)")
	}
}

// SkipIfNFSv4Unsupported skips the test on platforms where NFSv4 mount
// is not reliable. On macOS (Darwin), the NFSv4 client has known issues
// with pseudo-filesystem browsing, delegations, and stateful operations.
func SkipIfNFSv4Unsupported(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("Skipping: NFSv4 mount is unreliable on macOS (Darwin NFSv4 client known issues)")
	}
}
