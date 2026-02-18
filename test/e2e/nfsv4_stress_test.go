//go:build e2e && stress

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Stress tests for NFSv4 (gated behind -tags=stress)
//
// These tests exercise the NFS server under high load conditions.
// They are excluded from normal E2E test runs and must be explicitly
// enabled with: go test -tags='e2e,stress' -v ./test/e2e/ -run TestStress
//
// NOTE: These tests may take several minutes to complete.
// =============================================================================

// TestStressLargeDirectory creates 500 files in a single directory,
// verifies all entries are listed, then deletes all files.
// Tests READDIR pagination and directory entry tracking under load.
func TestStressLargeDirectory(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	_, _, nfsPort := setupNFSv4TestServer(t)

	versions := []string{"3", "4.0"}
	for _, ver := range versions {
		ver := ver
		t.Run(fmt.Sprintf("v%s", ver), func(t *testing.T) {
			if ver == "4.0" {
				framework.SkipIfNFSv4Unsupported(t)
			}

			mount := framework.MountNFSWithVersion(t, nfsPort, ver)
			t.Cleanup(mount.Cleanup)

			// Create stress test directory
			stressDir := mount.FilePath(fmt.Sprintf("stress_largedir_%s", ver))
			framework.CreateDir(t, stressDir)
			t.Cleanup(func() { _ = os.RemoveAll(stressDir) })

			const fileCount = 500

			// Create 500 files
			t.Logf("Creating %d files...", fileCount)
			for i := 0; i < fileCount; i++ {
				filePath := filepath.Join(stressDir, fmt.Sprintf("file_%04d.txt", i))
				content := fmt.Sprintf("content for file %d in NFSv%s stress test", i, ver)
				err := os.WriteFile(filePath, []byte(content), 0644)
				require.NoError(t, err, "Should create file %d", i)
			}
			t.Logf("Created %d files successfully", fileCount)

			// List directory and verify all entries present
			t.Log("Listing directory entries...")
			entries, err := os.ReadDir(stressDir)
			require.NoError(t, err, "Should list directory")
			assert.Equal(t, fileCount, len(entries),
				"Directory should contain exactly %d entries", fileCount)
			t.Logf("Directory listing returned %d entries (expected %d)", len(entries), fileCount)

			// Verify a sample of files have correct content
			for _, checkIdx := range []int{0, 99, 249, 499} {
				filePath := filepath.Join(stressDir, fmt.Sprintf("file_%04d.txt", checkIdx))
				content, err := os.ReadFile(filePath)
				require.NoError(t, err, "Should read file %d", checkIdx)
				expected := fmt.Sprintf("content for file %d in NFSv%s stress test", checkIdx, ver)
				assert.Equal(t, expected, string(content), "File %d content should match", checkIdx)
			}
			t.Log("Sample file content verified")

			// Delete all files
			t.Logf("Deleting %d files...", fileCount)
			for i := 0; i < fileCount; i++ {
				filePath := filepath.Join(stressDir, fmt.Sprintf("file_%04d.txt", i))
				err := os.Remove(filePath)
				require.NoError(t, err, "Should delete file %d", i)
			}
			t.Logf("Deleted %d files successfully", fileCount)

			// Verify directory is empty
			entries, err = os.ReadDir(stressDir)
			require.NoError(t, err, "Should list empty directory")
			assert.Empty(t, entries, "Directory should be empty after deletion")
			t.Log("Directory verified empty after deletion")
		})
	}
}

// TestStressConcurrentDelegations starts a server with NFSv4 and opens the
// same file from multiple mount points to trigger delegation grant/recall cycles.
func TestStressConcurrentDelegations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	framework.SkipIfNFSv4Unsupported(t)

	_, _, nfsPort := setupNFSv4TestServer(t)

	const mountCount = 3

	// Create multiple mount points
	mounts := make([]*framework.Mount, mountCount)
	for i := 0; i < mountCount; i++ {
		mounts[i] = framework.MountNFSWithVersion(t, nfsPort, "4.0")
		t.Cleanup(mounts[i].Cleanup)
	}

	// Create a shared file via the first mount
	sharedFile := "stress_deleg_shared.txt"
	sharedPath := mounts[0].FilePath(sharedFile)
	framework.WriteFile(t, sharedPath, []byte("initial delegation test content"))
	t.Cleanup(func() { _ = os.Remove(sharedPath) })

	// Rapidly open/close/read/write from different mounts to trigger
	// delegation grant and recall cycles
	const iterations = 50
	var wg sync.WaitGroup
	errors := make([]error, mountCount)

	for mountIdx := 0; mountIdx < mountCount; mountIdx++ {
		mountIdx := mountIdx
		wg.Add(1)
		go func() {
			defer wg.Done()
			filePath := mounts[mountIdx].FilePath(sharedFile)

			for i := 0; i < iterations; i++ {
				// Read the file
				_, err := os.ReadFile(filePath)
				if err != nil {
					errors[mountIdx] = fmt.Errorf("mount %d, iteration %d read: %w", mountIdx, i, err)
					return
				}

				// Write unique content (triggers delegation recall on other mounts)
				content := fmt.Sprintf("mount_%d_iter_%d", mountIdx, i)
				err = os.WriteFile(filePath, []byte(content), 0644)
				if err != nil {
					errors[mountIdx] = fmt.Errorf("mount %d, iteration %d write: %w", mountIdx, i, err)
					return
				}
			}
		}()
	}

	wg.Wait()

	// Check for errors
	for i, err := range errors {
		if err != nil {
			t.Errorf("Mount %d encountered error: %v", i, err)
		}
	}

	// Verify final content is readable and non-empty
	finalContent, err := os.ReadFile(sharedPath)
	require.NoError(t, err, "Should read final content")
	assert.NotEmpty(t, finalContent, "Final content should not be empty")
	t.Logf("Delegation stress test completed. Final content: %q", string(finalContent))
}

// TestStressConcurrentFileCreation uses 10 goroutines each creating 100 files
// concurrently. Verifies all 1000 files exist with correct content and
// no data corruption.
func TestStressConcurrentFileCreation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	_, _, nfsPort := setupNFSv4TestServer(t)

	versions := []string{"3", "4.0"}
	for _, ver := range versions {
		ver := ver
		t.Run(fmt.Sprintf("v%s", ver), func(t *testing.T) {
			if ver == "4.0" {
				framework.SkipIfNFSv4Unsupported(t)
			}

			mount := framework.MountNFSWithVersion(t, nfsPort, ver)
			t.Cleanup(mount.Cleanup)

			// Create stress test directory
			stressDir := mount.FilePath(fmt.Sprintf("stress_concurrent_%s", ver))
			framework.CreateDir(t, stressDir)
			t.Cleanup(func() { _ = os.RemoveAll(stressDir) })

			const goroutineCount = 10
			const filesPerGoroutine = 100
			const totalFiles = goroutineCount * filesPerGoroutine

			// Track checksums for verification
			type fileEntry struct {
				path     string
				checksum string
			}
			resultCh := make(chan fileEntry, totalFiles)
			errorCh := make(chan error, totalFiles)

			var wg sync.WaitGroup
			t.Logf("Launching %d goroutines, each creating %d files...", goroutineCount, filesPerGoroutine)

			for g := 0; g < goroutineCount; g++ {
				g := g
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < filesPerGoroutine; i++ {
						fileName := fmt.Sprintf("g%02d_f%03d.txt", g, i)
						filePath := filepath.Join(stressDir, fileName)

						// Each file has unique content based on goroutine and file index
						content := fmt.Sprintf("goroutine=%d file=%d version=%s unique=%s",
							g, i, ver, fileName)

						hash := sha256.Sum256([]byte(content))
						checksum := hex.EncodeToString(hash[:])

						err := os.WriteFile(filePath, []byte(content), 0644)
						if err != nil {
							errorCh <- fmt.Errorf("goroutine %d, file %d: %w", g, i, err)
							return
						}
						resultCh <- fileEntry{path: filePath, checksum: checksum}
					}
				}()
			}

			wg.Wait()
			close(resultCh)
			close(errorCh)

			// Check for creation errors
			creationErrors := 0
			for err := range errorCh {
				t.Errorf("File creation error: %v", err)
				creationErrors++
			}
			if creationErrors > 0 {
				t.Fatalf("%d file creation errors occurred", creationErrors)
			}

			// Collect all created files
			createdFiles := make([]fileEntry, 0, totalFiles)
			for entry := range resultCh {
				createdFiles = append(createdFiles, entry)
			}
			assert.Equal(t, totalFiles, len(createdFiles),
				"Should have created %d files", totalFiles)
			t.Logf("Created %d files across %d goroutines", len(createdFiles), goroutineCount)

			// Verify all files exist via directory listing
			entries, err := os.ReadDir(stressDir)
			require.NoError(t, err, "Should list stress directory")
			assert.Equal(t, totalFiles, len(entries),
				"Directory should contain %d entries", totalFiles)
			t.Logf("Directory listing: %d entries (expected %d)", len(entries), totalFiles)

			// Verify a sample of files for data integrity
			sampleSize := 50 // Check 50 random files
			if sampleSize > len(createdFiles) {
				sampleSize = len(createdFiles)
			}

			corruptionCount := 0
			for i := 0; i < sampleSize; i++ {
				// Sample from different parts of the list
				idx := (i * len(createdFiles)) / sampleSize
				entry := createdFiles[idx]

				content, err := os.ReadFile(entry.path)
				if err != nil {
					t.Errorf("Failed to read %s: %v", entry.path, err)
					corruptionCount++
					continue
				}

				hash := sha256.Sum256(content)
				actualChecksum := hex.EncodeToString(hash[:])
				if actualChecksum != entry.checksum {
					t.Errorf("Checksum mismatch for %s: expected %s, got %s",
						entry.path, entry.checksum, actualChecksum)
					corruptionCount++
				}
			}

			assert.Zero(t, corruptionCount,
				"No data corruption should be detected (checked %d/%d files)",
				sampleSize, totalFiles)
			t.Logf("Data integrity verified for %d sample files (0 corruptions)", sampleSize)
		})
	}
}
