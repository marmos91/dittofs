package suites

import (
	"testing"

	"github.com/marmos91/dittofs/test/e2e/framework"
)

// TestSymlinkOperations tests symbolic link creation and operations
func TestSymlinkOperations(t *testing.T, storeType framework.StoreType) {
	ctx := framework.NewTestContext(t, storeType)

	t.Run("CreateSymlinkToFile", func(t *testing.T) {
		// Create target file
		err := ctx.WriteFile("target.txt", []byte("target content"), 0644)
		if err != nil {
			t.Fatalf("Failed to create target file: %v", err)
		}

		// Create symlink
		err = ctx.Symlink("target.txt", "link.txt")
		if err != nil {
			t.Fatalf("Failed to create symlink: %v", err)
		}

		ctx.AssertIsSymlink("link.txt")

		// Verify symlink target
		target, err := ctx.Readlink("link.txt")
		if err != nil {
			t.Fatalf("Failed to read symlink: %v", err)
		}
		if target != "target.txt" {
			t.Errorf("Symlink target = %q, want %q", target, "target.txt")
		}

		// Verify can read through symlink
		ctx.AssertFileContent("link.txt", []byte("target content"))
	})

	t.Run("CreateSymlinkToDirectory", func(t *testing.T) {
		// Create target directory
		err := ctx.Mkdir("targetdir", 0755)
		if err != nil {
			t.Fatalf("Failed to create directory: %v", err)
		}
		_ = ctx.WriteFile("targetdir/file.txt", []byte("data"), 0644)

		// Create symlink to directory
		err = ctx.Symlink("targetdir", "linkdir")
		if err != nil {
			t.Fatalf("Failed to create symlink to directory: %v", err)
		}

		ctx.AssertIsSymlink("linkdir")

		// Verify can access directory through symlink
		entries, err := ctx.ReadDir("linkdir")
		if err != nil {
			t.Fatalf("Failed to read through symlink: %v", err)
		}
		if len(entries) != 1 {
			t.Errorf("Directory through symlink has %d entries, want 1", len(entries))
		}

		// Verify can read file through symlink
		ctx.AssertFileContent("linkdir/file.txt", []byte("data"))
	})

	t.Run("CreateAbsoluteSymlink", func(t *testing.T) {
		// Create target file
		_ = ctx.WriteFile("abstarget.txt", []byte("absolute"), 0644)

		// Create absolute symlink
		absTarget := ctx.Path("abstarget.txt")
		err := ctx.Symlink(absTarget, "abslink.txt")
		if err != nil {
			t.Fatalf("Failed to create absolute symlink: %v", err)
		}

		ctx.AssertIsSymlink("abslink.txt")

		// Verify symlink target
		target, err := ctx.Readlink("abslink.txt")
		if err != nil {
			t.Fatalf("Failed to read symlink: %v", err)
		}
		if target != absTarget {
			t.Errorf("Symlink target = %q, want %q", target, absTarget)
		}
	})

	t.Run("CreateDanglingSymlink", func(t *testing.T) {
		// Create symlink to non-existent target
		err := ctx.Symlink("nonexistent.txt", "dangling.txt")
		if err != nil {
			t.Fatalf("Failed to create dangling symlink: %v", err)
		}

		ctx.AssertIsSymlink("dangling.txt")

		// Verify symlink exists but target doesn't
		target, err := ctx.Readlink("dangling.txt")
		if err != nil {
			t.Fatalf("Failed to read dangling symlink: %v", err)
		}
		if target != "nonexistent.txt" {
			t.Errorf("Dangling symlink target = %q, want %q", target, "nonexistent.txt")
		}

		// Verify accessing through symlink fails
		_, err = ctx.ReadFile("dangling.txt")
		if err == nil {
			t.Error("Reading through dangling symlink should fail")
		}
	})

	t.Run("DeleteSymlink", func(t *testing.T) {
		// Create target and symlink
		_ = ctx.WriteFile("deletestarget.txt", []byte("data"), 0644)
		_ = ctx.Symlink("deletestarget.txt", "deleteslink.txt")
		ctx.AssertIsSymlink("deleteslink.txt")

		// Delete symlink
		err := ctx.Remove("deleteslink.txt")
		if err != nil {
			t.Fatalf("Failed to delete symlink: %v", err)
		}

		// Verify symlink is gone but target remains
		ctx.AssertFileNotExists("deleteslink.txt")
		ctx.AssertFileExists("deletestarget.txt")
	})

	t.Run("RenameSymlink", func(t *testing.T) {
		// Create target and symlink
		_ = ctx.WriteFile("renametarget.txt", []byte("data"), 0644)
		_ = ctx.Symlink("renametarget.txt", "oldlink.txt")

		// Rename symlink
		err := ctx.Rename("oldlink.txt", "newlink.txt")
		if err != nil {
			t.Fatalf("Failed to rename symlink: %v", err)
		}

		// Verify old link is gone, new link exists
		ctx.AssertFileNotExists("oldlink.txt")
		ctx.AssertIsSymlink("newlink.txt")

		// Verify new link still points to correct target
		target, err := ctx.Readlink("newlink.txt")
		if err != nil {
			t.Fatalf("Failed to read renamed symlink: %v", err)
		}
		if target != "renametarget.txt" {
			t.Errorf("Renamed symlink target = %q, want %q", target, "renametarget.txt")
		}
	})

	t.Run("OverwriteSymlink", func(t *testing.T) {
		// Create first target and symlink
		_ = ctx.WriteFile("target1.txt", []byte("first"), 0644)
		_ = ctx.Symlink("target1.txt", "overlink.txt")

		// Create second target
		_ = ctx.WriteFile("target2.txt", []byte("second"), 0644)

		// Remove and recreate symlink to new target
		_ = ctx.Remove("overlink.txt")
		err := ctx.Symlink("target2.txt", "overlink.txt")
		if err != nil {
			t.Fatalf("Failed to overwrite symlink: %v", err)
		}

		// Verify symlink points to new target
		target, err := ctx.Readlink("overlink.txt")
		if err != nil {
			t.Fatalf("Failed to read symlink: %v", err)
		}
		if target != "target2.txt" {
			t.Errorf("Symlink target = %q, want %q", target, "target2.txt")
		}

		ctx.AssertFileContent("overlink.txt", []byte("second"))
	})

	t.Run("ChainedSymlinks", func(t *testing.T) {
		// Create chain: target.txt <- link1 <- link2 <- link3
		_ = ctx.WriteFile("chaintarget.txt", []byte("chain content"), 0644)
		_ = ctx.Symlink("chaintarget.txt", "chainlink1.txt")
		_ = ctx.Symlink("chainlink1.txt", "chainlink2.txt")
		_ = ctx.Symlink("chainlink2.txt", "chainlink3.txt")

		// Verify final link can access content
		ctx.AssertFileContent("chainlink3.txt", []byte("chain content"))

		// Verify each link in chain
		target1, _ := ctx.Readlink("chainlink1.txt")
		if target1 != "chaintarget.txt" {
			t.Errorf("chainlink1 target = %q, want %q", target1, "chaintarget.txt")
		}

		target2, _ := ctx.Readlink("chainlink2.txt")
		if target2 != "chainlink1.txt" {
			t.Errorf("chainlink2 target = %q, want %q", target2, "chainlink1.txt")
		}

		target3, _ := ctx.Readlink("chainlink3.txt")
		if target3 != "chainlink2.txt" {
			t.Errorf("chainlink3 target = %q, want %q", target3, "chainlink2.txt")
		}
	})

	t.Run("SymlinkWithRelativePath", func(t *testing.T) {
		// Create nested structure
		_ = ctx.MkdirAll("dir1/subdir", 0755)
		_ = ctx.WriteFile("dir1/file.txt", []byte("relative content"), 0644)

		// Create symlink with relative path
		_ = ctx.Symlink("../file.txt", "dir1/subdir/link.txt")

		// Verify symlink target
		target, err := ctx.Readlink("dir1/subdir/link.txt")
		if err != nil {
			t.Fatalf("Failed to read symlink: %v", err)
		}
		if target != "../file.txt" {
			t.Errorf("Symlink target = %q, want %q", target, "../file.txt")
		}

		// Verify can access through symlink
		ctx.AssertFileContent("dir1/subdir/link.txt", []byte("relative content"))
	})
}
