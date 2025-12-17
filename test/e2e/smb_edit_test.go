//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestSMBEditFile tests editing an existing file via SMB
func TestSMBEditFile(t *testing.T) {
	runSMBOnAllConfigs(t, func(t *testing.T, tc *SMBTestContext) {
		filePath := tc.Path("edit_test.txt")

		// Create initial file
		initialContent := []byte("Initial content")
		err := os.WriteFile(filePath, initialContent, 0644)
		if err != nil {
			t.Fatalf("Failed to create initial file: %v", err)
		}

		// Overwrite with new content
		newContent := []byte("Updated content via SMB")
		err = os.WriteFile(filePath, newContent, 0644)
		if err != nil {
			t.Fatalf("Failed to overwrite file: %v", err)
		}

		// Verify new content
		readContent, err := os.ReadFile(filePath)
		if err != nil {
			t.Fatalf("Failed to read file: %v", err)
		}

		if !bytes.Equal(readContent, newContent) {
			t.Errorf("Content mismatch after edit: expected %q, got %q", newContent, readContent)
		}
	})
}

// TestSMBEditMultipleFiles tests editing multiple files via SMB
func TestSMBEditMultipleFiles(t *testing.T) {
	runSMBOnAllConfigs(t, func(t *testing.T, tc *SMBTestContext) {
		basePath := tc.Path("edit_multi")

		// Create folder
		err := os.Mkdir(basePath, 0755)
		if err != nil {
			t.Fatalf("Failed to create folder: %v", err)
		}

		// Create 20 files with initial content
		for i := 0; i < 20; i++ {
			filePath := filepath.Join(basePath, fmt.Sprintf("file%d.txt", i))
			content := []byte(fmt.Sprintf("Initial content %d", i))
			err := os.WriteFile(filePath, content, 0644)
			if err != nil {
				t.Fatalf("Failed to create file %d: %v", i, err)
			}
		}

		// Edit all files
		for i := 0; i < 20; i++ {
			filePath := filepath.Join(basePath, fmt.Sprintf("file%d.txt", i))
			content := []byte(fmt.Sprintf("Updated content %d via SMB", i))
			err := os.WriteFile(filePath, content, 0644)
			if err != nil {
				t.Fatalf("Failed to edit file %d: %v", i, err)
			}
		}

		// Verify all files have new content
		for i := 0; i < 20; i++ {
			filePath := filepath.Join(basePath, fmt.Sprintf("file%d.txt", i))
			expectedContent := []byte(fmt.Sprintf("Updated content %d via SMB", i))
			readContent, err := os.ReadFile(filePath)
			if err != nil {
				t.Fatalf("Failed to read file %d: %v", i, err)
			}

			if !bytes.Equal(readContent, expectedContent) {
				t.Errorf("File %d content mismatch: expected %q, got %q", i, expectedContent, readContent)
			}
		}
	})
}

// TestSMBAppendFile tests appending to a file via SMB
func TestSMBAppendFile(t *testing.T) {
	runSMBOnAllConfigs(t, func(t *testing.T, tc *SMBTestContext) {
		filePath := tc.Path("append_test.txt")

		// Create initial file
		initialContent := []byte("Initial line\n")
		err := os.WriteFile(filePath, initialContent, 0644)
		if err != nil {
			t.Fatalf("Failed to create initial file: %v", err)
		}

		// Open for append
		f, err := os.OpenFile(filePath, os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			t.Fatalf("Failed to open file for append: %v", err)
		}

		_, err = f.WriteString("Appended line via SMB\n")
		if err != nil {
			f.Close()
			t.Fatalf("Failed to append: %v", err)
		}
		f.Close()

		// Verify content
		readContent, err := os.ReadFile(filePath)
		if err != nil {
			t.Fatalf("Failed to read file: %v", err)
		}

		expectedContent := "Initial line\nAppended line via SMB\n"
		if string(readContent) != expectedContent {
			t.Errorf("Content mismatch: expected %q, got %q", expectedContent, string(readContent))
		}
	})
}
