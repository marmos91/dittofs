//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// NFSv4.1 Coexistence E2E Tests
// =============================================================================
//
// These tests validate that NFSv4.0 and NFSv4.1 clients can coexist on the
// same share simultaneously, with bidirectional cross-version visibility for
// files, directories, renames, and deletes. Also tests v3+v4.1 coexistence.

// =============================================================================
// Test 1: v4.0 + v4.1 Coexistence
// =============================================================================

// TestNFSv41v40Coexistence mounts BOTH v4.0 and v4.1 simultaneously on the
// SAME share and verifies that operations from one version are visible from
// the other. This validates that the server correctly handles concurrent
// stateful sessions from different minor versions.
func TestNFSv41v40Coexistence(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping v4.0/v4.1 coexistence tests in short mode")
	}

	// Both v4.0 and v4.1 require Linux
	framework.SkipIfNFSv4Unsupported(t)
	framework.SkipIfNFSv41Unsupported(t)

	_, _, nfsPort := setupNFSv4TestServer(t)

	// Mount both versions simultaneously on the same share
	mountV40 := framework.MountNFSWithVersion(t, nfsPort, "4.0")
	t.Cleanup(mountV40.Cleanup)

	mountV41 := framework.MountNFSWithVersion(t, nfsPort, "4.1")
	t.Cleanup(mountV41.Cleanup)

	t.Run("WriteV40ReadV41", func(t *testing.T) {
		content := []byte("Written from NFSv4.0, read from NFSv4.1")
		filePath40 := mountV40.FilePath("coexist_v40_write.txt")

		framework.WriteFile(t, filePath40, content)
		t.Cleanup(func() { _ = os.Remove(filePath40) })

		// Allow NFS attribute cache to settle between versions
		time.Sleep(500 * time.Millisecond)

		// Read from v4.1 mount
		filePath41 := mountV41.FilePath("coexist_v40_write.txt")
		readContent := framework.ReadFile(t, filePath41)
		assert.Equal(t, content, readContent,
			"v4.1 should read file written by v4.0")
	})

	t.Run("WriteV41ReadV40", func(t *testing.T) {
		content := []byte("Written from NFSv4.1, read from NFSv4.0")
		filePath41 := mountV41.FilePath("coexist_v41_write.txt")

		framework.WriteFile(t, filePath41, content)
		t.Cleanup(func() { _ = os.Remove(filePath41) })

		// Allow NFS attribute cache to settle between versions
		time.Sleep(500 * time.Millisecond)

		// Read from v4.0 mount
		filePath40 := mountV40.FilePath("coexist_v41_write.txt")
		readContent := framework.ReadFile(t, filePath40)
		assert.Equal(t, content, readContent,
			"v4.0 should read file written by v4.1")
	})

	t.Run("MkdirV40ListV41", func(t *testing.T) {
		dirPath40 := mountV40.FilePath("coexist_dir_v40")
		framework.CreateDir(t, dirPath40)
		t.Cleanup(func() { _ = os.RemoveAll(dirPath40) })

		// Create files inside the directory from v4.0
		framework.WriteFile(t, filepath.Join(dirPath40, "file1.txt"), []byte("from v4.0"))
		framework.WriteFile(t, filepath.Join(dirPath40, "file2.txt"), []byte("from v4.0"))

		// Allow cache to settle
		time.Sleep(500 * time.Millisecond)

		// List from v4.1 mount
		dirPath41 := mountV41.FilePath("coexist_dir_v40")
		assert.True(t, framework.DirExists(dirPath41),
			"v4.1 should see directory created by v4.0")

		entries := framework.ListDir(t, dirPath41)
		assert.Len(t, entries, 2,
			"v4.1 should see both files created by v4.0 in directory")
	})

	t.Run("MkdirV41ListV40", func(t *testing.T) {
		dirPath41 := mountV41.FilePath("coexist_dir_v41")
		framework.CreateDir(t, dirPath41)
		t.Cleanup(func() { _ = os.RemoveAll(dirPath41) })

		// Create files inside the directory from v4.1
		framework.WriteFile(t, filepath.Join(dirPath41, "fileA.txt"), []byte("from v4.1"))
		framework.WriteFile(t, filepath.Join(dirPath41, "fileB.txt"), []byte("from v4.1"))

		// Allow cache to settle
		time.Sleep(500 * time.Millisecond)

		// List from v4.0 mount
		dirPath40 := mountV40.FilePath("coexist_dir_v41")
		assert.True(t, framework.DirExists(dirPath40),
			"v4.0 should see directory created by v4.1")

		entries := framework.ListDir(t, dirPath40)
		assert.Len(t, entries, 2,
			"v4.0 should see both files created by v4.1 in directory")
	})

	t.Run("RenameV40SeeV41", func(t *testing.T) {
		// Create a file from v4.0
		srcPath40 := mountV40.FilePath("coexist_rename_src.txt")
		dstName := "coexist_rename_dst.txt"
		dstPath40 := mountV40.FilePath(dstName)
		content := []byte("rename test content")

		framework.WriteFile(t, srcPath40, content)

		// Rename from v4.0
		err := os.Rename(srcPath40, dstPath40)
		require.NoError(t, err, "Should rename file from v4.0")
		t.Cleanup(func() { _ = os.Remove(dstPath40) })

		// Allow cache to settle
		time.Sleep(500 * time.Millisecond)

		// Verify from v4.1: source gone, destination present with correct content
		srcPath41 := mountV41.FilePath("coexist_rename_src.txt")
		dstPath41 := mountV41.FilePath(dstName)

		assert.False(t, framework.FileExists(srcPath41),
			"v4.1 should NOT see old filename after rename by v4.0")
		assert.True(t, framework.FileExists(dstPath41),
			"v4.1 should see new filename after rename by v4.0")

		readContent := framework.ReadFile(t, dstPath41)
		assert.Equal(t, content, readContent,
			"v4.1 should read correct content of renamed file")
	})

	t.Run("DeleteV41SeeV40", func(t *testing.T) {
		// Create a file from v4.0
		fileName := "coexist_delete_target.txt"
		filePath40 := mountV40.FilePath(fileName)
		framework.WriteFile(t, filePath40, []byte("to be deleted by v4.1"))

		// Verify it exists from both
		time.Sleep(500 * time.Millisecond)

		filePath41 := mountV41.FilePath(fileName)
		assert.True(t, framework.FileExists(filePath41),
			"v4.1 should see file created by v4.0")

		// Delete from v4.1
		err := os.Remove(filePath41)
		require.NoError(t, err, "Should delete file from v4.1")

		// Allow cache to settle
		time.Sleep(500 * time.Millisecond)

		// Verify gone from v4.0
		assert.False(t, framework.FileExists(filePath40),
			"v4.0 should NOT see file after deletion by v4.1")
	})
}

// =============================================================================
// Test 2: v3 + v4.1 Coexistence
// =============================================================================

// TestNFSv41v3Coexistence mounts BOTH v3 and v4.1 simultaneously on the
// SAME share and verifies cross-version visibility. This tests that the
// stateless v3 protocol and stateful v4.1 sessions can coexist.
func TestNFSv41v3Coexistence(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping v3/v4.1 coexistence tests in short mode")
	}

	// v4.1 requires Linux
	framework.SkipIfNFSv41Unsupported(t)

	_, _, nfsPort := setupNFSv4TestServer(t)

	// Mount both v3 and v4.1 simultaneously
	mountV3 := framework.MountNFSWithVersion(t, nfsPort, "3")
	t.Cleanup(mountV3.Cleanup)

	mountV41 := framework.MountNFSWithVersion(t, nfsPort, "4.1")
	t.Cleanup(mountV41.Cleanup)

	t.Run("WriteV3ReadV41", func(t *testing.T) {
		content := []byte("Written from NFSv3, read from NFSv4.1")
		filePath3 := mountV3.FilePath("v3v41_from_v3.txt")

		framework.WriteFile(t, filePath3, content)
		t.Cleanup(func() { _ = os.Remove(filePath3) })

		// Allow NFS attribute cache to settle
		time.Sleep(500 * time.Millisecond)

		filePath41 := mountV41.FilePath("v3v41_from_v3.txt")
		readContent := framework.ReadFile(t, filePath41)
		assert.Equal(t, content, readContent,
			"v4.1 should read file written by v3")
	})

	t.Run("WriteV41ReadV3", func(t *testing.T) {
		content := []byte("Written from NFSv4.1, read from NFSv3")
		filePath41 := mountV41.FilePath("v3v41_from_v41.txt")

		framework.WriteFile(t, filePath41, content)
		t.Cleanup(func() { _ = os.Remove(filePath41) })

		// Allow NFS attribute cache to settle
		time.Sleep(500 * time.Millisecond)

		filePath3 := mountV3.FilePath("v3v41_from_v41.txt")
		readContent := framework.ReadFile(t, filePath3)
		assert.Equal(t, content, readContent,
			"v3 should read file written by v4.1")
	})

	t.Run("MkdirV3ListV41", func(t *testing.T) {
		dirPath3 := mountV3.FilePath("v3v41_dir_from_v3")
		framework.CreateDir(t, dirPath3)
		t.Cleanup(func() { _ = os.RemoveAll(dirPath3) })

		for i := range 3 {
			framework.WriteFile(t,
				filepath.Join(dirPath3, fmt.Sprintf("file%d.txt", i)),
				[]byte(fmt.Sprintf("content %d from v3", i)))
		}

		time.Sleep(500 * time.Millisecond)

		dirPath41 := mountV41.FilePath("v3v41_dir_from_v3")
		assert.True(t, framework.DirExists(dirPath41),
			"v4.1 should see directory created by v3")

		entries := framework.ListDir(t, dirPath41)
		assert.Len(t, entries, 3,
			"v4.1 should see all 3 files created by v3")
	})

	t.Run("MkdirV41ListV3", func(t *testing.T) {
		dirPath41 := mountV41.FilePath("v3v41_dir_from_v41")
		framework.CreateDir(t, dirPath41)
		t.Cleanup(func() { _ = os.RemoveAll(dirPath41) })

		for i := range 3 {
			framework.WriteFile(t,
				filepath.Join(dirPath41, fmt.Sprintf("file%d.txt", i)),
				[]byte(fmt.Sprintf("content %d from v4.1", i)))
		}

		time.Sleep(500 * time.Millisecond)

		dirPath3 := mountV3.FilePath("v3v41_dir_from_v41")
		assert.True(t, framework.DirExists(dirPath3),
			"v3 should see directory created by v4.1")

		entries := framework.ListDir(t, dirPath3)
		assert.Len(t, entries, 3,
			"v3 should see all 3 files created by v4.1")
	})

	t.Run("LargeFileV41ReadV3", func(t *testing.T) {
		filePath41 := mountV41.FilePath("v3v41_large.bin")
		checksum := framework.WriteRandomFile(t, filePath41, 1*1024*1024) // 1MB
		t.Cleanup(func() { _ = os.Remove(filePath41) })

		time.Sleep(500 * time.Millisecond)

		filePath3 := mountV3.FilePath("v3v41_large.bin")
		framework.VerifyFileChecksum(t, filePath3, checksum)
		t.Log("v3 read 1MB file written by v4.1 with matching checksum")
	})
}
