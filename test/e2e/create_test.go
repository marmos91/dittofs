package e2e

import (
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
