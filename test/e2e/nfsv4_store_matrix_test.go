//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Test 1: Version-Parameterized Store Matrix (v3 + v4 x all 9 backends)
// =============================================================================

// TestStoreMatrixV4 validates that all 9 combinations of metadata stores
// (memory, badger, postgres) and payload stores (memory, filesystem, s3) work
// correctly with file operations across BOTH NFSv3 and NFSv4.0 mounts.
//
// This produces 18 subtests: 2 versions x 9 backend combinations.
// Extends the existing store_matrix_test.go pattern to the version dimension.
func TestStoreMatrixV4(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping version-parameterized store matrix tests in short mode")
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

	versions := []string{"3", "4.0"}

	for _, ver := range versions {
		ver := ver
		for _, sc := range storeMatrix {
			sc := sc
			testName := fmt.Sprintf("v%s/%s/%s", ver, sc.metadataType, sc.payloadType)

			t.Run(testName, func(t *testing.T) {
				if ver == "4.0" {
					framework.SkipIfNFSv4Unsupported(t)
				}

				// Skip postgres combinations if container unavailable
				if sc.metadataType == "postgres" && !postgresAvailable {
					t.Skip("Skipping: PostgreSQL container not available")
				}

				// Skip s3 combinations if container unavailable
				if sc.payloadType == "s3" && !localstackAvailable {
					t.Skip("Skipping: Localstack (S3) container not available")
				}

				runStoreMatrixVersionTest(t, ver, sc, postgresHelper, localstackHelper)
			})
		}
	}
}

// runStoreMatrixVersionTest executes file operation tests for a specific
// version x store combination.
func runStoreMatrixVersionTest(t *testing.T, version string, sc storeConfig, pgHelper *framework.PostgresHelper, lsHelper *framework.LocalstackHelper) {
	t.Helper()

	// Start server process
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Login as admin
	runner := helpers.LoginAsAdmin(t, sp.APIURL())

	// Create unique store names for this test
	metaStoreName := helpers.UniqueTestName("meta")
	payloadStoreName := helpers.UniqueTestName("payload")
	shareName := "/export-v4matrix"

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
		bucketName := strings.ReplaceAll(fmt.Sprintf("dittofs-v4mtx-%s", helpers.UniqueTestName("bkt")), "_", "-")
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

	framework.WaitForServer(t, nfsPort, 10*time.Second)

	// Mount with the specified version
	mount := framework.MountNFSExportWithVersion(t, nfsPort, shareName, version)
	t.Cleanup(mount.Cleanup)

	// Run basic file operation tests
	t.Run("CreateReadWriteFile", func(t *testing.T) {
		content := []byte(fmt.Sprintf("Hello from v%s %s/%s store matrix!", version, sc.metadataType, sc.payloadType))
		filePath := mount.FilePath("v4matrix_rw.txt")

		framework.WriteFile(t, filePath, content)
		t.Cleanup(func() { _ = os.Remove(filePath) })

		assert.True(t, framework.FileExists(filePath), "File should exist after creation")

		readContent := framework.ReadFile(t, filePath)
		assert.Equal(t, content, readContent, "Read content should match written content")

		// Overwrite
		newContent := []byte("Updated content")
		framework.WriteFile(t, filePath, newContent)
		readContent = framework.ReadFile(t, filePath)
		assert.Equal(t, newContent, readContent, "Overwritten content should match")
	})

	t.Run("DirectoryOps", func(t *testing.T) {
		dirPath := mount.FilePath("v4matrix_dir")
		framework.CreateDir(t, dirPath)
		t.Cleanup(func() { _ = os.RemoveAll(dirPath) })

		assert.True(t, framework.DirExists(dirPath), "Directory should exist")

		// Create files inside
		framework.WriteFile(t, filepath.Join(dirPath, "file1.txt"), []byte("file1"))
		framework.WriteFile(t, filepath.Join(dirPath, "file2.txt"), []byte("file2"))

		entries := framework.ListDir(t, dirPath)
		assert.Len(t, entries, 2, "Should have 2 files")
	})

	t.Run("DeleteFile", func(t *testing.T) {
		filePath := mount.FilePath("v4matrix_delete.txt")
		framework.WriteFile(t, filePath, []byte("to be deleted"))

		assert.True(t, framework.FileExists(filePath), "File should exist before deletion")

		err := os.Remove(filePath)
		require.NoError(t, err, "Should delete file")
		assert.False(t, framework.FileExists(filePath), "File should not exist after deletion")
	})

	t.Log("Store matrix v" + version + " " + sc.metadataType + "/" + sc.payloadType + ": PASSED")
}

// =============================================================================
// Test 2: File Size Matrix (v3 + v4 x 500KB/1MB/10MB/100MB)
// =============================================================================

// TestFileSizeMatrix validates file operations across multiple file sizes for
// both NFSv3 and NFSv4.0. Verifies write, read-back, and checksum correctness.
// The 100MB test is gated behind !testing.Short() to avoid CI slowness.
func TestFileSizeMatrix(t *testing.T) {
	// File size matrix tests - investigating for GitHub issue #130

	if testing.Short() {
		t.Skip("Skipping file size matrix tests in short mode")
	}

	type sizeSpec struct {
		name string
		size int64
		skip bool // gate behind !testing.Short()
	}

	sizes := []sizeSpec{
		{"500KB", 500 * 1024, false},
		{"1MB", 1 * 1024 * 1024, false},
		{"10MB", 10 * 1024 * 1024, false},
		{"100MB", 100 * 1024 * 1024, true}, // gated
	}

	versions := []string{"3", "4.0"}

	for _, ver := range versions {
		ver := ver
		t.Run(fmt.Sprintf("v%s", ver), func(t *testing.T) {
			if ver == "4.0" {
				framework.SkipIfNFSv4Unsupported(t)
			}

			// Start a single server for all sizes in this version
			_, _, nfsPort := setupNFSv4TestServer(t)

			mount := framework.MountNFSWithVersion(t, nfsPort, ver)
			t.Cleanup(mount.Cleanup)

			for _, sz := range sizes {
				sz := sz
				t.Run(sz.name, func(t *testing.T) {
					if sz.skip && testing.Short() {
						t.Skip("Skipping large file test in short mode")
					}

					filePath := mount.FilePath(fmt.Sprintf("size_%s_%s.bin", sz.name, ver))

					// Write random data
					checksum := framework.WriteRandomFile(t, filePath, sz.size)
					t.Cleanup(func() { _ = os.Remove(filePath) })

					// Verify file size
					info := framework.GetFileInfo(t, filePath)
					assert.Equal(t, sz.size, info.Size, "File size should be %s", sz.name)

					// Verify checksum
					framework.VerifyFileChecksum(t, filePath, checksum)

					t.Logf("File size %s via NFSv%s: PASSED (checksum verified)", sz.name, ver)
				})
			}
		})
	}
}

// =============================================================================
// Test 3: Multi-Share Concurrent (two shares mounted simultaneously)
// =============================================================================

// TestMultiShareConcurrent creates two separate shares and mounts both
// simultaneously. Verifies that files are isolated between shares and both
// function correctly for v3 and v4.0.
func TestMultiShareConcurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping multi-share concurrent tests in short mode")
	}

	// Start server
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	runner := helpers.LoginAsAdmin(t, sp.APIURL())

	// Create stores for share alpha
	metaAlpha := helpers.UniqueTestName("meta-alpha")
	payloadAlpha := helpers.UniqueTestName("payload-alpha")
	_, err := runner.CreateMetadataStore(metaAlpha, "memory")
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner.DeleteMetadataStore(metaAlpha) })

	_, err = runner.CreatePayloadStore(payloadAlpha, "memory")
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner.DeletePayloadStore(payloadAlpha) })

	// Create stores for share beta
	metaBeta := helpers.UniqueTestName("meta-beta")
	payloadBeta := helpers.UniqueTestName("payload-beta")
	_, err = runner.CreateMetadataStore(metaBeta, "memory")
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner.DeleteMetadataStore(metaBeta) })

	_, err = runner.CreatePayloadStore(payloadBeta, "memory")
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner.DeletePayloadStore(payloadBeta) })

	// Create two shares
	_, err = runner.CreateShare("/share-alpha", metaAlpha, payloadAlpha)
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner.DeleteShare("/share-alpha") })

	_, err = runner.CreateShare("/share-beta", metaBeta, payloadBeta)
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner.DeleteShare("/share-beta") })

	// Enable NFS adapter
	nfsPort := helpers.FindFreePort(t)
	_, err = runner.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = runner.DisableAdapter("nfs") })

	err = helpers.WaitForAdapterStatus(t, runner, "nfs", true, 5*time.Second)
	require.NoError(t, err)
	framework.WaitForServer(t, nfsPort, 10*time.Second)

	versions := []string{"3", "4.0"}
	for _, ver := range versions {
		ver := ver
		t.Run(fmt.Sprintf("v%s", ver), func(t *testing.T) {
			if ver == "4.0" {
				framework.SkipIfNFSv4Unsupported(t)
			}

			// Mount both shares simultaneously
			mountAlpha := framework.MountNFSExportWithVersion(t, nfsPort, "/share-alpha", ver)
			t.Cleanup(mountAlpha.Cleanup)

			mountBeta := framework.MountNFSExportWithVersion(t, nfsPort, "/share-beta", ver)
			t.Cleanup(mountBeta.Cleanup)

			// Write a file to each share
			alphaContent := []byte("Alpha share content via v" + ver)
			betaContent := []byte("Beta share content via v" + ver)

			alphaFile := mountAlpha.FilePath("alpha_file.txt")
			betaFile := mountBeta.FilePath("beta_file.txt")

			framework.WriteFile(t, alphaFile, alphaContent)
			t.Cleanup(func() { _ = os.Remove(alphaFile) })

			framework.WriteFile(t, betaFile, betaContent)
			t.Cleanup(func() { _ = os.Remove(betaFile) })

			// Verify files are readable on their respective mounts
			readAlpha := framework.ReadFile(t, alphaFile)
			assert.Equal(t, alphaContent, readAlpha, "Alpha file content should match")

			readBeta := framework.ReadFile(t, betaFile)
			assert.Equal(t, betaContent, readBeta, "Beta file content should match")

			// Verify isolation: alpha_file should NOT be visible on beta mount
			assert.False(t, framework.FileExists(mountBeta.FilePath("alpha_file.txt")),
				"Alpha file should NOT be visible on beta mount (shares are isolated)")
			assert.False(t, framework.FileExists(mountAlpha.FilePath("beta_file.txt")),
				"Beta file should NOT be visible on alpha mount (shares are isolated)")

			t.Logf("Multi-share concurrent v%s: PASSED (shares isolated)", ver)
		})
	}
}

// =============================================================================
// Test 4: Multi-Client Concurrency (two mounts to same share)
// =============================================================================

// TestMultiClientConcurrency mounts the same share twice (different mount points)
// and verifies that files written by one mount are visible to the other. Also
// tests concurrent writes from both mounts to different files via goroutines.
func TestMultiClientConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping multi-client concurrency tests in short mode")
	}

	_, _, nfsPort := setupNFSv4TestServer(t)

	versions := []string{"3", "4.0"}
	for _, ver := range versions {
		ver := ver
		t.Run(fmt.Sprintf("v%s", ver), func(t *testing.T) {
			if ver == "4.0" {
				framework.SkipIfNFSv4Unsupported(t)
			}

			// Mount the same share twice
			mount1 := framework.MountNFSWithVersion(t, nfsPort, ver)
			t.Cleanup(mount1.Cleanup)

			mount2 := framework.MountNFSWithVersion(t, nfsPort, ver)
			t.Cleanup(mount2.Cleanup)

			t.Run("CrossVisibility", func(t *testing.T) {
				// Mount1 writes a file
				content1 := []byte("written from mount1")
				file1 := mount1.FilePath("from_mount1.txt")
				framework.WriteFile(t, file1, content1)
				t.Cleanup(func() { _ = os.Remove(file1) })

				// Mount2 writes a file
				content2 := []byte("written from mount2")
				file2 := mount2.FilePath("from_mount2.txt")
				framework.WriteFile(t, file2, content2)
				t.Cleanup(func() { _ = os.Remove(file2) })

				// Allow NFS attribute cache to expire (actimeo=0 should make this immediate)
				time.Sleep(500 * time.Millisecond)

				// Mount1 should see mount2's file
				read1 := framework.ReadFile(t, mount1.FilePath("from_mount2.txt"))
				assert.Equal(t, content2, read1, "Mount1 should see mount2's file")

				// Mount2 should see mount1's file
				read2 := framework.ReadFile(t, mount2.FilePath("from_mount1.txt"))
				assert.Equal(t, content1, read2, "Mount2 should see mount1's file")
			})

			t.Run("ConcurrentWrites", func(t *testing.T) {
				const numFiles = 5
				var wg sync.WaitGroup
				checksums := make(map[string]string)
				var mu sync.Mutex

				// Both mounts write different files simultaneously
				for i := 0; i < numFiles; i++ {
					i := i
					wg.Add(2)

					// Mount1 writes
					go func() {
						defer wg.Done()
						fileName := fmt.Sprintf("concurrent_m1_%d.bin", i)
						filePath := mount1.FilePath(fileName)
						data := framework.GenerateRandomData(t, 64*1024) // 64KB each
						hash := sha256.Sum256(data)

						framework.WriteFile(t, filePath, data)

						mu.Lock()
						checksums["m1_"+fileName] = hex.EncodeToString(hash[:])
						mu.Unlock()
					}()

					// Mount2 writes
					go func() {
						defer wg.Done()
						fileName := fmt.Sprintf("concurrent_m2_%d.bin", i)
						filePath := mount2.FilePath(fileName)
						data := framework.GenerateRandomData(t, 64*1024) // 64KB each
						hash := sha256.Sum256(data)

						framework.WriteFile(t, filePath, data)

						mu.Lock()
						checksums["m2_"+fileName] = hex.EncodeToString(hash[:])
						mu.Unlock()
					}()
				}

				wg.Wait()

				// Allow caches to settle
				time.Sleep(1 * time.Second)

				// Verify all files exist and have correct checksums
				for key, expectedChecksum := range checksums {
					var filePath string
					if strings.HasPrefix(key, "m1_") {
						filePath = mount1.FilePath(strings.TrimPrefix(key, "m1_"))
					} else {
						filePath = mount2.FilePath(strings.TrimPrefix(key, "m2_"))
					}

					assert.True(t, framework.FileExists(filePath),
						"File %s should exist after concurrent write", key)
					framework.VerifyFileChecksum(t, filePath, expectedChecksum)
				}

				// Cleanup concurrent files
				t.Cleanup(func() {
					for key := range checksums {
						var filePath string
						if strings.HasPrefix(key, "m1_") {
							filePath = mount1.FilePath(strings.TrimPrefix(key, "m1_"))
						} else {
							filePath = mount2.FilePath(strings.TrimPrefix(key, "m2_"))
						}
						_ = os.Remove(filePath)
					}
				})

				t.Logf("Concurrent writes v%s: %d files verified (no corruption)", ver, len(checksums))
			})
		})
	}
}
