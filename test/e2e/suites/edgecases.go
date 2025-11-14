package suites

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/framework"
)

// TestEdgeCases tests edge cases and boundary conditions
func TestEdgeCases(t *testing.T, storeType framework.StoreType) {
	ctx := framework.NewTestContext(t, storeType)

	t.Run("EmptyFilename", func(t *testing.T) {
		// Try to create file with empty name (should fail)
		err := ctx.WriteFile("", []byte("data"), 0644)
		if err == nil {
			t.Error("Creating file with empty name should fail")
		}
	})

	t.Run("VeryLongFilename", func(t *testing.T) {
		// Most filesystems have a 255 byte filename limit
		// Test with a long but valid filename
		longName := strings.Repeat("a", 200) + ".txt"
		content := []byte("long filename content")

		err := ctx.WriteFile(longName, content, 0644)
		if err != nil {
			t.Skipf("Long filename not supported: %v", err)
		}

		ctx.AssertFileContent(longName, content)
		_ = ctx.Remove(longName)
	})

	t.Run("MaxFilename", func(t *testing.T) {
		// Test maximum filename length (typically 255 bytes)
		maxName := strings.Repeat("x", 255)
		content := []byte("max length filename")

		err := ctx.WriteFile(maxName, content, 0644)
		if err != nil {
			t.Skipf("Max length filename not supported: %v", err)
		}

		ctx.AssertFileContent(maxName, content)
		_ = ctx.Remove(maxName)
	})

	t.Run("DotFiles", func(t *testing.T) {
		// Test hidden files (starting with dot)
		dotFile := ".hidden"
		content := []byte("hidden content")

		err := ctx.WriteFile(dotFile, content, 0644)
		if err != nil {
			t.Fatalf("Failed to create dot file: %v", err)
		}

		ctx.AssertFileContent(dotFile, content)

		// Verify it appears in directory listing
		entries, err := ctx.ReadDir(".")
		if err != nil {
			t.Fatalf("Failed to read directory: %v", err)
		}

		found := false
		for _, entry := range entries {
			if entry.Name() == dotFile {
				found = true
				break
			}
		}
		if !found {
			t.Error("Dot file not found in directory listing")
		}
	})

	t.Run("FilenameWithNewline", func(t *testing.T) {
		// Try to create file with newline in name (should fail or be escaped)
		err := ctx.WriteFile("file\nwith\nnewline.txt", []byte("data"), 0644)
		if err == nil {
			t.Skip("Filesystem allows newlines in filenames (unusual)")
		}
	})

	t.Run("FilenameWithSlash", func(t *testing.T) {
		// Slash in filename should be interpreted as directory separator
		err := ctx.WriteFile("parent/child.txt", []byte("data"), 0644)
		if err != nil {
			// Parent directory might not exist - that's expected
			// Create parent first
			_ = ctx.Mkdir("parent", 0755)
			err = ctx.WriteFile("parent/child.txt", []byte("data"), 0644)
			if err != nil {
				t.Fatalf("Failed to create file in subdirectory: %v", err)
			}
		}
		ctx.AssertFileContent("parent/child.txt", []byte("data"))
	})

	t.Run("ZeroSizeFile", func(t *testing.T) {
		// Create zero-size file
		err := ctx.WriteFile("zero.txt", []byte{}, 0644)
		if err != nil {
			t.Fatalf("Failed to create zero-size file: %v", err)
		}

		info, err := ctx.Stat("zero.txt")
		if err != nil {
			t.Fatalf("Failed to stat zero-size file: %v", err)
		}

		if info.Size() != 0 {
			t.Errorf("Zero-size file has size %d, want 0", info.Size())
		}

		// Reading should return empty
		content, err := ctx.ReadFile("zero.txt")
		if err != nil {
			t.Fatalf("Failed to read zero-size file: %v", err)
		}
		if len(content) != 0 {
			t.Errorf("Zero-size file returned %d bytes, want 0", len(content))
		}
	})

	t.Run("ConcurrentReads", func(t *testing.T) {
		// Create file
		content := []byte("concurrent read test content")
		_ = ctx.WriteFile("concurrent-read.txt", content, 0644)

		// Read concurrently from multiple goroutines
		const numReaders = 10
		done := make(chan error, numReaders)

		for i := 0; i < numReaders; i++ {
			go func() {
				data, err := ctx.ReadFile("concurrent-read.txt")
				if err != nil {
					done <- err
					return
				}
				if string(data) != string(content) {
					done <- fmt.Errorf("content mismatch")
					return
				}
				done <- nil
			}()
		}

		// Wait for all readers
		for i := 0; i < numReaders; i++ {
			err := <-done
			if err != nil {
				t.Errorf("Concurrent read %d failed: %v", i, err)
			}
		}
	})

	t.Run("ReadNonExistentFile", func(t *testing.T) {
		_, err := ctx.ReadFile("does-not-exist.txt")
		if err == nil {
			t.Error("Reading non-existent file should fail")
		}
		if !os.IsNotExist(err) {
			t.Errorf("Expected IsNotExist error, got: %v", err)
		}
	})

	t.Run("StatNonExistentFile", func(t *testing.T) {
		_, err := ctx.Stat("does-not-exist.txt")
		if err == nil {
			t.Error("Stat on non-existent file should fail")
		}
		if !os.IsNotExist(err) {
			t.Errorf("Expected IsNotExist error, got: %v", err)
		}
	})

	t.Run("RemoveOpenFile", func(t *testing.T) {
		// Create and open file
		filename := "remove-while-open.txt"
		content := []byte("delete me while open")
		_ = ctx.WriteFile(filename, content, 0644)

		file, err := os.Open(ctx.Path(filename))
		if err != nil {
			t.Fatalf("Failed to open file: %v", err)
		}
		defer func() { _ = file.Close() }()

		// Try to remove while open
		// On Unix-like systems, this should succeed
		// On Windows, this might fail (file is locked)
		err = ctx.Remove(filename)
		if err != nil {
			t.Skipf("Cannot remove open file on this platform: %v", err)
		}

		// File handle should still be valid for reading
		data, err := io.ReadAll(file)
		if err != nil {
			t.Errorf("Failed to read from deleted file handle: %v", err)
		}
		if string(data) != string(content) {
			t.Error("Content changed after deletion")
		}
	})

	t.Run("SymlinkLoop", func(t *testing.T) {
		// Create symlink loop: a -> b -> a
		err := ctx.Symlink("loop-b", "loop-a")
		if err != nil {
			t.Fatalf("Failed to create first symlink: %v", err)
		}

		err = ctx.Symlink("loop-a", "loop-b")
		if err != nil {
			t.Fatalf("Failed to create second symlink: %v", err)
		}

		// Try to read through the loop (should detect and fail)
		_, err = ctx.ReadFile("loop-a")
		if err == nil {
			t.Error("Reading through symlink loop should fail")
		}
	})

	t.Run("DeepDirectoryNesting", func(t *testing.T) {
		// Create very deep directory structure
		depth := 50
		path := strings.Repeat("dir/", depth)
		path = strings.TrimSuffix(path, "/")

		err := ctx.MkdirAll(path, 0755)
		if err != nil {
			t.Skipf("Deep nesting not supported: %v", err)
		}

		// Create file at deepest level
		filepath := path + "/deep.txt"
		err = ctx.WriteFile(filepath, []byte("deep"), 0644)
		if err != nil {
			t.Fatalf("Failed to create file in deep structure: %v", err)
		}

		ctx.AssertFileContent(filepath, []byte("deep"))
	})

	t.Run("ManyFilesInDirectory", func(t *testing.T) {
		// Create directory with many files
		dirname := "many-files"
		_ = ctx.Mkdir(dirname, 0755)

		numFiles := 100
		for i := 0; i < numFiles; i++ {
			filename := filepath.Join(dirname, fmt.Sprintf("file_%03d.txt", i))
			_ = ctx.WriteFile(filename, []byte("data"), 0644)
		}

		// List directory
		entries, err := ctx.ReadDir(dirname)
		if err != nil {
			t.Fatalf("Failed to read directory with many files: %v", err)
		}

		if len(entries) != numFiles {
			t.Errorf("Directory has %d files, want %d", len(entries), numFiles)
		}
	})

	t.Run("RenameToSameName", func(t *testing.T) {
		// Create file
		filename := "same-name.txt"
		content := []byte("rename to self")
		_ = ctx.WriteFile(filename, content, 0644)

		// Rename to same name (no-op)
		err := ctx.Rename(filename, filename)
		if err != nil {
			t.Errorf("Rename to same name failed: %v", err)
		}

		// Verify file still exists with same content
		ctx.AssertFileContent(filename, content)
	})
}
