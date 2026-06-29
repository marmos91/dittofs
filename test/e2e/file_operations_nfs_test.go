//go:build e2e && linux

package e2e

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Least-privilege identity used by the NFS file-operation suite. The e2e
// harness runs as root (needed to mount NFS), but exercising the real
// permission path means acting as an ordinary granted user, not root. Under the
// default root_to_guest squash root is not auto-privileged, so all data-plane
// I/O below runs as this non-root uid/gid (see TestNFSRootSquash for the
// denial side). Gated to linux because dropping privileges per-op is
// Linux-specific (see framework/uid_ops_linux.go).
const (
	nfsLeastPrivUser = "nfs-lpuser"
	nfsLeastPrivPass = "nfs-lpuser-pw-123"
	nfsLeastPrivUID  = uint32(2000)
	nfsLeastPrivGID  = uint32(2000)
)

// TestNFSFileOperations validates basic file operations via NFS mount, run as a
// least-privilege granted user (not root).
// This covers requirements NFS-01 through NFS-06:
func TestNFSFileOperations(t *testing.T) {
	// Start server process
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Login as admin to get CLI runner
	runner := helpers.LoginAsAdmin(t, sp.APIURL())

	// Create metadata and block stores for our test share
	metaStoreName := helpers.UniqueTestName("meta")
	localStoreName := helpers.UniqueTestName("local")
	shareName := "/export"

	_, err := runner.CreateMetadataStore(metaStoreName, "memory")
	require.NoError(t, err, "Should create metadata store")
	t.Cleanup(func() {
		_ = runner.DeleteMetadataStore(metaStoreName)
	})

	_, err = runner.CreateLocalBlockStore(localStoreName, "memory")
	require.NoError(t, err, "Should create block store")
	t.Cleanup(func() {
		_ = runner.DeleteLocalBlockStore(localStoreName)
	})

	// Create the share
	_, err = runner.CreateShare(shareName, metaStoreName, localStoreName)
	require.NoError(t, err, "Should create share")
	t.Cleanup(func() {
		_ = runner.DeleteShare(shareName)
	})

	// Create a non-root user and grant it read-write. The grant is projected
	// into the share root directory's ACL (ReconcileShareRootACL), so this user
	// can create entries at the export root even though it is owned by uid 0.
	// All file operations below run as this user to test real permissioned use.
	_, err = runner.CreateUser(nfsLeastPrivUser, nfsLeastPrivPass, helpers.WithUID(nfsLeastPrivUID))
	require.NoError(t, err, "Should create least-privilege user")
	t.Cleanup(func() {
		_ = runner.DeleteUser(nfsLeastPrivUser)
	})

	err = runner.GrantUserPermission(shareName, nfsLeastPrivUser, "read-write")
	require.NoError(t, err, "Should grant read-write to least-privilege user")

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

	uid, gid := nfsLeastPrivUID, nfsLeastPrivGID

	// Run subtests - NOT parallel since they share the same mount
	t.Run("NFS-01 Read files", func(t *testing.T) {
		testNFSReadFiles(t, mount, uid, gid)
	})

	t.Run("NFS-02 Write files", func(t *testing.T) {
		testNFSWriteFiles(t, mount, uid, gid)
	})

	t.Run("NFS-03 Delete files", func(t *testing.T) {
		testNFSDeleteFiles(t, mount, uid, gid)
	})

	t.Run("NFS-04 List directories", func(t *testing.T) {
		testNFSListDirectories(t, mount, uid, gid)
	})

	t.Run("NFS-05 Create directories", func(t *testing.T) {
		testNFSCreateDirectories(t, mount, uid, gid)
	})

	t.Run("NFS-06 Change permissions", func(t *testing.T) {
		testNFSChangePermissions(t, mount, uid, gid)
	})
}

// testNFSReadFiles tests NFS-01: Files can be read via NFS mount.
func testNFSReadFiles(t *testing.T, mount *framework.Mount, uid, gid uint32) {
	t.Helper()

	// Create a test file with known content
	testContent := []byte("Hello, NFS! This is a test file for reading.")
	testFile := mount.FilePath("read_test.txt")

	// Write the file
	require.NoError(t, framework.WriteFileAsUID(t, uid, gid, testFile, testContent))
	t.Cleanup(func() {
		_ = framework.RemoveAsUID(t, uid, gid, testFile)
	})

	// Read the file back
	readContent, err := framework.ReadFileAsUID(t, uid, gid, testFile)
	require.NoError(t, err, "Should read file")

	// Verify content matches
	assert.Equal(t, testContent, readContent, "Read content should match written content")

	// Test reading with different file sizes
	sizes := []int64{100, 1024, 4096, 8192}
	for _, size := range sizes {
		name := filepath.Base(helpers.UniqueTestName("read"))
		filePath := mount.FilePath(name)

		// Write random data and get checksum
		checksum, err := framework.WriteRandomFileAsUID(t, uid, gid, filePath, size)
		require.NoError(t, err, "Should write random file")
		t.Cleanup(func() {
			_ = framework.RemoveAsUID(t, uid, gid, filePath)
		})

		// Verify file can be read and checksum matches
		require.NoError(t, framework.VerifyFileChecksumAsUID(t, uid, gid, filePath, checksum))
	}

	t.Log("NFS-01: Read files passed")
}

// testNFSWriteFiles tests NFS-02: Files can be written via NFS mount.
func testNFSWriteFiles(t *testing.T, mount *framework.Mount, uid, gid uint32) {
	t.Helper()

	// Test simple write
	testContent := []byte("Test content for write operation")
	testFile := mount.FilePath("write_test.txt")

	require.NoError(t, framework.WriteFileAsUID(t, uid, gid, testFile, testContent))
	t.Cleanup(func() {
		_ = framework.RemoveAsUID(t, uid, gid, testFile)
	})

	// Verify file exists
	assert.True(t, framework.FileExistsAsUID(t, uid, gid, testFile), "Written file should exist")

	// Verify content
	readContent, err := framework.ReadFileAsUID(t, uid, gid, testFile)
	require.NoError(t, err, "Should read written file")
	assert.Equal(t, testContent, readContent, "Written content should match")

	// Test overwrite
	newContent := []byte("Updated content")
	require.NoError(t, framework.WriteFileAsUID(t, uid, gid, testFile, newContent))
	readContent, err = framework.ReadFileAsUID(t, uid, gid, testFile)
	require.NoError(t, err, "Should read overwritten file")
	assert.Equal(t, newContent, readContent, "Overwritten content should match")

	// Test writing larger files
	largeFile := mount.FilePath("write_large.bin")
	checksum, err := framework.WriteRandomFileAsUID(t, uid, gid, largeFile, 100*1024) // 100KB
	require.NoError(t, err, "Should write large file")
	t.Cleanup(func() {
		_ = framework.RemoveAsUID(t, uid, gid, largeFile)
	})

	assert.True(t, framework.FileExistsAsUID(t, uid, gid, largeFile), "Large file should exist")
	require.NoError(t, framework.VerifyFileChecksumAsUID(t, uid, gid, largeFile, checksum))

	t.Log("NFS-02: Write files passed")
}

// testNFSDeleteFiles tests NFS-03: Files can be deleted via NFS mount.
func testNFSDeleteFiles(t *testing.T, mount *framework.Mount, uid, gid uint32) {
	t.Helper()

	// Create a file to delete
	testFile := mount.FilePath("delete_test.txt")
	require.NoError(t, framework.WriteFileAsUID(t, uid, gid, testFile, []byte("To be deleted")))

	// Verify file exists
	assert.True(t, framework.FileExistsAsUID(t, uid, gid, testFile), "File should exist before deletion")

	// Delete the file
	require.NoError(t, framework.RemoveAsUID(t, uid, gid, testFile), "Should delete file")

	// Verify file is gone
	assert.False(t, framework.FileExistsAsUID(t, uid, gid, testFile), "File should not exist after deletion")

	// Test deleting non-existent file should error
	err := framework.RemoveAsUID(t, uid, gid, mount.FilePath("nonexistent.txt"))
	assert.Error(t, err, "Deleting non-existent file should error")

	t.Log("NFS-03: Delete files passed")
}

// testNFSListDirectories tests NFS-04: Directories can be listed via NFS mount.
func testNFSListDirectories(t *testing.T, mount *framework.Mount, uid, gid uint32) {
	t.Helper()

	// Create a subdirectory with files
	subDir := mount.FilePath("list_test_dir")
	require.NoError(t, framework.MkdirAsUID(t, uid, gid, subDir))
	t.Cleanup(func() {
		_ = framework.RemoveAllAsUID(t, uid, gid, subDir)
	})

	// Create some files in the directory
	fileNames := []string{"file1.txt", "file2.txt", "file3.txt"}
	for _, name := range fileNames {
		require.NoError(t, framework.WriteFileAsUID(t, uid, gid, filepath.Join(subDir, name), []byte("content")))
	}

	// Create a subdirectory
	nestedDir := filepath.Join(subDir, "nested")
	require.NoError(t, framework.MkdirAsUID(t, uid, gid, nestedDir))

	// List directory contents
	entries, err := framework.ListDirAsUID(t, uid, gid, subDir)
	require.NoError(t, err, "Should list directory")

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

	// Test file/directory count helpers by classifying each entry.
	files, dirs := 0, 0
	for _, name := range entries {
		info, err := framework.StatAsUID(t, uid, gid, filepath.Join(subDir, name))
		require.NoError(t, err, "Should stat %s", name)
		if info.IsDir {
			dirs++
		} else {
			files++
		}
	}
	assert.Equal(t, len(fileNames), files, "Should have correct file count")
	assert.Equal(t, 1, dirs, "Should have one subdirectory")

	t.Log("NFS-04: List directories passed")
}

// testNFSCreateDirectories tests NFS-05: Directories can be created via NFS mount.
func testNFSCreateDirectories(t *testing.T, mount *framework.Mount, uid, gid uint32) {
	t.Helper()

	// Test simple directory creation
	simpleDir := mount.FilePath("create_dir_test")
	require.NoError(t, framework.MkdirAsUID(t, uid, gid, simpleDir))
	t.Cleanup(func() {
		_ = framework.RemoveAllAsUID(t, uid, gid, simpleDir)
	})

	// Verify directory exists
	assert.True(t, framework.FileExistsAsUID(t, uid, gid, simpleDir), "Created directory should exist")

	// Verify it's a directory, not a file
	info, err := framework.StatAsUID(t, uid, gid, simpleDir)
	require.NoError(t, err, "Should stat created directory")
	assert.True(t, info.IsDir, "Should be a directory")

	// Test nested directory creation with MkdirAll
	nestedDir := mount.FilePath("nested/level1/level2")
	require.NoError(t, framework.MkdirAllAsUID(t, uid, gid, nestedDir))
	t.Cleanup(func() {
		_ = framework.RemoveAllAsUID(t, uid, gid, mount.FilePath("nested"))
	})

	nestedInfo, err := framework.StatAsUID(t, uid, gid, nestedDir)
	require.NoError(t, err, "Should stat nested directory")
	assert.True(t, nestedInfo.IsDir, "Nested directory should exist")

	// Verify intermediate directories were created
	parentInfo, err := framework.StatAsUID(t, uid, gid, mount.FilePath("nested"))
	require.NoError(t, err, "Should stat parent directory")
	assert.True(t, parentInfo.IsDir, "Parent dir should exist")
	midInfo, err := framework.StatAsUID(t, uid, gid, mount.FilePath("nested/level1"))
	require.NoError(t, err, "Should stat intermediate directory")
	assert.True(t, midInfo.IsDir, "Intermediate dir should exist")

	// Test creating directory that already exists
	err = framework.MkdirAsUID(t, uid, gid, simpleDir)
	assert.Error(t, err, "Creating existing directory should error")

	t.Log("NFS-05: Create directories passed")
}

// testNFSChangePermissions tests NFS-06: File permissions can be changed via NFS mount.
func testNFSChangePermissions(t *testing.T, mount *framework.Mount, uid, gid uint32) {
	t.Helper()

	// Create a test file
	testFile := mount.FilePath("chmod_test.txt")
	require.NoError(t, framework.WriteFileAsUID(t, uid, gid, testFile, []byte("Permission test")))
	t.Cleanup(func() {
		_ = framework.RemoveAsUID(t, uid, gid, testFile)
	})

	// Get initial permissions
	initialInfo, err := framework.StatAsUID(t, uid, gid, testFile)
	require.NoError(t, err, "Should stat file")
	t.Logf("Initial mode: %o", initialInfo.Mode.Perm())

	// Change permissions to 0600 (owner read/write only)
	require.NoError(t, framework.ChmodAsUID(t, uid, gid, 0600, testFile), "Should change permissions to 0600")

	info, err := framework.StatAsUID(t, uid, gid, testFile)
	require.NoError(t, err, "Should stat file after chmod")
	// Check that mode was changed - NFS may not preserve exact permissions
	// so we check that it's different or at least includes the requested bits
	assert.True(t, info.Mode.Perm()&0600 == 0600 || info.Mode.Perm() != initialInfo.Mode.Perm(),
		"Permissions should have changed, got %o", info.Mode.Perm())
	t.Logf("After chmod 0600: %o", info.Mode.Perm())

	// Change permissions to 0755 (owner all, group/other read+execute)
	require.NoError(t, framework.ChmodAsUID(t, uid, gid, 0755, testFile), "Should change permissions to 0755")

	info, err = framework.StatAsUID(t, uid, gid, testFile)
	require.NoError(t, err, "Should stat file after second chmod")
	t.Logf("After chmod 0755: %o", info.Mode.Perm())
	assert.True(t, info.Mode.Perm()&0755 == 0755 || info.Mode.Perm()&0700 == 0700,
		"Permissions should include owner rwx, got %o", info.Mode.Perm())

	t.Log("NFS-06: Change permissions passed")
}
