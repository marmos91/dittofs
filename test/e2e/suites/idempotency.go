package suites

import (
	"strconv"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/framework"
)

// TestIdempotency tests that operations can be repeated safely
func TestIdempotency(t *testing.T, storeType framework.StoreType) {
	ctx := framework.NewTestContext(t, storeType)

	t.Run("CreateSameFileTwice", func(t *testing.T) {
		// Create file
		content := []byte("content")
		err := ctx.WriteFile("idempotent.txt", content, 0644)
		if err != nil {
			t.Fatalf("First create failed: %v", err)
		}

		// Create again (overwrite)
		err = ctx.WriteFile("idempotent.txt", content, 0644)
		if err != nil {
			t.Fatalf("Second create failed: %v", err)
		}

		// Verify content is still correct
		ctx.AssertFileContent("idempotent.txt", content)
	})

	t.Run("DeleteNonExistentFile", func(t *testing.T) {
		// Try to delete file that doesn't exist
		err := ctx.Remove("nonexistent.txt")
		if err == nil {
			t.Error("Deleting non-existent file should fail")
		}
	})

	t.Run("CreateDirectoryTwice", func(t *testing.T) {
		// Create directory
		err := ctx.Mkdir("idempotent-dir", 0755)
		if err != nil {
			t.Fatalf("First mkdir failed: %v", err)
		}

		// Create again (should fail - not idempotent by design)
		err = ctx.Mkdir("idempotent-dir", 0755)
		if err == nil {
			t.Error("Second mkdir should fail")
		}

		// But MkdirAll is idempotent
		err = ctx.MkdirAll("idempotent-dir", 0755)
		if err != nil {
			t.Errorf("MkdirAll on existing directory failed: %v", err)
		}
	})

	t.Run("ReadSameFileTwice", func(t *testing.T) {
		content := []byte("read me twice")
		_ = ctx.WriteFile("readtwice.txt", content, 0644)

		// Read first time
		data1, err := ctx.ReadFile("readtwice.txt")
		if err != nil {
			t.Fatalf("First read failed: %v", err)
		}

		// Read second time
		data2, err := ctx.ReadFile("readtwice.txt")
		if err != nil {
			t.Fatalf("Second read failed: %v", err)
		}

		// Verify both reads return same content
		if string(data1) != string(data2) {
			t.Error("Multiple reads returned different content")
		}
		if string(data1) != string(content) {
			t.Error("Read content doesn't match written content")
		}
	})

	t.Run("StatSameFileTwice", func(t *testing.T) {
		_ = ctx.WriteFile("stattwice.txt", []byte("content"), 0644)

		// Stat first time
		info1, err := ctx.Stat("stattwice.txt")
		if err != nil {
			t.Fatalf("First stat failed: %v", err)
		}

		// Stat second time
		info2, err := ctx.Stat("stattwice.txt")
		if err != nil {
			t.Fatalf("Second stat failed: %v", err)
		}

		// Verify attributes are consistent
		if info1.Size() != info2.Size() {
			t.Errorf("Stat returned different sizes: %d vs %d", info1.Size(), info2.Size())
		}
		if info1.Mode() != info2.Mode() {
			t.Errorf("Stat returned different modes: %v vs %v", info1.Mode(), info2.Mode())
		}
	})

	t.Run("WriteFileMultipleTimes", func(t *testing.T) {
		// Write file multiple times with different content
		for i := 0; i < 5; i++ {
			content := []byte("iteration " + strconv.Itoa(i))
			err := ctx.WriteFile("multiwrite.txt", content, 0644)
			if err != nil {
				t.Fatalf("Write %d failed: %v", i, err)
			}

			// Verify content after each write
			ctx.AssertFileContent("multiwrite.txt", content)
		}
	})

	t.Run("RenameToExistingName", func(t *testing.T) {
		// Create two files
		_ = ctx.WriteFile("rename-src.txt", []byte("source"), 0644)
		_ = ctx.WriteFile("rename-dst.txt", []byte("destination"), 0644)

		// Rename source to destination (should replace)
		err := ctx.Rename("rename-src.txt", "rename-dst.txt")
		if err != nil {
			t.Fatalf("Rename failed: %v", err)
		}

		// Verify source is gone
		ctx.AssertFileNotExists("rename-src.txt")

		// Verify destination has source content
		ctx.AssertFileContent("rename-dst.txt", []byte("source"))
	})

	t.Run("RecreateSameSymlink", func(t *testing.T) {
		target := "target.txt"
		link := "idempotent-link.txt"

		// Create target
		_ = ctx.WriteFile(target, []byte("data"), 0644)

		// Create symlink
		err := ctx.Symlink(target, link)
		if err != nil {
			t.Fatalf("First symlink creation failed: %v", err)
		}

		// Delete and recreate symlink
		_ = ctx.Remove(link)
		err = ctx.Symlink(target, link)
		if err != nil {
			t.Fatalf("Second symlink creation failed: %v", err)
		}

		// Verify it works
		linkTarget, err := ctx.Readlink(link)
		if err != nil {
			t.Fatalf("Readlink failed: %v", err)
		}
		if linkTarget != target {
			t.Errorf("Symlink target = %q, want %q", linkTarget, target)
		}
	})

	t.Run("RecreateDeletedFile", func(t *testing.T) {
		filename := "recreate.txt"
		content1 := []byte("first version")
		content2 := []byte("second version")

		// Create file
		_ = ctx.WriteFile(filename, content1, 0644)
		ctx.AssertFileContent(filename, content1)

		// Delete file
		_ = ctx.Remove(filename)
		ctx.AssertFileNotExists(filename)

		// Recreate with different content
		_ = ctx.WriteFile(filename, content2, 0644)
		ctx.AssertFileContent(filename, content2)
	})

	t.Run("ListDirectoryMultipleTimes", func(t *testing.T) {
		// Create directory with files
		_ = ctx.Mkdir("list-dir", 0755)
		_ = ctx.WriteFile("list-dir/file1.txt", []byte("1"), 0644)
		_ = ctx.WriteFile("list-dir/file2.txt", []byte("2"), 0644)

		// List multiple times
		for i := 0; i < 3; i++ {
			entries, err := ctx.ReadDir("list-dir")
			if err != nil {
				t.Fatalf("ReadDir iteration %d failed: %v", i, err)
			}

			if len(entries) != 2 {
				t.Errorf("ReadDir iteration %d returned %d entries, want 2", i, len(entries))
			}
		}
	})
}
