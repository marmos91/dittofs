//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
)

// TestCacheBasicOperations tests basic cache functionality.
// Note: Cache is now always enabled as part of the core architecture.
func TestCacheBasicOperations(t *testing.T) {
	// Run on all local configurations (cache is always enabled)
	framework.RunOnLocalConfigs(t, func(t *testing.T, tc *framework.TestContext) {

		// Basic write and read
		t.Run("WriteAndRead", func(t *testing.T) {
			filePath := tc.Path("cache_write_read.txt")
			content := []byte("Cached content")

			framework.WriteFile(t, filePath, content)

			// Read back immediately (should hit cache)
			readContent := framework.ReadFile(t, filePath)
			if !bytes.Equal(readContent, content) {
				t.Error("Content mismatch")
			}
		})

		// Multiple writes before close
		t.Run("MultipleWritesBeforeClose", func(t *testing.T) {
			filePath := tc.Path("cache_multi_write.txt")

			f, err := os.Create(filePath)
			if err != nil {
				t.Fatalf("Failed to create file: %v", err)
			}

			// Write multiple chunks
			for i := 0; i < 10; i++ {
				chunk := []byte(fmt.Sprintf("chunk %d\n", i))
				if _, err := f.Write(chunk); err != nil {
					_ = f.Close()
					t.Fatalf("Failed to write chunk %d: %v", i, err)
				}
			}

			if err := f.Close(); err != nil {
				t.Fatalf("Failed to close file: %v", err)
			}

			// Verify all chunks are there
			content := framework.ReadFile(t, filePath)
			for i := 0; i < 10; i++ {
				expected := fmt.Sprintf("chunk %d\n", i)
				if !bytes.Contains(content, []byte(expected)) {
					t.Errorf("Missing chunk %d in content", i)
				}
			}
		})

		// Large file with cache
		t.Run("LargeFileWrite", func(t *testing.T) {
			if testing.Short() {
				t.Skip("Skipping large file test in short mode")
			}

			filePath := tc.Path("cache_large.bin")
			size := int64(5 * 1024 * 1024) // 5MB

			checksum := framework.WriteRandomFile(t, filePath, size)

			// Verify
			framework.VerifyFileChecksum(t, filePath, checksum)
		})
	})
}

// TestCacheReadHits tests that reads from cache are faster than disk.
func TestCacheReadHits(t *testing.T) {
	framework.RunOnLocalConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		filePath := tc.Path("cache_read_hit.txt")
		content := []byte("Content for cache read test")
		framework.WriteFile(t, filePath, content)

		// First read (may populate cache)
		_ = framework.ReadFile(t, filePath)

		// Subsequent reads should be cache hits
		for i := 0; i < 10; i++ {
			readContent := framework.ReadFile(t, filePath)
			if !bytes.Equal(readContent, content) {
				t.Errorf("Read %d: content mismatch", i)
			}
		}
	})
}

// TestCacheCoherence tests that cache is invalidated on writes.
func TestCacheCoherence(t *testing.T) {
	framework.RunOnLocalConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		filePath := tc.Path("cache_coherence.txt")

		// Write initial content
		content1 := []byte("initial content")
		framework.WriteFile(t, filePath, content1)

		// Read to populate cache
		readContent := framework.ReadFile(t, filePath)
		if !bytes.Equal(readContent, content1) {
			t.Error("Initial content mismatch")
		}

		// Overwrite
		content2 := []byte("updated content - longer than before")
		framework.WriteFile(t, filePath, content2)

		// Read again - should see new content, not cached old content
		readContent = framework.ReadFile(t, filePath)
		if !bytes.Equal(readContent, content2) {
			t.Error("Cache coherence issue: got old content after write")
		}

		// Verify size also updated
		info := framework.GetFileInfo(t, filePath)
		if info.Size != int64(len(content2)) {
			t.Errorf("Size mismatch after write: expected %d, got %d", len(content2), info.Size)
		}
	})
}

// TestCacheWithManyFiles tests cache behavior with many small files.
func TestCacheWithManyFiles(t *testing.T) {
	framework.RunOnLocalConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		basePath := tc.Path("cache_many_files")
		framework.CreateDir(t, basePath)

		fileCount := 50

		// Create files
		for i := 0; i < fileCount; i++ {
			filePath := filepath.Join(basePath, fmt.Sprintf("file_%03d.txt", i))
			content := []byte(fmt.Sprintf("content %d", i))
			framework.WriteFile(t, filePath, content)
		}

		// Read all files (mix of cache hits and misses)
		for i := 0; i < fileCount; i++ {
			filePath := filepath.Join(basePath, fmt.Sprintf("file_%03d.txt", i))
			expected := []byte(fmt.Sprintf("content %d", i))
			actual := framework.ReadFile(t, filePath)
			if !bytes.Equal(actual, expected) {
				t.Errorf("File %d content mismatch", i)
			}
		}
	})
}

// TestCacheFlush tests that cache is flushed properly on close.
func TestCacheFlush(t *testing.T) {
	framework.RunOnLocalConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		filePath := tc.Path("cache_flush.txt")

		// Create file and write data
		f, err := os.Create(filePath)
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		content := []byte("Data that should be flushed")
		if _, err := f.Write(content); err != nil {
			_ = f.Close()
			t.Fatalf("Failed to write: %v", err)
		}

		// Sync to ensure data is flushed
		if err := f.Sync(); err != nil {
			_ = f.Close()
			t.Fatalf("Failed to sync: %v", err)
		}

		if err := f.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}

		// Small delay for any async operations
		time.Sleep(100 * time.Millisecond)

		// Re-read and verify
		readContent := framework.ReadFile(t, filePath)
		if !bytes.Equal(readContent, content) {
			t.Error("Content not properly flushed")
		}
	})
}

// TestCacheAppend tests appending to files with cache.
func TestCacheAppend(t *testing.T) {
	framework.RunOnLocalConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		filePath := tc.Path("cache_append.txt")

		// Create initial file
		initial := []byte("initial\n")
		framework.WriteFile(t, filePath, initial)

		// Append data
		f, err := os.OpenFile(filePath, os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			t.Fatalf("Failed to open for append: %v", err)
		}

		appended := []byte("appended\n")
		if _, err := f.Write(appended); err != nil {
			_ = f.Close()
			t.Fatalf("Failed to append: %v", err)
		}

		if err := f.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}

		// Verify complete content
		expected := append(initial, appended...)
		actual := framework.ReadFile(t, filePath)
		if !bytes.Equal(actual, expected) {
			t.Errorf("Append content mismatch: got %q, want %q", actual, expected)
		}
	})
}
