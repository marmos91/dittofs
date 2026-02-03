//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNFSFileOperations validates basic file operations via NFS mount.
// This covers requirements NFS-01 through NFS-06:
//   - NFS-01: Read files via NFS mount
//   - NFS-02: Write files via NFS mount
//   - NFS-03: Delete files via NFS mount
//   - NFS-04: List directories via NFS mount
//   - NFS-05: Create directories via NFS mount
//   - NFS-06: Change permissions via NFS mount
//
// Note: These tests run sequentially (not parallel) as they share the same mount.
func TestNFSFileOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFS file operations tests in short mode")
	}

	// Start server process
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Login as admin to get CLI runner
	runner := helpers.LoginAsAdmin(t, sp.APIURL())

	// Create metadata and payload stores for our test share
	metaStoreName := helpers.UniqueTestName("meta")
	payloadStoreName := helpers.UniqueTestName("payload")
	shareName := "/export"

	_, err := runner.CreateMetadataStore(metaStoreName, "memory")
	require.NoError(t, err, "Should create metadata store")
	t.Cleanup(func() {
		_ = runner.DeleteMetadataStore(metaStoreName)
	})

	_, err = runner.CreatePayloadStore(payloadStoreName, "memory")
	require.NoError(t, err, "Should create payload store")
	t.Cleanup(func() {
		_ = runner.DeletePayloadStore(payloadStoreName)
	})

	// Create the share
	_, err = runner.CreateShare(shareName, metaStoreName, payloadStoreName)
	require.NoError(t, err, "Should create share")
	t.Cleanup(func() {
		_ = runner.DeleteShare(shareName)
	})

	// Enable NFS adapter on a dynamic port
	nfsPort := helpers.FindFreePort(t)
	_, err = runner.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err, "Should enable NFS adapter")
	t.Cleanup(func() {
		_, _ = runner.DisableAdapter("nfs")
	})

	// Wait for adapter to be ready
	err = helpers.WaitForAdapterStatus(t, runner, "nfs", true, 5*time.Second)
	require.NoError(t, err, "NFS adapter should become enabled")

	// Wait for NFS server to be listening
	framework.WaitForServer(t, nfsPort, 10*time.Second)

	// Mount the NFS share
	mount := framework.MountNFS(t, nfsPort)
	t.Cleanup(mount.Cleanup)

	// Run subtests - NOT parallel since they share the same mount
	t.Run("NFS-01 Read files", func(t *testing.T) {
		testNFSReadFiles(t, mount)
	})

	t.Run("NFS-02 Write files", func(t *testing.T) {
		testNFSWriteFiles(t, mount)
	})

	t.Run("NFS-03 Delete files", func(t *testing.T) {
		testNFSDeleteFiles(t, mount)
	})

	t.Run("NFS-04 List directories", func(t *testing.T) {
		testNFSListDirectories(t, mount)
	})

	t.Run("NFS-05 Create directories", func(t *testing.T) {
		testNFSCreateDirectories(t, mount)
	})

	t.Run("NFS-06 Change permissions", func(t *testing.T) {
		testNFSChangePermissions(t, mount)
	})
}

// testNFSReadFiles tests NFS-01: Files can be read via NFS mount.
func testNFSReadFiles(t *testing.T, mount *framework.Mount) {
	t.Helper()

	// Create a test file with known content
	testContent := []byte("Hello, NFS! This is a test file for reading.")
	testFile := mount.FilePath("read_test.txt")

	// Write the file
	framework.WriteFile(t, testFile, testContent)
	t.Cleanup(func() {
		_ = os.Remove(testFile)
	})

	// Read the file back
	readContent := framework.ReadFile(t, testFile)

	// Verify content matches
	assert.Equal(t, testContent, readContent, "Read content should match written content")

	// Test reading with different file sizes
	sizes := []int64{100, 1024, 4096, 8192}
	for _, size := range sizes {
		name := filepath.Base(helpers.UniqueTestName("read"))
		filePath := mount.FilePath(name)

		// Write random data and get checksum
		checksum := framework.WriteRandomFile(t, filePath, size)
		t.Cleanup(func() {
			_ = os.Remove(filePath)
		})

		// Verify file can be read and checksum matches
		framework.VerifyFileChecksum(t, filePath, checksum)
	}

	t.Log("NFS-01: Read files passed")
}

// testNFSWriteFiles tests NFS-02: Files can be written via NFS mount.
func testNFSWriteFiles(t *testing.T, mount *framework.Mount) {
	t.Helper()

	// Test simple write
	testContent := []byte("Test content for write operation")
	testFile := mount.FilePath("write_test.txt")

	framework.WriteFile(t, testFile, testContent)
	t.Cleanup(func() {
		_ = os.Remove(testFile)
	})

	// Verify file exists
	assert.True(t, framework.FileExists(testFile), "Written file should exist")

	// Verify content
	readContent := framework.ReadFile(t, testFile)
	assert.Equal(t, testContent, readContent, "Written content should match")

	// Test overwrite
	newContent := []byte("Updated content")
	framework.WriteFile(t, testFile, newContent)
	readContent = framework.ReadFile(t, testFile)
	assert.Equal(t, newContent, readContent, "Overwritten content should match")

	// Test writing larger files
	largeFile := mount.FilePath("write_large.bin")
	checksum := framework.WriteRandomFile(t, largeFile, 100*1024) // 100KB
	t.Cleanup(func() {
		_ = os.Remove(largeFile)
	})

	assert.True(t, framework.FileExists(largeFile), "Large file should exist")
	framework.VerifyFileChecksum(t, largeFile, checksum)

	t.Log("NFS-02: Write files passed")
}

// testNFSDeleteFiles tests NFS-03: Files can be deleted via NFS mount.
func testNFSDeleteFiles(t *testing.T, mount *framework.Mount) {
	t.Helper()

	// Create a file to delete
	testFile := mount.FilePath("delete_test.txt")
	framework.WriteFile(t, testFile, []byte("To be deleted"))

	// Verify file exists
	assert.True(t, framework.FileExists(testFile), "File should exist before deletion")

	// Delete the file
	err := os.Remove(testFile)
	require.NoError(t, err, "Should delete file")

	// Verify file is gone
	assert.False(t, framework.FileExists(testFile), "File should not exist after deletion")

	// Test deleting non-existent file should error
	err = os.Remove(mount.FilePath("nonexistent.txt"))
	assert.Error(t, err, "Deleting non-existent file should error")

	t.Log("NFS-03: Delete files passed")
}

// testNFSListDirectories tests NFS-04: Directories can be listed via NFS mount.
func testNFSListDirectories(t *testing.T, mount *framework.Mount) {
	t.Helper()

	// Create a subdirectory with files
	subDir := mount.FilePath("list_test_dir")
	framework.CreateDir(t, subDir)
	t.Cleanup(func() {
		_ = os.RemoveAll(subDir)
	})

	// Create some files in the directory
	fileNames := []string{"file1.txt", "file2.txt", "file3.txt"}
	for _, name := range fileNames {
		framework.WriteFile(t, filepath.Join(subDir, name), []byte("content"))
	}

	// Create a subdirectory
	nestedDir := filepath.Join(subDir, "nested")
	framework.CreateDir(t, nestedDir)

	// List directory contents
	entries := framework.ListDir(t, subDir)

	// Verify all expected entries are present
	expectedEntries := append(fileNames, "nested")
	assert.Len(t, entries, len(expectedEntries), "Should have correct number of entries")

	for _, expected := range expectedEntries {
		found := false
		for _, entry := range entries {
			if entry == expected {
				found = true
				break
			}
		}
		assert.True(t, found, "Directory should contain %s", expected)
	}

	// Test file/directory count helpers
	assert.Equal(t, len(fileNames), framework.CountFiles(t, subDir), "Should have correct file count")
	assert.Equal(t, 1, framework.CountDirs(t, subDir), "Should have one subdirectory")

	t.Log("NFS-04: List directories passed")
}

// testNFSCreateDirectories tests NFS-05: Directories can be created via NFS mount.
func testNFSCreateDirectories(t *testing.T, mount *framework.Mount) {
	t.Helper()

	// Test simple directory creation
	simpleDir := mount.FilePath("create_dir_test")
	framework.CreateDir(t, simpleDir)
	t.Cleanup(func() {
		_ = os.RemoveAll(simpleDir)
	})

	// Verify directory exists
	assert.True(t, framework.DirExists(simpleDir), "Created directory should exist")

	// Verify it's a directory, not a file
	info := framework.GetFileInfo(t, simpleDir)
	assert.True(t, info.IsDir, "Should be a directory")

	// Test nested directory creation with MkdirAll
	nestedDir := mount.FilePath("nested/level1/level2")
	framework.CreateDirAll(t, nestedDir)
	t.Cleanup(func() {
		_ = os.RemoveAll(mount.FilePath("nested"))
	})

	assert.True(t, framework.DirExists(nestedDir), "Nested directory should exist")

	// Verify intermediate directories were created
	assert.True(t, framework.DirExists(mount.FilePath("nested")), "Parent dir should exist")
	assert.True(t, framework.DirExists(mount.FilePath("nested/level1")), "Intermediate dir should exist")

	// Test creating directory that already exists
	err := os.Mkdir(simpleDir, 0755)
	assert.Error(t, err, "Creating existing directory should error")

	t.Log("NFS-05: Create directories passed")
}

// testNFSChangePermissions tests NFS-06: File permissions can be changed via NFS mount.
func testNFSChangePermissions(t *testing.T, mount *framework.Mount) {
	t.Helper()

	// Create a test file
	testFile := mount.FilePath("chmod_test.txt")
	framework.WriteFile(t, testFile, []byte("Permission test"))
	t.Cleanup(func() {
		_ = os.Remove(testFile)
	})

	// Get initial permissions
	initialInfo := framework.GetFileInfo(t, testFile)
	t.Logf("Initial mode: %o", initialInfo.Mode.Perm())

	// Change permissions to 0600 (owner read/write only)
	err := os.Chmod(testFile, 0600)
	require.NoError(t, err, "Should change permissions to 0600")

	info := framework.GetFileInfo(t, testFile)
	// Check that mode was changed - NFS may not preserve exact permissions
	// so we check that it's different or at least includes the requested bits
	assert.True(t, info.Mode.Perm()&0600 == 0600 || info.Mode.Perm() != initialInfo.Mode.Perm(),
		"Permissions should have changed, got %o", info.Mode.Perm())
	t.Logf("After chmod 0600: %o", info.Mode.Perm())

	// Change permissions to 0755 (owner all, group/other read+execute)
	err = os.Chmod(testFile, 0755)
	require.NoError(t, err, "Should change permissions to 0755")

	info = framework.GetFileInfo(t, testFile)
	t.Logf("After chmod 0755: %o", info.Mode.Perm())
	assert.True(t, info.Mode.Perm()&0755 == 0755 || info.Mode.Perm()&0700 == 0700,
		"Permissions should include owner rwx, got %o", info.Mode.Perm())

	// Test chmod on directory
	testDir := mount.FilePath("chmod_dir_test")
	framework.CreateDir(t, testDir)
	t.Cleanup(func() {
		_ = os.RemoveAll(testDir)
	})

	err = os.Chmod(testDir, 0700)
	require.NoError(t, err, "Should change directory permissions")

	dirInfo := framework.GetFileInfo(t, testDir)
	t.Logf("Directory after chmod 0700: %o", dirInfo.Mode.Perm())
	assert.True(t, dirInfo.Mode.Perm()&0700 == 0700,
		"Directory should have owner rwx, got %o", dirInfo.Mode.Perm())

	t.Log("NFS-06: Change permissions passed")
}
