package suites

import (
	"os"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/framework"
)

// TestHardLinkOperations tests hard link creation and operations
func TestHardLinkOperations(t *testing.T, storeType framework.StoreType) {
	ctx := framework.NewTestContext(t, storeType)

	t.Run("CreateHardLink", func(t *testing.T) {
		// Create original file
		content := []byte("hard link content")
		err := ctx.WriteFile("original.txt", content, 0644)
		if err != nil {
			t.Fatalf("Failed to create original file: %v", err)
		}

		// Create hard link
		err = ctx.Link("original.txt", "hardlink.txt")
		if err != nil {
			t.Fatalf("Failed to create hard link: %v", err)
		}

		// Verify both files exist and have same content
		ctx.AssertFileContent("original.txt", content)
		ctx.AssertFileContent("hardlink.txt", content)

		// Verify they have the same inode (same underlying file)
		info1, err := ctx.Stat("original.txt")
		if err != nil {
			t.Fatalf("Failed to stat original: %v", err)
		}

		info2, err := ctx.Stat("hardlink.txt")
		if err != nil {
			t.Fatalf("Failed to stat hard link: %v", err)
		}

		// Check if they're the same file (same size, mode)
		if info1.Size() != info2.Size() {
			t.Errorf("Files have different sizes: %d vs %d", info1.Size(), info2.Size())
		}
		if info1.Mode() != info2.Mode() {
			t.Errorf("Files have different modes: %v vs %v", info1.Mode(), info2.Mode())
		}
	})

	t.Run("ModifyThroughHardLink", func(t *testing.T) {
		// Create original file
		err := ctx.WriteFile("modify1.txt", []byte("initial"), 0644)
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// Create hard link
		_ = ctx.Link("modify1.txt", "modify2.txt")

		// Modify through hard link
		err = ctx.WriteFile("modify2.txt", []byte("modified"), 0644)
		if err != nil {
			t.Fatalf("Failed to modify through hard link: %v", err)
		}

		// Verify both reflect the change
		ctx.AssertFileContent("modify1.txt", []byte("modified"))
		ctx.AssertFileContent("modify2.txt", []byte("modified"))
	})

	t.Run("DeleteOriginalKeepsHardLink", func(t *testing.T) {
		// Create original and hard link
		content := []byte("persistent content")
		_ = ctx.WriteFile("deloriginal.txt", content, 0644)
		_ = ctx.Link("deloriginal.txt", "persistent.txt")

		// Delete original
		err := ctx.Remove("deloriginal.txt")
		if err != nil {
			t.Fatalf("Failed to delete original: %v", err)
		}

		// Verify hard link still exists with content
		ctx.AssertFileNotExists("deloriginal.txt")
		ctx.AssertFileContent("persistent.txt", content)
	})

	t.Run("DeleteHardLinkKeepsOriginal", func(t *testing.T) {
		// Create original and hard link
		content := []byte("keep original")
		_ = ctx.WriteFile("keeporiginal.txt", content, 0644)
		_ = ctx.Link("keeporiginal.txt", "deletehardlink.txt")

		// Delete hard link
		err := ctx.Remove("deletehardlink.txt")
		if err != nil {
			t.Fatalf("Failed to delete hard link: %v", err)
		}

		// Verify original still exists with content
		ctx.AssertFileNotExists("deletehardlink.txt")
		ctx.AssertFileContent("keeporiginal.txt", content)
	})

	t.Run("MultipleHardLinks", func(t *testing.T) {
		// Create original file
		content := []byte("shared content")
		_ = ctx.WriteFile("multi.txt", content, 0644)

		// Create multiple hard links
		_ = ctx.Link("multi.txt", "link1.txt")
		_ = ctx.Link("multi.txt", "link2.txt")
		_ = ctx.Link("multi.txt", "link3.txt")

		// Verify all have same content
		ctx.AssertFileContent("multi.txt", content)
		ctx.AssertFileContent("link1.txt", content)
		ctx.AssertFileContent("link2.txt", content)
		ctx.AssertFileContent("link3.txt", content)

		// Modify through one link
		newContent := []byte("modified shared")
		_ = ctx.WriteFile("link2.txt", newContent, 0644)

		// Verify all reflect the change
		ctx.AssertFileContent("multi.txt", newContent)
		ctx.AssertFileContent("link1.txt", newContent)
		ctx.AssertFileContent("link2.txt", newContent)
		ctx.AssertFileContent("link3.txt", newContent)
	})

	t.Run("HardLinkAcrossDirectories", func(t *testing.T) {
		// Create directories
		_ = ctx.Mkdir("dir1", 0755)
		_ = ctx.Mkdir("dir2", 0755)

		// Create file in first directory
		content := []byte("cross-directory")
		_ = ctx.WriteFile("dir1/file.txt", content, 0644)

		// Create hard link in second directory
		err := ctx.Link("dir1/file.txt", "dir2/link.txt")
		if err != nil {
			t.Fatalf("Failed to create hard link across directories: %v", err)
		}

		// Verify both exist with same content
		ctx.AssertFileContent("dir1/file.txt", content)
		ctx.AssertFileContent("dir2/link.txt", content)
	})

	t.Run("CannotHardLinkDirectory", func(t *testing.T) {
		// Create directory
		_ = ctx.Mkdir("linkdir", 0755)

		// Try to create hard link to directory (should fail)
		err := ctx.Link("linkdir", "dirlink")
		if err == nil {
			t.Error("Hard linking directory should fail")
		}
	})

	t.Run("HardLinkToNonExistent", func(t *testing.T) {
		// Try to create hard link to non-existent file
		err := ctx.Link("nonexistent.txt", "badlink.txt")
		if err == nil {
			t.Error("Hard linking to non-existent file should fail")
		}
	})

	t.Run("RenameOneHardLink", func(t *testing.T) {
		// Create original and hard link
		content := []byte("rename test")
		_ = ctx.WriteFile("rename1.txt", content, 0644)
		_ = ctx.Link("rename1.txt", "rename2.txt")

		// Rename one of them
		err := ctx.Rename("rename2.txt", "renamed.txt")
		if err != nil {
			t.Fatalf("Failed to rename hard link: %v", err)
		}

		// Verify rename succeeded
		ctx.AssertFileNotExists("rename2.txt")
		ctx.AssertFileContent("renamed.txt", content)

		// Verify original still exists with same content
		ctx.AssertFileContent("rename1.txt", content)

		// Modify through renamed link
		newContent := []byte("after rename")
		_ = ctx.WriteFile("renamed.txt", newContent, 0644)

		// Verify both reflect the change
		ctx.AssertFileContent("rename1.txt", newContent)
		ctx.AssertFileContent("renamed.txt", newContent)
	})

	t.Run("HardLinkPermissions", func(t *testing.T) {
		// Create file with specific permissions
		_ = ctx.WriteFile("perms.txt", []byte("permissions test"), 0600)

		// Get original permissions
		info1, _ := ctx.Stat("perms.txt")

		// Create hard link
		_ = ctx.Link("perms.txt", "permslink.txt")

		// Verify hard link has same permissions
		info2, _ := ctx.Stat("permslink.txt")
		if info1.Mode() != info2.Mode() {
			t.Errorf("Hard link permissions differ: %v vs %v", info1.Mode(), info2.Mode())
		}

		// Change permissions through hard link
		err := os.Chmod(ctx.Path("permslink.txt"), 0644)
		if err != nil {
			t.Fatalf("Failed to chmod through hard link: %v", err)
		}

		// Verify both reflect the change
		info1, _ = ctx.Stat("perms.txt")
		info2, _ = ctx.Stat("permslink.txt")

		// Compare permission bits (ignoring file type bits)
		perm1 := info1.Mode().Perm()
		perm2 := info2.Mode().Perm()
		if perm1 != perm2 {
			t.Errorf("Permissions differ after chmod: %v vs %v", perm1, perm2)
		}
	})
}
