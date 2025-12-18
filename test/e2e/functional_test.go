//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/framework"
)

// TestCRUD tests basic Create, Read, Update, Delete operations.
func TestCRUD(t *testing.T) {
	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		// Test create folder
		t.Run("CreateFolder", func(t *testing.T) {
			folderPath := tc.Path("testfolder")
			framework.CreateDir(t, folderPath)

			if !framework.DirExists(folderPath) {
				t.Error("Folder should exist after creation")
			}
		})

		// Test create empty file
		t.Run("CreateEmptyFile", func(t *testing.T) {
			filePath := tc.Path("empty.txt")
			framework.WriteFile(t, filePath, []byte{})

			info := framework.GetFileInfo(t, filePath)
			if info.Size != 0 {
				t.Errorf("Expected empty file, got size %d", info.Size)
			}
		})

		// Test create file with content
		t.Run("CreateFileWithContent", func(t *testing.T) {
			filePath := tc.Path("content.txt")
			content := []byte("Hello, DittoFS!")
			framework.WriteFile(t, filePath, content)

			readContent := framework.ReadFile(t, filePath)
			if !bytes.Equal(readContent, content) {
				t.Error("Content mismatch")
			}
		})

		// Test read file
		t.Run("ReadFile", func(t *testing.T) {
			filePath := tc.Path("read_test.txt")
			expected := []byte("Read test content")
			framework.WriteFile(t, filePath, expected)

			actual := framework.ReadFile(t, filePath)
			if !bytes.Equal(actual, expected) {
				t.Error("Read content mismatch")
			}
		})

		// Test update file
		t.Run("UpdateFile", func(t *testing.T) {
			filePath := tc.Path("update_test.txt")

			// Create initial file
			initialContent := []byte("initial content")
			framework.WriteFile(t, filePath, initialContent)

			// Verify initial content
			content := framework.ReadFile(t, filePath)
			if !bytes.Equal(content, initialContent) {
				t.Error("Initial content mismatch")
			}

			// Update with new content
			newContent := []byte("updated content - much longer than before")
			framework.WriteFile(t, filePath, newContent)

			// Verify new content
			content = framework.ReadFile(t, filePath)
			if !bytes.Equal(content, newContent) {
				t.Error("Updated content mismatch")
			}
		})

		// Test delete file
		t.Run("DeleteFile", func(t *testing.T) {
			filePath := tc.Path("delete_me.txt")
			framework.WriteFile(t, filePath, []byte("delete me"))

			if !framework.FileExists(filePath) {
				t.Fatal("File should exist before deletion")
			}

			if err := os.Remove(filePath); err != nil {
				t.Fatalf("Failed to delete file: %v", err)
			}

			if framework.FileExists(filePath) {
				t.Error("File should not exist after deletion")
			}
		})

		// Test delete folder
		t.Run("DeleteFolder", func(t *testing.T) {
			folderPath := tc.Path("delete_folder")
			framework.CreateDir(t, folderPath)

			if err := os.Remove(folderPath); err != nil {
				t.Fatalf("Failed to delete folder: %v", err)
			}

			if framework.DirExists(folderPath) {
				t.Error("Folder should not exist after deletion")
			}
		})
	})
}

// TestNestedFolders tests creating deeply nested folder structures.
func TestNestedFolders(t *testing.T) {
	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		basePath := tc.Path("nested")
		framework.CreateDir(t, basePath)
		currentPath := basePath

		// Create 20 nested folders
		for i := 0; i < 20; i++ {
			currentPath = filepath.Join(currentPath, fmt.Sprintf("level%d", i))
			framework.CreateDir(t, currentPath)
		}

		// Verify the deepest folder exists
		if !framework.DirExists(currentPath) {
			t.Error("Deepest folder should exist")
		}

		// Create a file in the deepest folder
		deepFile := filepath.Join(currentPath, "deep.txt")
		framework.WriteFile(t, deepFile, []byte("deep content"))

		// Verify the file
		content := framework.ReadFile(t, deepFile)
		if !bytes.Equal(content, []byte("deep content")) {
			t.Error("Deep file content mismatch")
		}
	})
}

// TestBulkOperations tests creating and deleting many files.
func TestBulkOperations(t *testing.T) {
	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		basePath := tc.Path("bulk")
		framework.CreateDir(t, basePath)

		fileCount := 20

		// Create many files
		t.Run("CreateManyFiles", func(t *testing.T) {
			for i := 0; i < fileCount; i++ {
				filePath := filepath.Join(basePath, fmt.Sprintf("file%d.txt", i))
				content := []byte(fmt.Sprintf("content %d", i))
				framework.WriteFile(t, filePath, content)
			}

			count := framework.CountFiles(t, basePath)
			if count != fileCount {
				t.Errorf("Expected %d files, got %d", fileCount, count)
			}
		})

		// Edit all files
		t.Run("EditManyFiles", func(t *testing.T) {
			for i := 0; i < fileCount; i++ {
				filePath := filepath.Join(basePath, fmt.Sprintf("file%d.txt", i))
				newContent := []byte(fmt.Sprintf("edited content %d - version 2", i))
				framework.WriteFile(t, filePath, newContent)
			}

			// Verify a few edits
			for i := 0; i < 5; i++ {
				filePath := filepath.Join(basePath, fmt.Sprintf("file%d.txt", i))
				content := framework.ReadFile(t, filePath)
				expected := []byte(fmt.Sprintf("edited content %d - version 2", i))
				if !bytes.Equal(content, expected) {
					t.Errorf("File %d content mismatch", i)
				}
			}
		})

		// Delete all files
		t.Run("DeleteManyFiles", func(t *testing.T) {
			entries := framework.ListDir(t, basePath)
			for _, name := range entries {
				filePath := filepath.Join(basePath, name)
				if err := os.Remove(filePath); err != nil {
					t.Fatalf("Failed to delete %s: %v", name, err)
				}
			}

			count := framework.CountFiles(t, basePath)
			if count != 0 {
				t.Errorf("Expected 0 files after deletion, got %d", count)
			}
		})
	})
}

// TestConcurrentAccess tests concurrent file operations.
func TestConcurrentAccess(t *testing.T) {
	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		basePath := tc.Path("concurrent")
		framework.CreateDir(t, basePath)

		// Concurrent reads
		t.Run("ConcurrentReads", func(t *testing.T) {
			filePath := filepath.Join(basePath, "shared.txt")
			content := []byte("shared content for concurrent reads")
			framework.WriteFile(t, filePath, content)

			var wg sync.WaitGroup
			errors := make(chan error, 10)

			for i := 0; i < 10; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					readContent, err := os.ReadFile(filePath)
					if err != nil {
						errors <- err
						return
					}
					if !bytes.Equal(readContent, content) {
						errors <- fmt.Errorf("content mismatch")
					}
				}()
			}

			wg.Wait()
			close(errors)

			for err := range errors {
				t.Errorf("Concurrent read error: %v", err)
			}
		})

		// Concurrent writes to different files
		t.Run("ConcurrentWritesDifferentFiles", func(t *testing.T) {
			var wg sync.WaitGroup
			errors := make(chan error, 10)

			for i := 0; i < 10; i++ {
				wg.Add(1)
				go func(idx int) {
					defer wg.Done()
					filePath := filepath.Join(basePath, fmt.Sprintf("concurrent%d.txt", idx))
					content := []byte(fmt.Sprintf("content from goroutine %d", idx))
					if err := os.WriteFile(filePath, content, 0644); err != nil {
						errors <- err
					}
				}(i)
			}

			wg.Wait()
			close(errors)

			for err := range errors {
				t.Errorf("Concurrent write error: %v", err)
			}

			// Verify all files exist with correct content
			for i := 0; i < 10; i++ {
				filePath := filepath.Join(basePath, fmt.Sprintf("concurrent%d.txt", i))
				content := framework.ReadFile(t, filePath)
				expected := []byte(fmt.Sprintf("content from goroutine %d", i))
				if !bytes.Equal(content, expected) {
					t.Errorf("File %d content mismatch", i)
				}
			}
		})
	})
}

// TestFilePermissions tests basic file permission handling.
func TestFilePermissions(t *testing.T) {
	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		// Test file mode preservation
		t.Run("FileModePreservation", func(t *testing.T) {
			filePath := tc.Path("mode_test.txt")
			framework.WriteFile(t, filePath, []byte("test"))

			// Set specific mode
			if err := os.Chmod(filePath, 0600); err != nil {
				t.Fatalf("Failed to chmod: %v", err)
			}

			info := framework.GetFileInfo(t, filePath)
			// Check that the mode includes 0600 (owner read/write)
			mode := info.Mode.Perm()
			if mode&0600 != 0600 {
				t.Errorf("Expected mode to include 0600, got %o", mode)
			}
		})

		// Test directory mode
		t.Run("DirectoryMode", func(t *testing.T) {
			dirPath := tc.Path("mode_dir")
			if err := os.Mkdir(dirPath, 0750); err != nil {
				t.Fatalf("Failed to create directory: %v", err)
			}

			info := framework.GetFileInfo(t, dirPath)
			mode := info.Mode.Perm()
			// Check basic permission bits are set
			if mode&0700 != 0700 {
				t.Errorf("Expected owner permissions to include 0700, got %o", mode)
			}
		})
	})
}

// TestListDirectory tests directory listing operations.
func TestListDirectory(t *testing.T) {
	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		basePath := tc.Path("listdir")
		framework.CreateDir(t, basePath)

		// Create mixed content
		for i := 0; i < 5; i++ {
			filePath := filepath.Join(basePath, fmt.Sprintf("file%d.txt", i))
			framework.WriteFile(t, filePath, []byte(fmt.Sprintf("content %d", i)))
		}
		for i := 0; i < 3; i++ {
			dirPath := filepath.Join(basePath, fmt.Sprintf("dir%d", i))
			framework.CreateDir(t, dirPath)
		}

		// Test listing
		entries := framework.ListDir(t, basePath)
		if len(entries) != 8 {
			t.Errorf("Expected 8 entries, got %d", len(entries))
		}

		// Test file count
		fileCount := framework.CountFiles(t, basePath)
		if fileCount != 5 {
			t.Errorf("Expected 5 files, got %d", fileCount)
		}

		// Test dir count
		dirCount := framework.CountDirs(t, basePath)
		if dirCount != 3 {
			t.Errorf("Expected 3 directories, got %d", dirCount)
		}
	})
}
