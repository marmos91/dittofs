//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"golang.org/x/sys/unix"
)

// TestHardlinks tests hard link creation and behavior.
func TestHardlinks(t *testing.T) {
	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		// Basic hard link
		t.Run("CreateHardlink", func(t *testing.T) {
			originalPath := tc.Path("original.txt")
			content := []byte("This is the original file content")
			framework.WriteFile(t, originalPath, content)

			linkPath := tc.Path("hardlink.txt")
			if err := os.Link(originalPath, linkPath); err != nil {
				t.Fatalf("Failed to create hard link: %v", err)
			}

			// Verify content matches
			linkContent := framework.ReadFile(t, linkPath)
			if !bytes.Equal(linkContent, content) {
				t.Error("Hard link content mismatch")
			}
		})

		// Modify through hard link
		t.Run("ModifyThroughHardlink", func(t *testing.T) {
			originalPath := tc.Path("original_modify.txt")
			content := []byte("original content")
			framework.WriteFile(t, originalPath, content)

			linkPath := tc.Path("hardlink_modify.txt")
			if err := os.Link(originalPath, linkPath); err != nil {
				t.Fatalf("Failed to create hard link: %v", err)
			}

			// Modify through hard link
			newContent := []byte("modified through hard link")
			framework.WriteFile(t, linkPath, newContent)

			// Verify change visible through original
			originalContent := framework.ReadFile(t, originalPath)
			if !bytes.Equal(originalContent, newContent) {
				t.Error("Original file not updated through hard link")
			}
		})

		// Delete original, hard link persists
		t.Run("DeleteOriginalHardlinkPersists", func(t *testing.T) {
			originalPath := tc.Path("original_delete.txt")
			content := []byte("content")
			framework.WriteFile(t, originalPath, content)

			linkPath := tc.Path("hardlink_delete.txt")
			if err := os.Link(originalPath, linkPath); err != nil {
				t.Fatalf("Failed to create hard link: %v", err)
			}

			// Remove original
			framework.RemoveAll(t, originalPath)

			// Hard link should still exist with content
			linkContent := framework.ReadFile(t, linkPath)
			if !bytes.Equal(linkContent, content) {
				t.Error("Hard link content changed after original deleted")
			}
		})

		// Multiple hard links
		t.Run("MultipleHardlinks", func(t *testing.T) {
			originalPath := tc.Path("original_multi.txt")
			content := []byte("content")
			framework.WriteFile(t, originalPath, content)

			link1 := tc.Path("link1.txt")
			link2 := tc.Path("link2.txt")
			link3 := tc.Path("link3.txt")

			if err := os.Link(originalPath, link1); err != nil {
				t.Fatalf("Failed to create link1: %v", err)
			}
			if err := os.Link(originalPath, link2); err != nil {
				t.Fatalf("Failed to create link2: %v", err)
			}
			if err := os.Link(originalPath, link3); err != nil {
				t.Fatalf("Failed to create link3: %v", err)
			}

			// Modify through one link
			newContent := []byte("modified")
			framework.WriteFile(t, link2, newContent)

			// All should see the change
			for _, path := range []string{originalPath, link1, link2, link3} {
				data := framework.ReadFile(t, path)
				if !bytes.Equal(data, newContent) {
					t.Errorf("%s has wrong content", filepath.Base(path))
				}
			}
		})
	})
}

// TestSymlinks tests symbolic link operations.
func TestSymlinks(t *testing.T) {
	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		// Create symlink to file
		t.Run("SymlinkToFile", func(t *testing.T) {
			targetPath := tc.Path("symlink_target.txt")
			content := []byte("target content")
			framework.WriteFile(t, targetPath, content)

			linkPath := tc.Path("symlink.txt")
			if err := os.Symlink(targetPath, linkPath); err != nil {
				t.Fatalf("Failed to create symlink: %v", err)
			}

			// Read through symlink
			linkContent := framework.ReadFile(t, linkPath)
			if !bytes.Equal(linkContent, content) {
				t.Error("Symlink content mismatch")
			}

			// Verify it's a symlink
			info, err := os.Lstat(linkPath)
			if err != nil {
				t.Fatalf("Failed to lstat symlink: %v", err)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				t.Error("Expected symlink mode")
			}
		})

		// Symlink to directory
		t.Run("SymlinkToDirectory", func(t *testing.T) {
			targetDir := tc.Path("symlink_dir_target")
			framework.CreateDir(t, targetDir)

			// Create file in target dir
			targetFile := filepath.Join(targetDir, "file.txt")
			framework.WriteFile(t, targetFile, []byte("content"))

			linkPath := tc.Path("symlink_dir")
			if err := os.Symlink(targetDir, linkPath); err != nil {
				t.Fatalf("Failed to create symlink to directory: %v", err)
			}

			// Read file through symlink
			linkFile := filepath.Join(linkPath, "file.txt")
			content := framework.ReadFile(t, linkFile)
			if !bytes.Equal(content, []byte("content")) {
				t.Error("Content through symlink mismatch")
			}
		})

		// Readlink
		t.Run("Readlink", func(t *testing.T) {
			targetPath := tc.Path("readlink_target.txt")
			framework.WriteFile(t, targetPath, []byte("content"))

			linkPath := tc.Path("readlink_link.txt")
			if err := os.Symlink(targetPath, linkPath); err != nil {
				t.Fatalf("Failed to create symlink: %v", err)
			}

			target, err := os.Readlink(linkPath)
			if err != nil {
				t.Fatalf("Failed to readlink: %v", err)
			}
			if target != targetPath {
				t.Errorf("Readlink returned wrong target: got %s, want %s", target, targetPath)
			}
		})
	})
}

// TestRename tests file and directory rename operations.
func TestRename(t *testing.T) {
	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		// Rename file in same directory
		t.Run("RenameFileInPlace", func(t *testing.T) {
			oldPath := tc.Path("oldname.txt")
			content := []byte("content")
			framework.WriteFile(t, oldPath, content)

			newPath := tc.Path("newname.txt")
			if err := os.Rename(oldPath, newPath); err != nil {
				t.Fatalf("Failed to rename: %v", err)
			}

			if framework.FileExists(oldPath) {
				t.Error("Old file still exists")
			}

			newContent := framework.ReadFile(t, newPath)
			if !bytes.Equal(newContent, content) {
				t.Error("Renamed file content mismatch")
			}
		})

		// Move file to another directory
		t.Run("MoveFileToDirectory", func(t *testing.T) {
			dir1 := tc.Path("move_dir1")
			dir2 := tc.Path("move_dir2")
			framework.CreateDir(t, dir1)
			framework.CreateDir(t, dir2)

			oldPath := filepath.Join(dir1, "file.txt")
			content := []byte("content")
			framework.WriteFile(t, oldPath, content)

			newPath := filepath.Join(dir2, "file.txt")
			if err := os.Rename(oldPath, newPath); err != nil {
				t.Fatalf("Failed to move file: %v", err)
			}

			if framework.FileExists(oldPath) {
				t.Error("Old file still exists")
			}

			newContent := framework.ReadFile(t, newPath)
			if !bytes.Equal(newContent, content) {
				t.Error("Moved file content mismatch")
			}
		})

		// Rename directory
		t.Run("RenameDirectory", func(t *testing.T) {
			oldDir := tc.Path("olddir")
			framework.CreateDir(t, oldDir)

			filePath := filepath.Join(oldDir, "file.txt")
			framework.WriteFile(t, filePath, []byte("content"))

			newDir := tc.Path("newdir")
			if err := os.Rename(oldDir, newDir); err != nil {
				t.Fatalf("Failed to rename directory: %v", err)
			}

			if framework.DirExists(oldDir) {
				t.Error("Old directory still exists")
			}

			newFilePath := filepath.Join(newDir, "file.txt")
			content := framework.ReadFile(t, newFilePath)
			if !bytes.Equal(content, []byte("content")) {
				t.Error("File in renamed directory has wrong content")
			}
		})

		// Rename with overwrite
		t.Run("RenameOverwrite", func(t *testing.T) {
			sourcePath := tc.Path("source.txt")
			sourceContent := []byte("source")
			framework.WriteFile(t, sourcePath, sourceContent)

			destPath := tc.Path("dest.txt")
			framework.WriteFile(t, destPath, []byte("dest"))

			if err := os.Rename(sourcePath, destPath); err != nil {
				t.Fatalf("Failed to rename with overwrite: %v", err)
			}

			if framework.FileExists(sourcePath) {
				t.Error("Source still exists")
			}

			content := framework.ReadFile(t, destPath)
			if !bytes.Equal(content, sourceContent) {
				t.Error("Destination has wrong content after overwrite")
			}
		})
	})
}

// TestSpecialFiles tests creation of special file types (FIFOs, devices).
func TestSpecialFiles(t *testing.T) {
	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		// FIFO (named pipe)
		t.Run("CreateFIFO", func(t *testing.T) {
			fifoPath := tc.Path("testpipe")

			if err := syscall.Mkfifo(fifoPath, 0644); err != nil {
				t.Fatalf("Failed to create FIFO: %v", err)
			}

			info, err := os.Lstat(fifoPath)
			if err != nil {
				t.Fatalf("Failed to stat FIFO: %v", err)
			}

			if info.Mode()&os.ModeNamedPipe == 0 {
				t.Error("Expected named pipe mode")
			}

			if info.Size() != 0 {
				t.Errorf("Expected FIFO size 0, got %d", info.Size())
			}
		})

		// FIFO in subdirectory
		t.Run("FIFOInSubdirectory", func(t *testing.T) {
			subdir := tc.Path("fifo_subdir")
			framework.CreateDir(t, subdir)

			fifoPath := filepath.Join(subdir, "pipe")
			if err := syscall.Mkfifo(fifoPath, 0644); err != nil {
				t.Fatalf("Failed to create FIFO: %v", err)
			}

			info, err := os.Lstat(fifoPath)
			if err != nil {
				t.Fatalf("Failed to stat FIFO: %v", err)
			}

			if info.Mode()&os.ModeNamedPipe == 0 {
				t.Error("Expected named pipe mode")
			}
		})

		// Socket file (mknod)
		t.Run("CreateSocket", func(t *testing.T) {
			sockPath := tc.Path("testsock")

			err := syscall.Mknod(sockPath, syscall.S_IFSOCK|0755, 0)
			if err != nil {
				t.Skipf("Socket creation via mknod not supported: %v", err)
			}

			info, err := os.Lstat(sockPath)
			if err != nil {
				t.Fatalf("Failed to stat socket: %v", err)
			}

			if info.Mode()&os.ModeSocket == 0 {
				t.Error("Expected socket mode")
			}
		})

		// Character device (requires root, Linux only)
		t.Run("CreateCharDevice", func(t *testing.T) {
			if os.Getuid() != 0 {
				t.Skip("Requires root")
			}
			if runtime.GOOS == "darwin" {
				t.Skip("Not supported on macOS via NFS")
			}

			devPath := tc.Path("testnull")
			dev := int(unix.Mkdev(1, 3)) // /dev/null

			if err := syscall.Mknod(devPath, syscall.S_IFCHR|0666, dev); err != nil {
				t.Fatalf("Failed to create char device: %v", err)
			}

			info, err := os.Lstat(devPath)
			if err != nil {
				t.Fatalf("Failed to stat device: %v", err)
			}

			if info.Mode()&os.ModeCharDevice == 0 {
				t.Error("Expected char device mode")
			}
		})

		// Block device (requires root, Linux only)
		t.Run("CreateBlockDevice", func(t *testing.T) {
			if os.Getuid() != 0 {
				t.Skip("Requires root")
			}
			if runtime.GOOS == "darwin" {
				t.Skip("Not supported on macOS via NFS")
			}

			devPath := tc.Path("testloop")
			dev := int(unix.Mkdev(7, 0)) // /dev/loop0

			if err := syscall.Mknod(devPath, syscall.S_IFBLK|0660, dev); err != nil {
				t.Fatalf("Failed to create block device: %v", err)
			}

			info, err := os.Lstat(devPath)
			if err != nil {
				t.Fatalf("Failed to stat device: %v", err)
			}

			// Block device has ModeDevice but not ModeCharDevice
			if info.Mode()&os.ModeDevice == 0 {
				t.Error("Expected device mode")
			}
			if info.Mode()&os.ModeCharDevice != 0 {
				t.Error("Should be block device, not char device")
			}
		})
	})
}
