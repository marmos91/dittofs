//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestSMBCreateFolder tests creating a single folder via SMB
func TestSMBCreateFolder(t *testing.T) {
	runSMBOnLocalConfigs(t, func(t *testing.T, tc *SMBTestContext) {
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

// TestSMBCreateNestedFolders tests creating 20 nested folders via SMB
func TestSMBCreateNestedFolders(t *testing.T) {
	runSMBOnLocalConfigs(t, func(t *testing.T, tc *SMBTestContext) {
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

// TestSMBCreateEmptyFile tests creating a single empty file via SMB
func TestSMBCreateEmptyFile(t *testing.T) {
	runSMBOnLocalConfigs(t, func(t *testing.T, tc *SMBTestContext) {
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

// TestSMBCreateFileWithContent tests creating a file with content via SMB
func TestSMBCreateFileWithContent(t *testing.T) {
	runSMBOnLocalConfigs(t, func(t *testing.T, tc *SMBTestContext) {
		filePath := tc.Path("content.txt")
		content := []byte("Hello from SMB test!")

		err := os.WriteFile(filePath, content, 0644)
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// Read back and verify
		readContent, err := os.ReadFile(filePath)
		if err != nil {
			t.Fatalf("Failed to read file: %v", err)
		}

		if string(readContent) != string(content) {
			t.Errorf("Content mismatch: expected %q, got %q", string(content), string(readContent))
		}
	})
}

// TestSMBCreateEmptyFilesInNestedFolders tests creating 20 empty files in nested folders via SMB
func TestSMBCreateEmptyFilesInNestedFolders(t *testing.T) {
	runSMBOnLocalConfigs(t, func(t *testing.T, tc *SMBTestContext) {
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
