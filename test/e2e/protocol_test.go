//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"golang.org/x/sys/unix"
)

// TestExclusiveCreate tests file creation with O_EXCL flag.
// This uses NFS CREATE EXCLUSIVE mode which relies on the idempotency token.
func TestExclusiveCreate(t *testing.T) {
	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		// Test basic O_EXCL creation
		t.Run("CreateWithExcl", func(t *testing.T) {
			filePath := tc.Path("excl_test.txt")

			// Create file with O_EXCL - should succeed
			fd, err := unix.Open(filePath, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY, 0644)
			if err != nil {
				t.Fatalf("Failed to create file with O_EXCL: %v", err)
			}
			unix.Close(fd)

			// Verify file exists
			if !framework.FileExists(filePath) {
				t.Error("File should exist after O_EXCL creation")
			}
		})

		// Test O_EXCL fails when file exists
		t.Run("ExclFailsIfExists", func(t *testing.T) {
			filePath := tc.Path("excl_exists.txt")

			// Create file first
			framework.WriteFile(t, filePath, []byte("existing content"))

			// Try to create with O_EXCL - should fail with EEXIST
			fd, err := unix.Open(filePath, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY, 0644)
			if fd >= 0 {
				unix.Close(fd)
				t.Fatal("O_EXCL should fail when file exists")
			}
			if err != unix.EEXIST {
				t.Errorf("Expected EEXIST, got %v", err)
			}
		})

		// Test concurrent O_EXCL creation - only one should succeed
		t.Run("ConcurrentExclCreate", func(t *testing.T) {
			var successCount int
			var mu sync.Mutex
			var wg sync.WaitGroup

			numGoroutines := 5
			wg.Add(numGoroutines)

			for i := 0; i < numGoroutines; i++ {
				go func() {
					defer wg.Done()

					filePath := tc.Path("concurrent_excl.txt")
					fd, err := unix.Open(filePath, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY, 0644)
					if err == nil {
						unix.Close(fd)
						mu.Lock()
						successCount++
						mu.Unlock()
					}
				}()
			}

			wg.Wait()

			if successCount != 1 {
				t.Errorf("Expected exactly 1 successful O_EXCL create, got %d", successCount)
			}
		})

		// Test O_EXCL with different file names (should all succeed)
		t.Run("ExclDifferentFiles", func(t *testing.T) {
			for i := 0; i < 5; i++ {
				filePath := tc.Path(fmt.Sprintf("excl_diff_%d.txt", i))
				fd, err := unix.Open(filePath, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY, 0644)
				if err != nil {
					t.Errorf("Failed to create file %d with O_EXCL: %v", i, err)
					continue
				}
				unix.Close(fd)
			}
		})
	})
}

// TestDirectoryListingWithModifications tests that directory listing
// handles concurrent modifications correctly. The NFS cookie verifier
// helps detect when a directory has been modified during pagination.
func TestDirectoryListingWithModifications(t *testing.T) {
	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		// Create a directory with many files
		t.Run("ListLargeDirectoryWithModifications", func(t *testing.T) {
			dirPath := tc.Path("large_dir")
			framework.CreateDir(t, dirPath)

			// Create many files to ensure multiple READDIR calls (pagination)
			numFiles := 100
			for i := 0; i < numFiles; i++ {
				filePath := filepath.Join(dirPath, fmt.Sprintf("file_%03d.txt", i))
				framework.WriteFile(t, filePath, []byte(fmt.Sprintf("content %d", i)))
			}

			// Start listing the directory
			dir, err := os.Open(dirPath)
			if err != nil {
				t.Fatalf("Failed to open directory: %v", err)
			}
			defer dir.Close()

			// Read first batch of entries
			entries1, err := dir.Readdirnames(50)
			if err != nil {
				t.Fatalf("Failed to read first batch: %v", err)
			}

			// Modify the directory while listing is in progress
			newFilePath := filepath.Join(dirPath, "new_file_during_read.txt")
			framework.WriteFile(t, newFilePath, []byte("new content"))

			// Read remaining entries - client should handle this gracefully
			// The NFS client may restart the read or continue depending on implementation
			entries2, err := dir.Readdirnames(-1)
			if err != nil {
				// Some clients may return an error here, which is acceptable
				t.Logf("Got error during continued read (may be expected): %v", err)
			}

			// Verify we got entries from both reads
			totalEntries := len(entries1) + len(entries2)
			t.Logf("Got %d entries in first batch, %d in second batch (total: %d)",
				len(entries1), len(entries2), totalEntries)

			// We should get at least some entries
			if totalEntries == 0 {
				t.Error("Should have gotten some directory entries")
			}
		})

		// Test that a simple directory listing works correctly
		t.Run("SimpleDirectoryListing", func(t *testing.T) {
			dirPath := tc.Path("simple_dir")
			framework.CreateDir(t, dirPath)

			// Create files
			expectedFiles := []string{"alpha.txt", "beta.txt", "gamma.txt"}
			for _, name := range expectedFiles {
				filePath := filepath.Join(dirPath, name)
				framework.WriteFile(t, filePath, []byte("content"))
			}

			// List directory
			entries, err := os.ReadDir(dirPath)
			if err != nil {
				t.Fatalf("Failed to read directory: %v", err)
			}

			// Verify we got all expected files
			if len(entries) != len(expectedFiles) {
				t.Errorf("Expected %d entries, got %d", len(expectedFiles), len(entries))
			}

			// Verify each expected file is present
			entryNames := make(map[string]bool)
			for _, e := range entries {
				entryNames[e.Name()] = true
			}
			for _, expected := range expectedFiles {
				if !entryNames[expected] {
					t.Errorf("Expected file %s not found in listing", expected)
				}
			}
		})

		// Test rapid directory modifications
		t.Run("RapidDirectoryModifications", func(t *testing.T) {
			dirPath := tc.Path("rapid_mod_dir")
			framework.CreateDir(t, dirPath)

			// Create initial files
			for i := 0; i < 10; i++ {
				filePath := filepath.Join(dirPath, fmt.Sprintf("initial_%d.txt", i))
				framework.WriteFile(t, filePath, []byte("content"))
			}

			// Rapidly modify while reading
			var wg sync.WaitGroup
			wg.Add(2)

			// Writer goroutine
			go func() {
				defer wg.Done()
				for i := 0; i < 20; i++ {
					filePath := filepath.Join(dirPath, fmt.Sprintf("dynamic_%d.txt", i))
					framework.WriteFile(t, filePath, []byte("new content"))
				}
			}()

			// Reader goroutine
			go func() {
				defer wg.Done()
				for i := 0; i < 10; i++ {
					_, err := os.ReadDir(dirPath)
					if err != nil {
						// Errors during concurrent access are acceptable
						t.Logf("Read %d got error (may be expected): %v", i, err)
					}
				}
			}()

			wg.Wait()

			// Final listing should work and show some files
			entries, err := os.ReadDir(dirPath)
			if err != nil {
				t.Fatalf("Final directory read failed: %v", err)
			}
			if len(entries) == 0 {
				t.Error("Directory should have some entries")
			}
			t.Logf("Final directory has %d entries", len(entries))
		})
	})
}

// TestCreateFileIdempotency tests that file creation is properly handled
// when there are issues that might cause retries.
func TestCreateFileIdempotency(t *testing.T) {
	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		// Test that repeated create attempts on the same file work correctly
		t.Run("RepeatedCreateAttempts", func(t *testing.T) {
			filePath := tc.Path("repeated_create.txt")

			// First create should succeed
			framework.WriteFile(t, filePath, []byte("first content"))

			// Second create (overwrite) should also succeed
			framework.WriteFile(t, filePath, []byte("second content"))

			// Verify final content
			content := framework.ReadFile(t, filePath)
			if string(content) != "second content" {
				t.Errorf("Expected 'second content', got '%s'", string(content))
			}
		})

		// Test creating many files rapidly
		t.Run("RapidFileCreation", func(t *testing.T) {
			var wg sync.WaitGroup
			numFiles := 50
			wg.Add(numFiles)

			errors := make(chan error, numFiles)

			for i := 0; i < numFiles; i++ {
				go func(id int) {
					defer wg.Done()
					filePath := tc.Path(fmt.Sprintf("rapid_%d.txt", id))
					if err := os.WriteFile(filePath, []byte(fmt.Sprintf("content %d", id)), 0644); err != nil {
						errors <- fmt.Errorf("file %d: %w", id, err)
					}
				}(i)
			}

			wg.Wait()
			close(errors)

			var errCount int
			for err := range errors {
				t.Errorf("Creation error: %v", err)
				errCount++
			}

			if errCount > 0 {
				t.Fatalf("%d files failed to create", errCount)
			}

			// Verify all files exist
			for i := 0; i < numFiles; i++ {
				filePath := tc.Path(fmt.Sprintf("rapid_%d.txt", i))
				if !framework.FileExists(filePath) {
					t.Errorf("File rapid_%d.txt doesn't exist", i)
				}
			}
		})
	})
}
