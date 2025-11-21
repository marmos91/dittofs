package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

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
