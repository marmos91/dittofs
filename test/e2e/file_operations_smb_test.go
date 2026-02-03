//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSMBFileOperations validates basic file operations over SMB protocol.
// This covers requirements SMB-01 through SMB-06:
//   - SMB-01: Read files via SMB mount
//   - SMB-02: Write files via SMB mount
//   - SMB-03: Delete files via SMB mount
//   - SMB-04: List directories via SMB mount
//   - SMB-05: Create directories via SMB mount
//   - SMB-06: Change file permissions via SMB mount
//
// Note: SMB requires authenticated user (unlike NFS AUTH_UNIX).
// Tests create a dedicated user with read-write permission on the share.
func TestSMBFileOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping SMB file operations tests in short mode")
	}

	// Start a server for all SMB tests
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Login as admin to configure the server
	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	// Create stores for the share
	metaStoreName := helpers.UniqueTestName("meta")
	payloadStoreName := helpers.UniqueTestName("payload")

	_, err := cli.CreateMetadataStore(metaStoreName, "memory")
	require.NoError(t, err, "Should create metadata store")

	_, err = cli.CreatePayloadStore(payloadStoreName, "memory")
	require.NoError(t, err, "Should create payload store")

	// Create share with read-write default permission
	shareName := "/export"
	_, err = cli.CreateShare(shareName, metaStoreName, payloadStoreName,
		helpers.WithShareDefaultPermission("read-write"))
	require.NoError(t, err, "Should create share")

	// Create a test user for SMB authentication
	// Password must be 8+ characters for SMB authentication
	testUsername := helpers.UniqueTestName("smbuser")
	testPassword := "testpass123"

	_, err = cli.CreateUser(testUsername, testPassword)
	require.NoError(t, err, "Should create test user")

	// Grant user read-write permission on the share
	err = cli.GrantUserPermission(shareName, testUsername, "read-write")
	require.NoError(t, err, "Should grant user permission")

	// Enable SMB adapter on a dynamic port
	smbPort := helpers.FindFreePort(t)
	_, err = cli.EnableAdapter("smb", helpers.WithAdapterPort(smbPort))
	require.NoError(t, err, "Should enable SMB adapter")

	// Wait for adapter to be fully enabled
	err = helpers.WaitForAdapterStatus(t, cli, "smb", true, 5*time.Second)
	require.NoError(t, err, "SMB adapter should become enabled")

	// Wait for SMB server to be listening
	framework.WaitForServer(t, smbPort, 10*time.Second)

	// Mount SMB share with credentials
	creds := framework.SMBCredentials{
		Username: testUsername,
		Password: testPassword,
	}
	mount := framework.MountSMB(t, smbPort, creds)
	t.Cleanup(mount.Cleanup)

	// Run subtests for each requirement
	t.Run("SMB-01 Read files", func(t *testing.T) {
		testSMBReadFiles(t, mount)
	})

	t.Run("SMB-02 Write files", func(t *testing.T) {
		testSMBWriteFiles(t, mount)
	})

	t.Run("SMB-03 Delete files", func(t *testing.T) {
		testSMBDeleteFiles(t, mount)
	})

	t.Run("SMB-04 List directories", func(t *testing.T) {
		testSMBListDirectories(t, mount)
	})

	t.Run("SMB-05 Create directories", func(t *testing.T) {
		testSMBCreateDirectories(t, mount)
	})

	t.Run("SMB-06 Change permissions", func(t *testing.T) {
		testSMBChangePermissions(t, mount)
	})
}

// testSMBReadFiles tests SMB-01: Files can be read via SMB mount.
func testSMBReadFiles(t *testing.T, mount *framework.Mount) {
	t.Helper()

	// Create a file with known content
	testContent := []byte("Hello, SMB World! This is test content for reading.")
	testFile := mount.FilePath("read_test.txt")

	framework.WriteFile(t, testFile, testContent)

	// Read the file back
	readContent := framework.ReadFile(t, testFile)
	assert.Equal(t, testContent, readContent, "Read content should match written content")

	// Test reading a larger file
	largeContent := framework.GenerateRandomData(t, 1024*100) // 100KB
	largeFile := mount.FilePath("read_large_test.bin")

	framework.WriteFile(t, largeFile, largeContent)
	readLarge := framework.ReadFile(t, largeFile)
	assert.Equal(t, largeContent, readLarge, "Large file content should match")

	// Cleanup
	_ = os.Remove(testFile)
	_ = os.Remove(largeFile)

	t.Log("SMB-01: Read files test passed")
}

// testSMBWriteFiles tests SMB-02: Files can be written via SMB mount.
func testSMBWriteFiles(t *testing.T, mount *framework.Mount) {
	t.Helper()

	// Test 1: Write a simple file
	testContent := []byte("This is a test file written via SMB protocol.")
	testFile := mount.FilePath("write_test.txt")

	framework.WriteFile(t, testFile, testContent)
	assert.True(t, framework.FileExists(testFile), "Written file should exist")

	// Verify content
	readContent := framework.ReadFile(t, testFile)
	assert.Equal(t, testContent, readContent, "File content should match what was written")

	// Test 2: Overwrite existing file
	newContent := []byte("This is updated content.")
	framework.WriteFile(t, testFile, newContent)
	readContent = framework.ReadFile(t, testFile)
	assert.Equal(t, newContent, readContent, "Overwritten content should match")

	// Test 3: Write file with random data and verify checksum
	checksumFile := mount.FilePath("write_checksum_test.bin")
	checksum := framework.WriteRandomFile(t, checksumFile, 1024*50) // 50KB
	framework.VerifyFileChecksum(t, checksumFile, checksum)

	// Cleanup
	_ = os.Remove(testFile)
	_ = os.Remove(checksumFile)

	t.Log("SMB-02: Write files test passed")
}

// testSMBDeleteFiles tests SMB-03: Files can be deleted via SMB mount.
func testSMBDeleteFiles(t *testing.T, mount *framework.Mount) {
	t.Helper()

	// Create a file to delete
	testFile := mount.FilePath("delete_test.txt")
	framework.WriteFile(t, testFile, []byte("File to be deleted"))
	require.True(t, framework.FileExists(testFile), "File should exist before deletion")

	// Delete the file
	err := os.Remove(testFile)
	require.NoError(t, err, "Should delete file without error")

	// Verify file is gone
	assert.False(t, framework.FileExists(testFile), "File should not exist after deletion")

	// Test deleting a file that doesn't exist
	err = os.Remove(mount.FilePath("nonexistent.txt"))
	assert.Error(t, err, "Deleting nonexistent file should return error")

	t.Log("SMB-03: Delete files test passed")
}

// testSMBListDirectories tests SMB-04: Directories can be listed via SMB mount.
func testSMBListDirectories(t *testing.T, mount *framework.Mount) {
	t.Helper()

	// Create a directory with files for listing
	listDir := mount.FilePath("list_test_dir")
	framework.CreateDir(t, listDir)

	// Create some files in the directory
	expectedFiles := []string{"file1.txt", "file2.txt", "file3.txt"}
	for _, name := range expectedFiles {
		filePath := filepath.Join(listDir, name)
		framework.WriteFile(t, filePath, []byte("content"))
	}

	// Create a subdirectory
	subDir := filepath.Join(listDir, "subdir")
	framework.CreateDir(t, subDir)

	// List the directory
	entries := framework.ListDir(t, listDir)

	// Verify expected entries are present
	assert.Contains(t, entries, "file1.txt", "Should list file1.txt")
	assert.Contains(t, entries, "file2.txt", "Should list file2.txt")
	assert.Contains(t, entries, "file3.txt", "Should list file3.txt")
	assert.Contains(t, entries, "subdir", "Should list subdir")

	// Verify counts
	assert.Equal(t, 3, framework.CountFiles(t, listDir), "Should count 3 files")
	assert.Equal(t, 1, framework.CountDirs(t, listDir), "Should count 1 subdirectory")

	// Cleanup
	_ = os.RemoveAll(listDir)

	t.Log("SMB-04: List directories test passed")
}

// testSMBCreateDirectories tests SMB-05: Directories can be created via SMB mount.
func testSMBCreateDirectories(t *testing.T, mount *framework.Mount) {
	t.Helper()

	// Test 1: Create a simple directory
	simpleDir := mount.FilePath("create_dir_test")
	framework.CreateDir(t, simpleDir)
	assert.True(t, framework.DirExists(simpleDir), "Created directory should exist")

	// Test 2: Create nested directories
	nestedDir := mount.FilePath("nested/path/structure")
	framework.CreateDirAll(t, nestedDir)
	assert.True(t, framework.DirExists(nestedDir), "Nested directory should exist")

	// Verify parent directories were also created
	assert.True(t, framework.DirExists(mount.FilePath("nested")), "Parent dir 'nested' should exist")
	assert.True(t, framework.DirExists(mount.FilePath("nested/path")), "Parent dir 'nested/path' should exist")

	// Test 3: Creating existing directory should work (idempotent for MkdirAll)
	err := os.MkdirAll(nestedDir, 0755)
	assert.NoError(t, err, "Creating existing directory with MkdirAll should succeed")

	// Test 4: Mkdir on existing directory should fail
	err = os.Mkdir(simpleDir, 0755)
	assert.Error(t, err, "Mkdir on existing directory should fail")

	// Cleanup
	_ = os.RemoveAll(simpleDir)
	_ = os.RemoveAll(mount.FilePath("nested"))

	t.Log("SMB-05: Create directories test passed")
}

// testSMBChangePermissions tests SMB-06: File permissions can be changed via SMB mount.
func testSMBChangePermissions(t *testing.T, mount *framework.Mount) {
	t.Helper()

	// Create a file with default permissions
	testFile := mount.FilePath("chmod_test.txt")
	framework.WriteFile(t, testFile, []byte("Permission test file"))

	// Get initial permissions
	initialInfo := framework.GetFileInfo(t, testFile)
	t.Logf("Initial file mode: %v", initialInfo.Mode)

	// Change permissions to read-only (0444)
	err := os.Chmod(testFile, 0444)
	require.NoError(t, err, "Should change file permissions")

	// Verify permissions changed
	newInfo := framework.GetFileInfo(t, testFile)
	// Note: macOS doesn't support chmod on SMB mounts - the OS silently ignores the operation.
	// On Linux with CIFS, chmod may work depending on mount options.
	// We only assert mode change on non-darwin platforms.
	if runtime.GOOS != "darwin" {
		assert.NotEqual(t, initialInfo.Mode, newInfo.Mode, "File mode should have changed")
	} else {
		t.Log("Skipping mode change assertion on macOS (chmod not supported on SMB mounts)")
	}
	t.Logf("New file mode: %v", newInfo.Mode)

	// Change permissions to read-write (0644)
	err = os.Chmod(testFile, 0644)
	require.NoError(t, err, "Should change file permissions back")

	// Test directory permissions
	testDir := mount.FilePath("chmod_dir_test")
	framework.CreateDir(t, testDir)

	// Change directory permissions
	err = os.Chmod(testDir, 0755)
	require.NoError(t, err, "Should change directory permissions")

	dirInfo := framework.GetFileInfo(t, testDir)
	t.Logf("Directory mode: %v", dirInfo.Mode)

	// Cleanup - need to ensure we have write permission first
	_ = os.Chmod(testFile, 0644)
	_ = os.Remove(testFile)
	_ = os.RemoveAll(testDir)

	t.Log("SMB-06: Change permissions test passed")
}
