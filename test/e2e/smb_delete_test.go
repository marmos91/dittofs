//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestSMBDeleteSingleFile tests deleting a single file via SMB
func TestSMBDeleteSingleFile(t *testing.T) {
	runSMBOnAllConfigs(t, func(t *testing.T, tc *SMBTestContext) {
		filePath := tc.Path("delete_me.txt")

		// Create file
		err := os.WriteFile(filePath, []byte("To be deleted"), 0644)
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// Verify file exists
		_, err = os.Stat(filePath)
		if err != nil {
			t.Fatalf("File should exist before delete: %v", err)
		}

		// Delete file
		err = os.Remove(filePath)
		if err != nil {
			t.Fatalf("Failed to delete file: %v", err)
		}

		// Verify file is gone
		_, err = os.Stat(filePath)
		if err == nil {
			t.Error("File should not exist after delete")
		}
	})
}

// TestSMBDeleteAllFiles tests creating and deleting multiple files via SMB
func TestSMBDeleteAllFiles(t *testing.T) {
	runSMBOnAllConfigs(t, func(t *testing.T, tc *SMBTestContext) {
		basePath := tc.Path("delete_files")

		// Create folder
		err := os.Mkdir(basePath, 0755)
		if err != nil {
			t.Fatalf("Failed to create folder: %v", err)
		}

		// Create 20 files
		for i := 0; i < 20; i++ {
			filePath := filepath.Join(basePath, fmt.Sprintf("file%d.txt", i))
			err := os.WriteFile(filePath, []byte(fmt.Sprintf("Content %d", i)), 0644)
			if err != nil {
				t.Fatalf("Failed to create file %d: %v", i, err)
			}
		}

		// Verify files exist
		entries, err := os.ReadDir(basePath)
		if err != nil {
			t.Fatalf("Failed to read directory: %v", err)
		}
		if len(entries) != 20 {
			t.Errorf("Expected 20 files, got %d", len(entries))
		}

		// Delete all files
		for i := 0; i < 20; i++ {
			filePath := filepath.Join(basePath, fmt.Sprintf("file%d.txt", i))
			err := os.Remove(filePath)
			if err != nil {
				t.Fatalf("Failed to delete file %d: %v", i, err)
			}
		}

		// Verify all files are gone
		entries, err = os.ReadDir(basePath)
		if err != nil {
			t.Fatalf("Failed to read directory: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("Expected 0 files after delete, got %d", len(entries))
		}
	})
}

// TestSMBDeleteAllFolders tests creating and deleting multiple folders via SMB
func TestSMBDeleteAllFolders(t *testing.T) {
	runSMBOnAllConfigs(t, func(t *testing.T, tc *SMBTestContext) {
		basePath := tc.Path("delete_folders")

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

		// Verify folders exist
		entries, err := os.ReadDir(basePath)
		if err != nil {
			t.Fatalf("Failed to read directory: %v", err)
		}
		if len(entries) != 10 {
			t.Errorf("Expected 10 folders, got %d", len(entries))
		}

		// Delete all folders (in reverse order to avoid issues with nested)
		for i := 9; i >= 0; i-- {
			folderPath := filepath.Join(basePath, fmt.Sprintf("folder%d", i))
			err := os.Remove(folderPath)
			if err != nil {
				t.Fatalf("Failed to delete folder %d: %v", i, err)
			}
		}

		// Verify all folders are gone
		entries, err = os.ReadDir(basePath)
		if err != nil {
			t.Fatalf("Failed to read directory: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("Expected 0 folders after delete, got %d", len(entries))
		}
	})
}

// TestSMBDeleteNestedFolder tests deleting a nested folder structure via SMB
func TestSMBDeleteNestedFolder(t *testing.T) {
	runSMBOnAllConfigs(t, func(t *testing.T, tc *SMBTestContext) {
		basePath := tc.Path("nested_delete")

		// Create nested structure
		currentPath := basePath
		for i := 0; i < 5; i++ {
			currentPath = filepath.Join(currentPath, fmt.Sprintf("level%d", i))
			err := os.MkdirAll(currentPath, 0755)
			if err != nil {
				t.Fatalf("Failed to create nested folder: %v", err)
			}
		}

		// Delete entire tree
		err := os.RemoveAll(basePath)
		if err != nil {
			t.Fatalf("Failed to remove nested folder: %v", err)
		}

		// Verify tree is gone
		_, err = os.Stat(basePath)
		if err == nil {
			t.Error("Nested folder tree should not exist after delete")
		}
	})
}
