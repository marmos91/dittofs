package e2e

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestCreateFolder tests creating a single folder
func TestCreateFolder(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		folderPath := tc.Path("testfolder")

		err := os.Mkdir(folderPath, 0755)
		if err != nil {
			t.Fatalf("Failed to create folder: %v", err)
		}

		// Verify folder exists
		info, err := os.Stat(folderPath)
		if err != nil {
			t.Fatalf("Failed to stat folder: %v", err)
		}

		if !info.IsDir() {
			t.Errorf("Expected directory, got file")
		}
	})
}

// TestCreateNestedFolders tests creating 20 nested folders
func TestCreateNestedFolders(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		basePath := tc.MountPath
		currentPath := basePath

		// Create 20 nested folders
		for i := 0; i < 20; i++ {
			currentPath = filepath.Join(currentPath, fmt.Sprintf("nested%d", i))
			err := os.Mkdir(currentPath, 0755)
			if err != nil {
				t.Fatalf("Failed to create nested folder %d: %v", i, err)
			}
		}

		// Verify the deepest folder exists
		info, err := os.Stat(currentPath)
		if err != nil {
			t.Fatalf("Failed to stat deepest folder: %v", err)
		}

		if !info.IsDir() {
			t.Errorf("Expected directory at deepest level")
		}
	})
}

// TestCreateEmptyFile tests creating a single empty file
func TestCreateEmptyFile(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		filePath := tc.Path("empty.txt")

		err := os.WriteFile(filePath, []byte{}, 0644)
		if err != nil {
			t.Fatalf("Failed to create empty file: %v", err)
		}

		// Verify file exists and is empty
		info, err := os.Stat(filePath)
		if err != nil {
			t.Fatalf("Failed to stat file: %v", err)
		}

		if info.Size() != 0 {
			t.Errorf("Expected empty file, got size %d", info.Size())
		}
	})
}

// TestCreateEmptyFilesInNestedFolders tests creating 20 empty files in nested folders
func TestCreateEmptyFilesInNestedFolders(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		basePath := tc.Path("nested_files")

		// Create base folder
		err := os.Mkdir(basePath, 0755)
		if err != nil {
			t.Fatalf("Failed to create base folder: %v", err)
		}

		// Create 20 nested folders, each with an empty file
		currentPath := basePath
		for i := 0; i < 20; i++ {
			currentPath = filepath.Join(currentPath, fmt.Sprintf("level%d", i))

			// Create folder
			err := os.Mkdir(currentPath, 0755)
			if err != nil {
				t.Fatalf("Failed to create folder at level %d: %v", i, err)
			}

			// Create empty file in this folder
			filePath := filepath.Join(currentPath, fmt.Sprintf("file%d.txt", i))
			err = os.WriteFile(filePath, []byte{}, 0644)
			if err != nil {
				t.Fatalf("Failed to create file at level %d: %v", i, err)
			}
		}

		// Verify some files exist
		testFile := filepath.Join(basePath, "level0", "file0.txt")
		info, err := os.Stat(testFile)
		if err != nil {
			t.Fatalf("Failed to stat test file: %v", err)
		}

		if info.Size() != 0 {
			t.Errorf("Expected empty file")
		}
	})
}

// TestDeleteSingleFile tests deleting a single file
func TestDeleteSingleFile(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		filePath := tc.Path("delete_me.txt")

		// Create file
		err := os.WriteFile(filePath, []byte("delete me"), 0644)
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// Verify file exists
		_, err = os.Stat(filePath)
		if err != nil {
			t.Fatalf("File should exist before deletion: %v", err)
		}

		// Delete file
		err = os.Remove(filePath)
		if err != nil {
			t.Fatalf("Failed to delete file: %v", err)
		}

		// Verify file is deleted
		_, err = os.Stat(filePath)
		if !os.IsNotExist(err) {
			t.Errorf("File should not exist after deletion")
		}
	})
}

// TestDeleteAllFolders tests deleting all folders
func TestDeleteAllFolders(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		basePath := tc.Path("folders_to_delete")

		// Create base folder
		err := os.Mkdir(basePath, 0755)
		if err != nil {
			t.Fatalf("Failed to create base folder: %v", err)
		}

		// Create 10 folders
		for i := 0; i < 10; i++ {
			folderPath := filepath.Join(basePath, fmt.Sprintf("folder%d", i))
			err := os.Mkdir(folderPath, 0755)
			if err != nil {
				t.Fatalf("Failed to create folder %d: %v", i, err)
			}
		}

		// Delete all folders
		err = os.RemoveAll(basePath)
		if err != nil {
			t.Fatalf("Failed to delete folders: %v", err)
		}

		// Verify base folder is deleted
		_, err = os.Stat(basePath)
		if !os.IsNotExist(err) {
			t.Errorf("Folders should not exist after deletion")
		}
	})
}

// TestDeleteAllFiles tests deleting all files in a folder
func TestDeleteAllFiles(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		basePath := tc.Path("files_to_delete")

		// Create base folder
		err := os.Mkdir(basePath, 0755)
		if err != nil {
			t.Fatalf("Failed to create base folder: %v", err)
		}

		// Create 20 files
		for i := 0; i < 20; i++ {
			filePath := filepath.Join(basePath, fmt.Sprintf("file%d.txt", i))
			err := os.WriteFile(filePath, []byte(fmt.Sprintf("content %d", i)), 0644)
			if err != nil {
				t.Fatalf("Failed to create file %d: %v", i, err)
			}
		}

		// Read directory
		entries, err := os.ReadDir(basePath)
		if err != nil {
			t.Fatalf("Failed to read directory: %v", err)
		}

		// Delete all files
		for _, entry := range entries {
			if !entry.IsDir() {
				filePath := filepath.Join(basePath, entry.Name())
				err := os.Remove(filePath)
				if err != nil {
					t.Fatalf("Failed to delete file %s: %v", entry.Name(), err)
				}
			}
		}

		// Verify all files are deleted
		entries, err = os.ReadDir(basePath)
		if err != nil {
			t.Fatalf("Failed to read directory after deletion: %v", err)
		}

		if len(entries) != 0 {
			t.Errorf("Expected 0 files, got %d", len(entries))
		}
	})
}

// TestEditFile tests editing a single file
func TestEditFile(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		filePath := tc.Path("edit_me.txt")

		// Create file with initial content
		initialContent := []byte("initial content")
		err := os.WriteFile(filePath, initialContent, 0644)
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// Verify initial content
		content, err := os.ReadFile(filePath)
		if err != nil {
			t.Fatalf("Failed to read file: %v", err)
		}
		if !bytes.Equal(content, initialContent) {
			t.Errorf("Initial content mismatch")
		}

		// Edit file with new content
		newContent := []byte("edited content - much longer than before")
		err = os.WriteFile(filePath, newContent, 0644)
		if err != nil {
			t.Fatalf("Failed to edit file: %v", err)
		}

		// Verify new content
		content, err = os.ReadFile(filePath)
		if err != nil {
			t.Fatalf("Failed to read file after edit: %v", err)
		}
		if !bytes.Equal(content, newContent) {
			t.Errorf("Edited content mismatch")
		}
	})
}

// TestEditMultipleFiles tests editing multiple files
func TestEditMultipleFiles(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		basePath := tc.Path("files_to_edit")

		// Create base folder
		err := os.Mkdir(basePath, 0755)
		if err != nil {
			t.Fatalf("Failed to create base folder: %v", err)
		}

		// Create 20 files
		for i := 0; i < 20; i++ {
			filePath := filepath.Join(basePath, fmt.Sprintf("file%d.txt", i))
			content := []byte(fmt.Sprintf("initial content %d", i))
			err := os.WriteFile(filePath, content, 0644)
			if err != nil {
				t.Fatalf("Failed to create file %d: %v", i, err)
			}
		}

		// Edit all files
		for i := 0; i < 20; i++ {
			filePath := filepath.Join(basePath, fmt.Sprintf("file%d.txt", i))
			newContent := []byte(fmt.Sprintf("edited content %d - this is the new version", i))
			err := os.WriteFile(filePath, newContent, 0644)
			if err != nil {
				t.Fatalf("Failed to edit file %d: %v", i, err)
			}
		}

		// Verify some edits
		for i := 0; i < 5; i++ {
			filePath := filepath.Join(basePath, fmt.Sprintf("file%d.txt", i))
			content, err := os.ReadFile(filePath)
			if err != nil {
				t.Fatalf("Failed to read file %d: %v", i, err)
			}

			expected := []byte(fmt.Sprintf("edited content %d - this is the new version", i))
			if !bytes.Equal(content, expected) {
				t.Errorf("File %d content mismatch", i)
			}
		}
	})
}

// TestUnmount tests unmounting the share
func TestUnmount(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		// Create a file before unmount
		filePath := tc.Path("before_unmount.txt")
		err := os.WriteFile(filePath, []byte("test"), 0644)
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// Unmount
		tc.unmountNFS()

		// Try to access the file (should fail)
		_, err = os.Stat(filePath)
		if err == nil {
			t.Errorf("Should not be able to access file after unmount")
		}

		// Remount for cleanup
		tc.mountNFS()
	})
}

// runOnAllConfigs is a helper that runs a test on all configurations
func runOnAllConfigs(t *testing.T, testFunc func(t *testing.T, tc *TestContext)) {
	t.Helper()

	configs := AllConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewTestContext(t, config)
			defer tc.Cleanup()

			testFunc(t, tc)
		})
	}
}
