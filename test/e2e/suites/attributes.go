package suites

import (
	"os"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
)

// TestFileAttributes tests file attributes (timestamps, permissions, ownership)
func TestFileAttributes(t *testing.T, storeType framework.StoreType) {
	ctx := framework.NewTestContext(t, storeType)

	t.Run("FileSize", func(t *testing.T) {
		content := []byte("test content with known size")
		_ = ctx.WriteFile("sized.txt", content, 0644)

		info, err := ctx.Stat("sized.txt")
		if err != nil {
			t.Fatalf("Failed to stat file: %v", err)
		}

		if info.Size() != int64(len(content)) {
			t.Errorf("File size = %d, want %d", info.Size(), len(content))
		}
	})

	t.Run("FilePermissions", func(t *testing.T) {
		// Test different permission modes
		testCases := []struct {
			name string
			mode os.FileMode
		}{
			{"ReadOnly", 0444},
			{"ReadWrite", 0644},
			{"ReadWriteExecute", 0755},
			{"OwnerOnly", 0600},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				filename := "perm_" + tc.name + ".txt"
				_ = ctx.WriteFile(filename, []byte("content"), tc.mode)

				info, err := ctx.Stat(filename)
				if err != nil {
					t.Fatalf("Failed to stat file: %v", err)
				}

				// Compare permission bits (ignore type bits)
				gotPerm := info.Mode().Perm()
				wantPerm := tc.mode.Perm()

				if gotPerm != wantPerm {
					t.Errorf("File permissions = %o, want %o", gotPerm, wantPerm)
				}
			})
		}
	})

	t.Run("ChangePermissions", func(t *testing.T) {
		// Create file with initial permissions
		_ = ctx.WriteFile("chmod.txt", []byte("chmod test"), 0644)

		// Change permissions
		newMode := os.FileMode(0600)
		err := os.Chmod(ctx.Path("chmod.txt"), newMode)
		if err != nil {
			t.Fatalf("Failed to chmod: %v", err)
		}

		// Verify new permissions
		info, err := ctx.Stat("chmod.txt")
		if err != nil {
			t.Fatalf("Failed to stat file: %v", err)
		}

		if info.Mode().Perm() != newMode.Perm() {
			t.Errorf("After chmod, permissions = %o, want %o",
				info.Mode().Perm(), newMode.Perm())
		}
	})

	t.Run("ModificationTime", func(t *testing.T) {
		// Create file
		before := time.Now()
		time.Sleep(10 * time.Millisecond) // Small delay for timestamp resolution

		_ = ctx.WriteFile("mtime.txt", []byte("mtime test"), 0644)

		time.Sleep(10 * time.Millisecond)
		after := time.Now()

		// Check modification time
		info, err := ctx.Stat("mtime.txt")
		if err != nil {
			t.Fatalf("Failed to stat file: %v", err)
		}

		mtime := info.ModTime()
		if mtime.Before(before) || mtime.After(after) {
			t.Errorf("Modification time %v not between %v and %v", mtime, before, after)
		}
	})

	t.Run("ModificationTimeUpdates", func(t *testing.T) {
		// Create file
		_ = ctx.WriteFile("mtime_update.txt", []byte("initial"), 0644)

		// Get initial mtime
		info1, _ := ctx.Stat("mtime_update.txt")
		mtime1 := info1.ModTime()

		// Wait a bit to ensure timestamp difference
		time.Sleep(100 * time.Millisecond)

		// Modify file
		_ = ctx.WriteFile("mtime_update.txt", []byte("modified"), 0644)

		// Get new mtime
		info2, _ := ctx.Stat("mtime_update.txt")
		mtime2 := info2.ModTime()

		// Verify mtime was updated
		if !mtime2.After(mtime1) {
			t.Errorf("Modification time not updated: %v should be after %v", mtime2, mtime1)
		}
	})

	t.Run("DirectoryPermissions", func(t *testing.T) {
		// Create directory with specific permissions
		mode := os.FileMode(0755)
		_ = ctx.Mkdir("permdir", mode)

		info, err := ctx.Stat("permdir")
		if err != nil {
			t.Fatalf("Failed to stat directory: %v", err)
		}

		// Verify it's a directory
		if !info.IsDir() {
			t.Error("Expected directory")
		}

		// Verify permissions
		if info.Mode().Perm() != mode.Perm() {
			t.Errorf("Directory permissions = %o, want %o",
				info.Mode().Perm(), mode.Perm())
		}
	})

	t.Run("FileType", func(t *testing.T) {
		// Test regular file
		_ = ctx.WriteFile("regular.txt", []byte("regular file"), 0644)
		info, _ := ctx.Stat("regular.txt")
		if !info.Mode().IsRegular() {
			t.Error("File should be regular file")
		}
		if info.IsDir() {
			t.Error("File should not be directory")
		}

		// Test directory
		_ = ctx.Mkdir("testdir", 0755)
		info, _ = ctx.Stat("testdir")
		if !info.IsDir() {
			t.Error("Directory should be directory")
		}
		if info.Mode().IsRegular() {
			t.Error("Directory should not be regular file")
		}

		// Test symlink
		_ = ctx.Symlink("regular.txt", "symlink.txt")
		info, _ = ctx.Lstat("symlink.txt")
		if info.Mode()&os.ModeSymlink == 0 {
			t.Error("Should be symlink")
		}
	})

	t.Run("TruncateFile", func(t *testing.T) {
		// Create file with content
		content := []byte("this is a longer file content that will be truncated")
		_ = ctx.WriteFile("truncate.txt", content, 0644)

		// Verify initial size
		info, _ := ctx.Stat("truncate.txt")
		if info.Size() != int64(len(content)) {
			t.Errorf("Initial size = %d, want %d", info.Size(), len(content))
		}

		// Truncate file
		newSize := int64(10)
		err := os.Truncate(ctx.Path("truncate.txt"), newSize)
		if err != nil {
			t.Fatalf("Failed to truncate: %v", err)
		}

		// Verify new size
		info, _ = ctx.Stat("truncate.txt")
		if info.Size() != newSize {
			t.Errorf("After truncate, size = %d, want %d", info.Size(), newSize)
		}

		// Verify content
		newContent, _ := ctx.ReadFile("truncate.txt")
		if len(newContent) != int(newSize) {
			t.Errorf("Content length = %d, want %d", len(newContent), newSize)
		}
		if string(newContent) != string(content[:newSize]) {
			t.Errorf("Content = %q, want %q", newContent, content[:newSize])
		}
	})

	t.Run("ExtendFile", func(t *testing.T) {
		// Create small file
		content := []byte("small")
		_ = ctx.WriteFile("extend.txt", content, 0644)

		// Extend file
		newSize := int64(100)
		err := os.Truncate(ctx.Path("extend.txt"), newSize)
		if err != nil {
			t.Fatalf("Failed to extend: %v", err)
		}

		// Verify new size
		info, _ := ctx.Stat("extend.txt")
		if info.Size() != newSize {
			t.Errorf("After extend, size = %d, want %d", info.Size(), newSize)
		}

		// Verify content (original content + zeros)
		newContent, _ := ctx.ReadFile("extend.txt")
		if len(newContent) != int(newSize) {
			t.Errorf("Content length = %d, want %d", len(newContent), newSize)
		}

		// Check original content is preserved
		if string(newContent[:len(content)]) != string(content) {
			t.Error("Original content not preserved after extend")
		}

		// Check extended region is zeros
		for i := len(content); i < len(newContent); i++ {
			if newContent[i] != 0 {
				t.Errorf("Byte at %d = %d, want 0 (extended region should be zeros)", i, newContent[i])
				break
			}
		}
	})
}
