//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/framework"
)

// File sizes for testing
var fileSizes = []struct {
	name      string
	size      int64
	skipShort bool // Skip in short mode
}{
	{"1MB", 1 << 20, false},
	{"10MB", 10 << 20, false},
	{"100MB", 100 << 20, true}, // Skip in short mode
	// Note: 1GB test removed - too slow for regular test runs.
	// To test 1GB files, create a separate test or use benchmarks.
}

// TestLargeFiles tests creating and reading large files.
func TestLargeFiles(t *testing.T) {
	for _, fs := range fileSizes {
		t.Run(fs.name, func(t *testing.T) {
			if fs.skipShort && testing.Short() {
				t.Skipf("Skipping %s file test in short mode", fs.name)
			}

			framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
				filePath := tc.Path(fmt.Sprintf("large_%s.bin", fs.name))

				// Write random file
				checksum := framework.WriteRandomFile(t, filePath, fs.size)

				// Verify size
				info := framework.GetFileInfo(t, filePath)
				if info.Size != fs.size {
					t.Errorf("Size mismatch: expected %d, got %d", fs.size, info.Size)
				}

				// Verify content
				framework.VerifyFileChecksum(t, filePath, checksum)
			})
		})
	}
}

// TestLargeFileWrite tests writing large files in chunks.
func TestLargeFileWrite(t *testing.T) {
	framework.SkipIfShort(t, "large file write test")

	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		filePath := tc.Path("chunked_write.bin")

		// Write 10MB in 1MB chunks
		f, err := os.Create(filePath)
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		chunkSize := 1 << 20 // 1MB
		chunks := 10
		totalSize := int64(chunkSize * chunks)

		for i := 0; i < chunks; i++ {
			chunk := framework.GenerateRandomData(t, int64(chunkSize))
			if _, err := f.Write(chunk); err != nil {
				_ = f.Close()
				t.Fatalf("Failed to write chunk %d: %v", i, err)
			}
		}

		if err := f.Close(); err != nil {
			t.Fatalf("Failed to close file: %v", err)
		}

		// Verify size
		info := framework.GetFileInfo(t, filePath)
		if info.Size != totalSize {
			t.Errorf("Size mismatch: expected %d, got %d", totalSize, info.Size)
		}
	})
}

// TestLargeFileRead tests reading large files in chunks.
func TestLargeFileRead(t *testing.T) {
	framework.SkipIfShort(t, "large file read test")

	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		filePath := tc.Path("chunked_read.bin")

		// Create 10MB file
		totalSize := int64(10 << 20)
		checksum := framework.WriteRandomFile(t, filePath, totalSize)

		// Read in chunks and verify
		f, err := os.Open(filePath)
		if err != nil {
			t.Fatalf("Failed to open file: %v", err)
		}
		defer func() { _ = f.Close() }()

		chunkSize := 1 << 20 // 1MB
		bytesRead := int64(0)
		buf := make([]byte, chunkSize)

		for {
			n, err := f.Read(buf)
			if n > 0 {
				bytesRead += int64(n)
			}
			if err != nil {
				break
			}
		}

		if bytesRead != totalSize {
			t.Errorf("Bytes read mismatch: expected %d, got %d", totalSize, bytesRead)
		}

		// Also verify checksum
		framework.VerifyFileChecksum(t, filePath, checksum)
	})
}

// TestManyFiles tests creating many files in a single directory.
func TestManyFiles(t *testing.T) {
	counts := []struct {
		name      string
		count     int
		skipShort bool
	}{
		{"100", 100, false},
		{"1000", 1000, false},
		{"10000", 10000, true}, // Only in full mode
	}

	for _, tc := range counts {
		t.Run(tc.name, func(t *testing.T) {
			if tc.skipShort && testing.Short() {
				t.Skipf("Skipping %s files test in short mode", tc.name)
			}

			framework.RunOnLocalConfigs(t, func(t *testing.T, ctx *framework.TestContext) {
				basePath := ctx.Path(fmt.Sprintf("many_files_%s", tc.name))
				framework.CreateDir(t, basePath)

				// Create files
				for i := 0; i < tc.count; i++ {
					filePath := filepath.Join(basePath, fmt.Sprintf("file_%05d.txt", i))
					framework.WriteFile(t, filePath, []byte(fmt.Sprintf("content %d", i)))
				}

				// Verify count
				entries := framework.ListDir(t, basePath)
				if len(entries) != tc.count {
					t.Errorf("Expected %d files, got %d", tc.count, len(entries))
				}

				// Verify we can read a sample
				samplePath := filepath.Join(basePath, "file_00050.txt")
				content := framework.ReadFile(t, samplePath)
				if !bytes.Equal(content, []byte("content 50")) {
					t.Error("Sample file content mismatch")
				}
			})
		})
	}
}

// TestManyDirectories tests creating many directories.
func TestManyDirectories(t *testing.T) {
	framework.RunOnLocalConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		basePath := tc.Path("many_dirs")
		framework.CreateDir(t, basePath)

		count := 100

		// Create directories
		for i := 0; i < count; i++ {
			dirPath := filepath.Join(basePath, fmt.Sprintf("dir_%03d", i))
			framework.CreateDir(t, dirPath)
		}

		// Verify count
		dirCount := framework.CountDirs(t, basePath)
		if dirCount != count {
			t.Errorf("Expected %d directories, got %d", count, dirCount)
		}
	})
}

// TestDeepNesting tests deeply nested directory structures.
func TestDeepNesting(t *testing.T) {
	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		depth := 50
		currentPath := tc.Path("deep")

		// Create base directory first
		framework.CreateDir(t, currentPath)

		// Create deep nesting
		for i := 0; i < depth; i++ {
			currentPath = filepath.Join(currentPath, fmt.Sprintf("level_%d", i))
			framework.CreateDir(t, currentPath)
		}

		// Create file at deepest level
		deepFile := filepath.Join(currentPath, "deep.txt")
		framework.WriteFile(t, deepFile, []byte("deep content"))

		// Verify file
		content := framework.ReadFile(t, deepFile)
		if !bytes.Equal(content, []byte("deep content")) {
			t.Error("Deep file content mismatch")
		}
	})
}

// TestMixedContent tests directories with mixed files and subdirectories.
func TestMixedContent(t *testing.T) {
	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		basePath := tc.Path("mixed")
		framework.CreateDir(t, basePath)

		fileCount := 20
		dirCount := 10

		// Create files
		for i := 0; i < fileCount; i++ {
			filePath := filepath.Join(basePath, fmt.Sprintf("file_%02d.txt", i))
			framework.WriteFile(t, filePath, []byte(fmt.Sprintf("file content %d", i)))
		}

		// Create directories
		for i := 0; i < dirCount; i++ {
			dirPath := filepath.Join(basePath, fmt.Sprintf("subdir_%02d", i))
			framework.CreateDir(t, dirPath)

			// Add files in each subdirectory
			for j := 0; j < 3; j++ {
				subFile := filepath.Join(dirPath, fmt.Sprintf("subfile_%d.txt", j))
				framework.WriteFile(t, subFile, []byte("sub content"))
			}
		}

		// Verify counts
		actualFileCount := framework.CountFiles(t, basePath)
		actualDirCount := framework.CountDirs(t, basePath)

		if actualFileCount != fileCount {
			t.Errorf("Expected %d files, got %d", fileCount, actualFileCount)
		}
		if actualDirCount != dirCount {
			t.Errorf("Expected %d dirs, got %d", dirCount, actualDirCount)
		}

		// Verify a subdirectory
		subDir := filepath.Join(basePath, "subdir_05")
		subFileCount := framework.CountFiles(t, subDir)
		if subFileCount != 3 {
			t.Errorf("Expected 3 files in subdir, got %d", subFileCount)
		}
	})
}

// TestLargeDirectoryListing tests listing directories with many entries.
func TestLargeDirectoryListing(t *testing.T) {
	framework.SkipIfShort(t, "large directory listing test")

	framework.RunOnLocalConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		basePath := tc.Path("large_listing")
		framework.CreateDir(t, basePath)

		count := 500

		// Create many files
		for i := 0; i < count; i++ {
			filePath := filepath.Join(basePath, fmt.Sprintf("file_%04d.txt", i))
			framework.WriteFile(t, filePath, []byte("content"))
		}

		// List directory multiple times (test READDIR/READDIRPLUS caching)
		for iteration := 0; iteration < 3; iteration++ {
			entries := framework.ListDir(t, basePath)
			if len(entries) != count {
				t.Errorf("Iteration %d: Expected %d entries, got %d", iteration, count, len(entries))
			}
		}
	})
}
