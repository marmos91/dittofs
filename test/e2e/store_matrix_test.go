//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStoreMatrixOperations validates that all 18 combinations of the 3D store
// matrix (3 metadata x 2 local x 3 remote) work correctly with file operations
// via NFSv3.
//
// In short mode, only 3-4 representative combos run.
// With DITTOFS_E2E_LOCAL_ONLY=1, only remoteType="none" combos run.
func TestStoreMatrixOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping store matrix tests in short mode")
	}

	postgresAvailable := framework.CheckPostgresAvailable(t)
	localstackAvailable := framework.CheckLocalstackAvailable(t)

	var postgresHelper *framework.PostgresHelper
	var localstackHelper *framework.LocalstackHelper

	if postgresAvailable {
		postgresHelper = framework.NewPostgresHelper(t)
	}

	if localstackAvailable {
		localstackHelper = framework.NewLocalstackHelper(t)
	}

	matrix := getStoreMatrix(testing.Short())

	for _, sc := range matrix {
		sc := sc // capture for closure

		t.Run(sc.testName(), func(t *testing.T) {
			if sc.needsPostgres() && !postgresAvailable {
				t.Skip("Skipping: PostgreSQL container not available")
			}

			if sc.needsS3() && !localstackAvailable {
				t.Skip("Skipping: Localstack (S3) container not available")
			}

			runStoreMatrix3DTest(t, sc, postgresHelper, localstackHelper)
		})
	}
}

// runStoreMatrix3DTest executes file operation tests for a specific 3D store combination.
func runStoreMatrix3DTest(t *testing.T, sc matrixStoreConfig, pgHelper *framework.PostgresHelper, lsHelper *framework.LocalstackHelper) {
	t.Helper()

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	runner := helpers.LoginAsAdmin(t, sp.APIURL())

	shareName := "/export-matrix"

	helpers.SetupStoreMatrix(t, runner, shareName, helpers.MatrixSetupConfig{
		MetadataType: sc.metadataType,
		LocalType:    sc.localType,
		RemoteType:   sc.remoteType,
	}, pgHelper, lsHelper)

	nfsPort := helpers.FindFreePort(t)
	_, err := runner.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err, "Should enable NFS adapter")
	t.Cleanup(func() {
		_, _ = runner.DisableAdapter("nfs")
	})

	err = helpers.WaitForAdapterStatus(t, runner, "nfs", true, 5*time.Second)
	require.NoError(t, err, "NFS adapter should become enabled")

	framework.WaitForServer(t, nfsPort, 10*time.Second)

	mount := mountNFSExport(t, nfsPort, shareName)
	t.Cleanup(mount.Cleanup)

	t.Run("CreateReadWriteFile", func(t *testing.T) {
		testMatrixCreateReadWriteFile(t, mount)
	})

	t.Run("CreateDirectory", func(t *testing.T) {
		testMatrixCreateDirectory(t, mount)
	})

	t.Run("ListDirectory", func(t *testing.T) {
		testMatrixListDirectory(t, mount)
	})

	t.Run("DeleteFile", func(t *testing.T) {
		testMatrixDeleteFile(t, mount)
	})

	t.Run("Rename", func(t *testing.T) {
		testMatrixRename(t, mount)
	})

	t.Run("Truncate", func(t *testing.T) {
		testMatrixTruncate(t, mount)
	})

	t.Run("Append", func(t *testing.T) {
		testMatrixAppend(t, mount)
	})

	t.Run("LargeFile1KB", func(t *testing.T) {
		testMatrixSmallFile(t, mount)
	})
}

// mountNFSExport mounts an NFS share with a custom export path.
func mountNFSExport(t *testing.T, port int, exportPath string) *framework.Mount {
	t.Helper()

	time.Sleep(500 * time.Millisecond)

	mountPath, err := os.MkdirTemp("", "dittofs-e2e-matrix-*")
	if err != nil {
		t.Fatalf("Failed to create NFS mount directory: %v", err)
	}

	mountOptions := fmt.Sprintf("nfsvers=3,tcp,port=%d,mountport=%d,actimeo=0", port, port)

	switch runtime.GOOS {
	case "darwin":
		mountOptions += ",resvport"
	case "linux":
		mountOptions += ",nolock"
	default:
		_ = os.RemoveAll(mountPath)
		t.Fatalf("Unsupported platform for NFS: %s", runtime.GOOS)
	}

	mountArgs := []string{"-t", "nfs", "-o", mountOptions, fmt.Sprintf("localhost:%s", exportPath), mountPath}

	var output []byte
	var lastErr error
	const maxRetries = 3

	for i := 0; i < maxRetries; i++ {
		cmd := exec.Command("mount", mountArgs...)
		output, lastErr = cmd.CombinedOutput()

		if lastErr == nil {
			t.Logf("NFS share mounted successfully at %s (export: %s)", mountPath, exportPath)
			break
		}

		if i < maxRetries-1 {
			t.Logf("NFS mount attempt %d failed (error: %v), retrying in 1 second...", i+1, lastErr)
			time.Sleep(time.Second)
		}
	}

	if lastErr != nil {
		_ = os.RemoveAll(mountPath)
		t.Fatalf("Failed to mount NFS share after %d attempts: %v\nOutput: %s\nMount command: mount %v",
			maxRetries, lastErr, string(output), mountArgs)
	}

	return &framework.Mount{
		T:        t,
		Path:     mountPath,
		Protocol: "nfs",
		Port:     port,
	}
}

// testMatrixCreateReadWriteFile tests file create, write, and read operations.
func testMatrixCreateReadWriteFile(t *testing.T, mount *framework.Mount) {
	t.Helper()

	testContent := []byte("Hello, Store Matrix! Testing file operations.")
	testFile := mount.FilePath("matrix_test.txt")

	framework.WriteFile(t, testFile, testContent)
	t.Cleanup(func() {
		_ = os.Remove(testFile)
	})

	assert.True(t, framework.FileExists(testFile), "File should exist after creation")

	readContent := framework.ReadFile(t, testFile)
	assert.Equal(t, testContent, readContent, "Read content should match written content")

	newContent := []byte("Updated content for store matrix test")
	framework.WriteFile(t, testFile, newContent)

	readContent = framework.ReadFile(t, testFile)
	assert.Equal(t, newContent, readContent, "Overwritten content should match")

	t.Log("CreateReadWriteFile: PASSED")
}

// testMatrixCreateDirectory tests directory creation and file operations inside directories.
func testMatrixCreateDirectory(t *testing.T, mount *framework.Mount) {
	t.Helper()

	testDir := mount.FilePath("matrix_dir")
	framework.CreateDir(t, testDir)
	t.Cleanup(func() {
		_ = os.RemoveAll(testDir)
	})

	assert.True(t, framework.DirExists(testDir), "Directory should exist")

	file1 := filepath.Join(testDir, "file1.txt")
	file2 := filepath.Join(testDir, "file2.txt")
	framework.WriteFile(t, file1, []byte("File 1 content"))
	framework.WriteFile(t, file2, []byte("File 2 content"))

	assert.True(t, framework.FileExists(file1), "File 1 should exist")
	assert.True(t, framework.FileExists(file2), "File 2 should exist")

	nestedDir := filepath.Join(testDir, "nested")
	framework.CreateDir(t, nestedDir)
	assert.True(t, framework.DirExists(nestedDir), "Nested directory should exist")

	t.Log("CreateDirectory: PASSED")
}

// testMatrixListDirectory tests directory listing operations.
func testMatrixListDirectory(t *testing.T, mount *framework.Mount) {
	t.Helper()

	testDir := mount.FilePath("matrix_list_dir")
	framework.CreateDir(t, testDir)
	t.Cleanup(func() {
		_ = os.RemoveAll(testDir)
	})

	fileNames := []string{"alpha.txt", "beta.txt", "gamma.txt"}
	for _, name := range fileNames {
		framework.WriteFile(t, filepath.Join(testDir, name), []byte("content"))
	}

	subDir := filepath.Join(testDir, "subdir")
	framework.CreateDir(t, subDir)

	entries := framework.ListDir(t, testDir)

	expectedCount := len(fileNames) + 1 // files + subdir
	assert.Len(t, entries, expectedCount, "Should have correct number of entries")

	for _, name := range fileNames {
		assert.Contains(t, entries, name, "Directory should contain %s", name)
	}

	assert.Equal(t, len(fileNames), framework.CountFiles(t, testDir), "Should have correct file count")
	assert.Equal(t, 1, framework.CountDirs(t, testDir), "Should have one subdirectory")

	t.Log("ListDirectory: PASSED")
}

// testMatrixDeleteFile tests file deletion operations.
func testMatrixDeleteFile(t *testing.T, mount *framework.Mount) {
	t.Helper()

	testFile := mount.FilePath("matrix_delete.txt")
	framework.WriteFile(t, testFile, []byte("To be deleted"))

	assert.True(t, framework.FileExists(testFile), "File should exist before deletion")

	err := os.Remove(testFile)
	require.NoError(t, err, "Should delete file")

	assert.False(t, framework.FileExists(testFile), "File should not exist after deletion")

	err = os.Remove(mount.FilePath("nonexistent.txt"))
	assert.Error(t, err, "Deleting non-existent file should error")

	t.Log("DeleteFile: PASSED")
}

// testMatrixRename tests file and directory rename operations.
func testMatrixRename(t *testing.T, mount *framework.Mount) {
	t.Helper()

	srcFile := mount.FilePath("matrix_rename_src.txt")
	dstFile := mount.FilePath("matrix_rename_dst.txt")
	framework.WriteFile(t, srcFile, []byte("rename me"))
	t.Cleanup(func() {
		_ = os.Remove(srcFile)
		_ = os.Remove(dstFile)
	})

	err := os.Rename(srcFile, dstFile)
	require.NoError(t, err, "Should rename file")

	assert.False(t, framework.FileExists(srcFile), "Source should not exist after rename")
	assert.True(t, framework.FileExists(dstFile), "Destination should exist after rename")
	content := framework.ReadFile(t, dstFile)
	assert.Equal(t, []byte("rename me"), content, "Content should be preserved after rename")

	t.Log("Rename: PASSED")
}

// testMatrixTruncate tests file truncation operations.
func testMatrixTruncate(t *testing.T, mount *framework.Mount) {
	t.Helper()

	testFile := mount.FilePath("matrix_truncate.txt")
	framework.WriteFile(t, testFile, []byte("hello world, this is some content"))
	t.Cleanup(func() {
		_ = os.Remove(testFile)
	})

	err := os.Truncate(testFile, 5)
	require.NoError(t, err, "Should truncate file")

	info, err := os.Stat(testFile)
	require.NoError(t, err)
	assert.Equal(t, int64(5), info.Size(), "File should be truncated to 5 bytes")

	content := framework.ReadFile(t, testFile)
	assert.Equal(t, []byte("hello"), content, "Truncated content should match")

	t.Log("Truncate: PASSED")
}

// testMatrixAppend tests file append operations.
func testMatrixAppend(t *testing.T, mount *framework.Mount) {
	t.Helper()

	testFile := mount.FilePath("matrix_append.txt")
	framework.WriteFile(t, testFile, []byte("hello"))
	t.Cleanup(func() {
		_ = os.Remove(testFile)
	})

	f, err := os.OpenFile(testFile, os.O_APPEND|os.O_WRONLY, 0644)
	require.NoError(t, err, "Should open file for append")
	_, err = f.Write([]byte(" world"))
	require.NoError(t, err, "Should write appended data")
	require.NoError(t, f.Sync(), "Should sync after append")
	require.NoError(t, f.Close(), "Should close file")

	content := framework.ReadFile(t, testFile)
	assert.Equal(t, []byte("hello world"), content, "Appended content should match")

	t.Log("Append: PASSED")
}

// testMatrixSmallFile tests 1KB file operations with checksum verification.
func testMatrixSmallFile(t *testing.T, mount *framework.Mount) {
	t.Helper()

	testFile := mount.FilePath("matrix_small.bin")
	checksum := framework.WriteRandomFile(t, testFile, 1024)
	t.Cleanup(func() {
		_ = os.Remove(testFile)
	})

	require.Eventually(t, func() bool {
		info, err := os.Stat(testFile)
		return err == nil && info.Size() == int64(1024)
	}, 10*time.Second, 250*time.Millisecond, "Small file should reach 1KB within timeout")

	framework.VerifyFileChecksum(t, testFile, checksum)

	t.Log("SmallFile1KB: PASSED")
}
