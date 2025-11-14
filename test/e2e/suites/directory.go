package suites

import (
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/framework"
)

// TestDirectoryOperations tests directory creation, listing, and deletion
func TestDirectoryOperations(t *testing.T, storeType framework.StoreType) {
	ctx := framework.NewTestContext(t, storeType)

	t.Run("CreateDirectory", func(t *testing.T) {
		err := ctx.Mkdir("testdir", 0755)
		if err != nil {
			t.Fatalf("Failed to create directory: %v", err)
		}
		ctx.AssertDirExists("testdir")
	})

	t.Run("CreateNestedDirectories", func(t *testing.T) {
		err := ctx.MkdirAll("nested/deep/path", 0755)
		if err != nil {
			t.Fatalf("Failed to create nested directories: %v", err)
		}
		ctx.AssertDirExists("nested")
		ctx.AssertDirExists("nested/deep")
		ctx.AssertDirExists("nested/deep/path")
	})

	t.Run("ListEmptyDirectory", func(t *testing.T) {
		err := ctx.Mkdir("emptydir", 0755)
		if err != nil {
			t.Fatalf("Failed to create directory: %v", err)
		}

		entries, err := ctx.ReadDir("emptydir")
		if err != nil {
			t.Fatalf("Failed to read directory: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("Empty directory has %d entries, want 0", len(entries))
		}
	})

	t.Run("ListDirectoryWithFiles", func(t *testing.T) {
		// Create directory with files
		err := ctx.Mkdir("filedir", 0755)
		if err != nil {
			t.Fatalf("Failed to create directory: %v", err)
		}

		files := []string{"file1.txt", "file2.txt", "file3.txt"}
		for _, file := range files {
			path := filepath.Join("filedir", file)
			err := ctx.WriteFile(path, []byte("content"), 0644)
			if err != nil {
				t.Fatalf("Failed to create file %s: %v", file, err)
			}
		}

		// List directory
		entries, err := ctx.ReadDir("filedir")
		if err != nil {
			t.Fatalf("Failed to read directory: %v", err)
		}

		if len(entries) != len(files) {
			t.Errorf("Directory has %d entries, want %d", len(entries), len(files))
		}

		// Verify all files are present
		entryNames := make(map[string]bool)
		for _, entry := range entries {
			entryNames[entry.Name()] = true
			if entry.IsDir() {
				t.Errorf("Entry %s is a directory, expected file", entry.Name())
			}
		}

		for _, file := range files {
			if !entryNames[file] {
				t.Errorf("Expected file %s not found in directory listing", file)
			}
		}
	})

	t.Run("ListMixedDirectory", func(t *testing.T) {
		// Create directory with mixed content
		err := ctx.Mkdir("mixeddir", 0755)
		if err != nil {
			t.Fatalf("Failed to create directory: %v", err)
		}

		// Add files
		_ = ctx.WriteFile("mixeddir/file.txt", []byte("data"), 0644)

		// Add subdirectories
		_ = ctx.Mkdir("mixeddir/subdir1", 0755)
		_ = ctx.Mkdir("mixeddir/subdir2", 0755)

		// List directory
		entries, err := ctx.ReadDir("mixeddir")
		if err != nil {
			t.Fatalf("Failed to read directory: %v", err)
		}

		if len(entries) != 3 {
			t.Errorf("Directory has %d entries, want 3", len(entries))
		}

		// Count files and directories
		fileCount := 0
		dirCount := 0
		for _, entry := range entries {
			if entry.IsDir() {
				dirCount++
			} else {
				fileCount++
			}
		}

		if fileCount != 1 {
			t.Errorf("Found %d files, want 1", fileCount)
		}
		if dirCount != 2 {
			t.Errorf("Found %d directories, want 2", dirCount)
		}
	})

	t.Run("DeleteEmptyDirectory", func(t *testing.T) {
		err := ctx.Mkdir("deleteme", 0755)
		if err != nil {
			t.Fatalf("Failed to create directory: %v", err)
		}
		ctx.AssertDirExists("deleteme")

		err = ctx.Remove("deleteme")
		if err != nil {
			t.Fatalf("Failed to delete directory: %v", err)
		}
		ctx.AssertFileNotExists("deleteme")
	})

	t.Run("DeleteNonEmptyDirectory", func(t *testing.T) {
		// Create directory with content
		err := ctx.Mkdir("nonempty", 0755)
		if err != nil {
			t.Fatalf("Failed to create directory: %v", err)
		}
		_ = ctx.WriteFile("nonempty/file.txt", []byte("data"), 0644)

		// Try to delete (should fail with Remove, but RemoveAll should work)
		err = ctx.Remove("nonempty")
		if err == nil {
			t.Error("Deleting non-empty directory with Remove should fail")
		}

		// Verify directory still exists
		ctx.AssertDirExists("nonempty")

		// Use RemoveAll
		err = ctx.RemoveAll("nonempty")
		if err != nil {
			t.Fatalf("Failed to delete directory with RemoveAll: %v", err)
		}
		ctx.AssertFileNotExists("nonempty")
	})

	t.Run("RenameDirectory", func(t *testing.T) {
		// Create directory with content
		err := ctx.Mkdir("oldname", 0755)
		if err != nil {
			t.Fatalf("Failed to create directory: %v", err)
		}
		_ = ctx.WriteFile("oldname/file.txt", []byte("data"), 0644)

		// Rename directory
		err = ctx.Rename("oldname", "newname")
		if err != nil {
			t.Fatalf("Failed to rename directory: %v", err)
		}

		// Verify old name doesn't exist
		ctx.AssertFileNotExists("oldname")

		// Verify new name exists with content
		ctx.AssertDirExists("newname")
		ctx.AssertFileContent("newname/file.txt", []byte("data"))
	})

	t.Run("MoveFileToDirectory", func(t *testing.T) {
		// Create file and directory
		_ = ctx.WriteFile("moveme.txt", []byte("move this"), 0644)
		_ = ctx.Mkdir("destdir", 0755)

		// Move file into directory
		err := ctx.Rename("moveme.txt", "destdir/moveme.txt")
		if err != nil {
			t.Fatalf("Failed to move file: %v", err)
		}

		// Verify move
		ctx.AssertFileNotExists("moveme.txt")
		ctx.AssertFileContent("destdir/moveme.txt", []byte("move this"))
	})

	t.Run("DeepNesting", func(t *testing.T) {
		// Create deeply nested structure
		deepPath := "level1/level2/level3/level4/level5"
		err := ctx.MkdirAll(deepPath, 0755)
		if err != nil {
			t.Fatalf("Failed to create deep structure: %v", err)
		}

		// Create file at deepest level
		filePath := filepath.Join(deepPath, "deep.txt")
		err = ctx.WriteFile(filePath, []byte("deep content"), 0644)
		if err != nil {
			t.Fatalf("Failed to create file in deep structure: %v", err)
		}

		ctx.AssertFileContent(filePath, []byte("deep content"))
	})
}
