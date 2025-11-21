package e2e

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

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
