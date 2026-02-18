//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storeConfig defines a combination of metadata and payload store types.
type storeConfig struct {
	metadataType string // "memory", "badger", "postgres"
	payloadType  string // "memory", "filesystem", "s3"
}

// storeMatrix defines all 9 store combinations to test (MTX-01 through MTX-09).
var storeMatrix = []storeConfig{
	{"memory", "memory"},       // MTX-01
	{"memory", "filesystem"},   // MTX-02
	{"memory", "s3"},           // MTX-03
	{"badger", "memory"},       // MTX-04
	{"badger", "filesystem"},   // MTX-05
	{"badger", "s3"},           // MTX-06
	{"postgres", "memory"},     // MTX-07
	{"postgres", "filesystem"}, // MTX-08
	{"postgres", "s3"},         // MTX-09
}

// TestStoreMatrixOperations validates that all 9 combinations of metadata stores
// (memory, badger, postgres) and payload stores (memory, filesystem, s3) work
// correctly with file operations.
//
// Requirements covered:
//   - MTX-01: memory/memory
//   - MTX-02: memory/filesystem
//   - MTX-03: memory/s3
//   - MTX-04: badger/memory
//   - MTX-05: badger/filesystem
//   - MTX-06: badger/s3
//   - MTX-07: postgres/memory
//   - MTX-08: postgres/filesystem
//   - MTX-09: postgres/s3
func TestStoreMatrixOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping store matrix tests in short mode")
	}

	// Check container availability once at the start
	postgresAvailable := framework.CheckPostgresAvailable(t)
	localstackAvailable := framework.CheckLocalstackAvailable(t)

	// Initialize helpers for containers (if available)
	var postgresHelper *framework.PostgresHelper
	var localstackHelper *framework.LocalstackHelper

	if postgresAvailable {
		postgresHelper = framework.NewPostgresHelper(t)
	}

	if localstackAvailable {
		localstackHelper = framework.NewLocalstackHelper(t)
	}

	for _, sc := range storeMatrix {
		testName := fmt.Sprintf("%s/%s", sc.metadataType, sc.payloadType)
		sc := sc // capture for closure

		t.Run(testName, func(t *testing.T) {
			// Skip postgres combinations if container unavailable
			if sc.metadataType == "postgres" && !postgresAvailable {
				t.Skip("Skipping: PostgreSQL container not available")
			}

			// Skip s3 combinations if container unavailable
			if sc.payloadType == "s3" && !localstackAvailable {
				t.Skip("Skipping: Localstack (S3) container not available")
			}

			// Run the store combination test
			runStoreMatrixTest(t, sc, postgresHelper, localstackHelper)
		})
	}
}

// runStoreMatrixTest executes file operation tests for a specific store combination.
func runStoreMatrixTest(t *testing.T, sc storeConfig, pgHelper *framework.PostgresHelper, lsHelper *framework.LocalstackHelper) {
	t.Helper()

	// Start server process
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Login as admin
	runner := helpers.LoginAsAdmin(t, sp.APIURL())

	// Create unique store names for this test
	metaStoreName := helpers.UniqueTestName("meta")
	payloadStoreName := helpers.UniqueTestName("payload")
	shareName := "/export-matrix"

	// Create metadata store based on type
	var metaOpts []helpers.MetadataStoreOption
	switch sc.metadataType {
	case "memory":
		// No options needed
	case "badger":
		badgerPath := filepath.Join(t.TempDir(), "badger")
		metaOpts = append(metaOpts, helpers.WithMetaDBPath(badgerPath))
	case "postgres":
		if pgHelper == nil {
			t.Fatal("PostgreSQL helper not available")
		}
		pgConfig := pgHelper.GetConfig()
		configJSON, err := json.Marshal(map[string]interface{}{
			"host":     pgConfig.Host,
			"port":     pgConfig.Port,
			"database": pgConfig.Database,
			"user":     pgConfig.User,
			"password": pgConfig.Password,
		})
		require.NoError(t, err, "Failed to marshal postgres config")
		metaOpts = append(metaOpts, helpers.WithMetaRawConfig(string(configJSON)))
	}

	_, err := runner.CreateMetadataStore(metaStoreName, sc.metadataType, metaOpts...)
	require.NoError(t, err, "Should create metadata store (%s)", sc.metadataType)
	t.Cleanup(func() {
		_ = runner.DeleteMetadataStore(metaStoreName)
	})

	// Create payload store based on type
	var payloadOpts []helpers.PayloadStoreOption
	switch sc.payloadType {
	case "memory":
		// No options needed
	case "filesystem":
		fsPath := filepath.Join(t.TempDir(), "payload")
		require.NoError(t, os.MkdirAll(fsPath, 0755), "Should create payload directory")
		payloadOpts = append(payloadOpts, helpers.WithPayloadPath(fsPath))
	case "s3":
		if lsHelper == nil {
			t.Fatal("Localstack helper not available")
		}
		// Create a unique bucket for this test (S3 bucket names can't have underscores)
		bucketName := strings.ReplaceAll(fmt.Sprintf("dittofs-matrix-%s", helpers.UniqueTestName("bucket")), "_", "-")
		err := lsHelper.CreateBucket(context.Background(), bucketName)
		require.NoError(t, err, "Should create S3 bucket")
		t.Cleanup(func() {
			lsHelper.CleanupBucket(context.Background(), bucketName)
		})

		payloadOpts = append(payloadOpts, helpers.WithPayloadS3Config(
			bucketName,
			"us-east-1",
			lsHelper.Endpoint,
			"test",
			"test",
		))
	}

	_, err = runner.CreatePayloadStore(payloadStoreName, sc.payloadType, payloadOpts...)
	require.NoError(t, err, "Should create payload store (%s)", sc.payloadType)
	t.Cleanup(func() {
		_ = runner.DeletePayloadStore(payloadStoreName)
	})

	// Create the share using the stores
	_, err = runner.CreateShare(shareName, metaStoreName, payloadStoreName)
	require.NoError(t, err, "Should create share")
	t.Cleanup(func() {
		_ = runner.DeleteShare(shareName)
	})

	// Enable NFS adapter
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

	// Mount the NFS share with the custom export name
	mount := mountNFSExport(t, nfsPort, shareName)
	t.Cleanup(mount.Cleanup)

	// Run file operation tests
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

	t.Run("LargeFile1MB", func(t *testing.T) {
		testMatrixLargeFile(t, mount)
	})
}

// mountNFSExport mounts an NFS share with a custom export path.
func mountNFSExport(t *testing.T, port int, exportPath string) *framework.Mount {
	t.Helper()

	// Give the NFS server a moment to fully initialize
	time.Sleep(500 * time.Millisecond)

	// Create mount directory
	mountPath, err := os.MkdirTemp("", "dittofs-e2e-matrix-*")
	if err != nil {
		t.Fatalf("Failed to create NFS mount directory: %v", err)
	}

	// Build mount command with custom export path
	mountOptions := fmt.Sprintf("nfsvers=3,tcp,port=%d,mountport=%d,actimeo=0", port, port)

	var mountArgs []string
	switch runtime.GOOS {
	case "darwin":
		mountOptions += ",resvport"
		mountArgs = []string{"-t", "nfs", "-o", mountOptions, fmt.Sprintf("localhost:%s", exportPath), mountPath}
	case "linux":
		mountOptions += ",nolock"
		mountArgs = []string{"-t", "nfs", "-o", mountOptions, fmt.Sprintf("localhost:%s", exportPath), mountPath}
	default:
		_ = os.RemoveAll(mountPath)
		t.Fatalf("Unsupported platform for NFS: %s", runtime.GOOS)
	}

	// Execute mount command with retries
	var output []byte
	var lastErr error
	maxRetries := 3

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

	// Create a test file with known content
	testContent := []byte("Hello, Store Matrix! Testing file operations.")
	testFile := mount.FilePath("matrix_test.txt")

	// Write file
	framework.WriteFile(t, testFile, testContent)
	t.Cleanup(func() {
		_ = os.Remove(testFile)
	})

	// Verify file exists
	assert.True(t, framework.FileExists(testFile), "File should exist after creation")

	// Read file and verify content
	readContent := framework.ReadFile(t, testFile)
	assert.Equal(t, testContent, readContent, "Read content should match written content")

	// Overwrite file
	newContent := []byte("Updated content for store matrix test")
	framework.WriteFile(t, testFile, newContent)

	// Verify updated content
	readContent = framework.ReadFile(t, testFile)
	assert.Equal(t, newContent, readContent, "Overwritten content should match")

	t.Log("CreateReadWriteFile: PASSED")
}

// testMatrixCreateDirectory tests directory creation and file operations inside directories.
func testMatrixCreateDirectory(t *testing.T, mount *framework.Mount) {
	t.Helper()

	// Create a directory
	testDir := mount.FilePath("matrix_dir")
	framework.CreateDir(t, testDir)
	t.Cleanup(func() {
		_ = os.RemoveAll(testDir)
	})

	// Verify directory exists
	assert.True(t, framework.DirExists(testDir), "Directory should exist")

	// Create files inside the directory
	file1 := filepath.Join(testDir, "file1.txt")
	file2 := filepath.Join(testDir, "file2.txt")

	framework.WriteFile(t, file1, []byte("File 1 content"))
	framework.WriteFile(t, file2, []byte("File 2 content"))

	// Verify files exist
	assert.True(t, framework.FileExists(file1), "File 1 should exist")
	assert.True(t, framework.FileExists(file2), "File 2 should exist")

	// Create nested directory
	nestedDir := filepath.Join(testDir, "nested")
	framework.CreateDir(t, nestedDir)
	assert.True(t, framework.DirExists(nestedDir), "Nested directory should exist")

	t.Log("CreateDirectory: PASSED")
}

// testMatrixListDirectory tests directory listing operations.
func testMatrixListDirectory(t *testing.T, mount *framework.Mount) {
	t.Helper()

	// Create a directory with known contents
	testDir := mount.FilePath("matrix_list_dir")
	framework.CreateDir(t, testDir)
	t.Cleanup(func() {
		_ = os.RemoveAll(testDir)
	})

	// Create some files
	fileNames := []string{"alpha.txt", "beta.txt", "gamma.txt"}
	for _, name := range fileNames {
		framework.WriteFile(t, filepath.Join(testDir, name), []byte("content"))
	}

	// Create a subdirectory
	subDir := filepath.Join(testDir, "subdir")
	framework.CreateDir(t, subDir)

	// List directory
	entries := framework.ListDir(t, testDir)

	// Verify all entries are present
	expectedCount := len(fileNames) + 1 // files + subdir
	assert.Len(t, entries, expectedCount, "Should have correct number of entries")

	// Verify specific entries
	for _, name := range fileNames {
		found := false
		for _, entry := range entries {
			if entry == name {
				found = true
				break
			}
		}
		assert.True(t, found, "Directory should contain %s", name)
	}

	// Verify counts
	assert.Equal(t, len(fileNames), framework.CountFiles(t, testDir), "Should have correct file count")
	assert.Equal(t, 1, framework.CountDirs(t, testDir), "Should have one subdirectory")

	t.Log("ListDirectory: PASSED")
}

// testMatrixDeleteFile tests file deletion operations.
func testMatrixDeleteFile(t *testing.T, mount *framework.Mount) {
	t.Helper()

	// Create a file to delete
	testFile := mount.FilePath("matrix_delete.txt")
	framework.WriteFile(t, testFile, []byte("To be deleted"))

	// Verify file exists
	assert.True(t, framework.FileExists(testFile), "File should exist before deletion")

	// Delete the file
	err := os.Remove(testFile)
	require.NoError(t, err, "Should delete file")

	// Verify file is gone
	assert.False(t, framework.FileExists(testFile), "File should not exist after deletion")

	// Test deleting non-existent file
	err = os.Remove(mount.FilePath("nonexistent.txt"))
	assert.Error(t, err, "Deleting non-existent file should error")

	t.Log("DeleteFile: PASSED")
}

// testMatrixLargeFile tests 1MB file operations with checksum verification.
func testMatrixLargeFile(t *testing.T, mount *framework.Mount) {
	t.Helper()

	// Write 1MB random file
	testFile := mount.FilePath("matrix_large.bin")
	checksum := framework.WriteRandomFile(t, testFile, 1*1024*1024) // 1MB
	t.Cleanup(func() {
		_ = os.Remove(testFile)
	})

	// Wait for async S3 uploads to complete by polling for expected file size.
	// S3 writes are buffered and flushed asynchronously, so we retry until the
	// file is fully visible rather than using a fixed sleep.
	require.Eventually(t, func() bool {
		info, err := os.Stat(testFile)
		return err == nil && info.Size() == int64(1*1024*1024)
	}, 10*time.Second, 250*time.Millisecond, "Large file should reach 1MB within timeout")

	// Verify checksum
	framework.VerifyFileChecksum(t, testFile, checksum)

	t.Log("LargeFile1MB: PASSED")
}
