package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestMoveFileInSameDirectory tests renaming a file within the same directory
func TestMoveFileInSameDirectory(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		// Create file with content
		oldPath := tc.Path("oldname.txt")
		content := []byte("File content")
		err := os.WriteFile(oldPath, content, 0644)
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// Rename file
		newPath := tc.Path("newname.txt")
		err = os.Rename(oldPath, newPath)
		if err != nil {
			t.Fatalf("Failed to rename file: %v", err)
		}

		// Verify old name doesn't exist
		_, err = os.Stat(oldPath)
		if err == nil {
			t.Errorf("Old file still exists after rename")
		}

		// Verify new name exists with content
		data, err := os.ReadFile(newPath)
		if err != nil {
			t.Fatalf("Failed to read renamed file: %v", err)
		}
		if !bytes.Equal(data, content) {
			t.Errorf("Renamed file has wrong content: got %q, want %q", data, content)
		}
	})
}

// TestMoveFileToAnotherDirectory tests moving a file to a different directory
func TestMoveFileToAnotherDirectory(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		// Create directories
		dir1 := tc.Path("dir1")
		dir2 := tc.Path("dir2")
		err := os.Mkdir(dir1, 0755)
		if err != nil {
			t.Fatalf("Failed to create dir1: %v", err)
		}
		err = os.Mkdir(dir2, 0755)
		if err != nil {
			t.Fatalf("Failed to create dir2: %v", err)
		}

		// Create file in dir1
		oldPath := filepath.Join(dir1, "file.txt")
		content := []byte("File content")
		err = os.WriteFile(oldPath, content, 0644)
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// Move to dir2
		newPath := filepath.Join(dir2, "file.txt")
		err = os.Rename(oldPath, newPath)
		if err != nil {
			t.Fatalf("Failed to move file: %v", err)
		}

		// Verify old location doesn't exist
		_, err = os.Stat(oldPath)
		if err == nil {
			t.Errorf("File still exists at old location")
		}

		// Verify new location exists with content
		data, err := os.ReadFile(newPath)
		if err != nil {
			t.Fatalf("Failed to read moved file: %v", err)
		}
		if !bytes.Equal(data, content) {
			t.Errorf("Moved file has wrong content: got %q, want %q", data, content)
		}
	})
}

// TestMoveDirectory tests moving/renaming directories
func TestMoveDirectory(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		// Create directory with files
		oldDir := tc.Path("olddir")
		err := os.Mkdir(oldDir, 0755)
		if err != nil {
			t.Fatalf("Failed to create directory: %v", err)
		}

		// Create files in directory
		file1 := filepath.Join(oldDir, "file1.txt")
		file2 := filepath.Join(oldDir, "file2.txt")
		err = os.WriteFile(file1, []byte("content1"), 0644)
		if err != nil {
			t.Fatalf("Failed to create file1: %v", err)
		}
		err = os.WriteFile(file2, []byte("content2"), 0644)
		if err != nil {
			t.Fatalf("Failed to create file2: %v", err)
		}

		// Rename directory
		newDir := tc.Path("newdir")
		err = os.Rename(oldDir, newDir)
		if err != nil {
			t.Fatalf("Failed to rename directory: %v", err)
		}

		// Verify old directory doesn't exist
		_, err = os.Stat(oldDir)
		if err == nil {
			t.Errorf("Old directory still exists after rename")
		}

		// Verify new directory exists with files
		newFile1 := filepath.Join(newDir, "file1.txt")
		newFile2 := filepath.Join(newDir, "file2.txt")

		data1, err := os.ReadFile(newFile1)
		if err != nil {
			t.Fatalf("Failed to read file1 in new directory: %v", err)
		}
		if !bytes.Equal(data1, []byte("content1")) {
			t.Errorf("file1 has wrong content in new directory")
		}

		data2, err := os.ReadFile(newFile2)
		if err != nil {
			t.Fatalf("Failed to read file2 in new directory: %v", err)
		}
		if !bytes.Equal(data2, []byte("content2")) {
			t.Errorf("file2 has wrong content in new directory")
		}
	})
}

// TestMoveReplace tests replacing an existing file with move
func TestMoveReplace(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		// Create source file
		sourcePath := tc.Path("source.txt")
		sourceContent := []byte("Source content")
		err := os.WriteFile(sourcePath, sourceContent, 0644)
		if err != nil {
			t.Fatalf("Failed to create source file: %v", err)
		}

		// Create destination file
		destPath := tc.Path("dest.txt")
		destContent := []byte("Destination content")
		err = os.WriteFile(destPath, destContent, 0644)
		if err != nil {
			t.Fatalf("Failed to create destination file: %v", err)
		}

		// Move source to destination (should replace)
		err = os.Rename(sourcePath, destPath)
		if err != nil {
			t.Fatalf("Failed to rename with replacement: %v", err)
		}

		// Verify source doesn't exist
		_, err = os.Stat(sourcePath)
		if err == nil {
			t.Errorf("Source file still exists after move")
		}

		// Verify destination has source content
		data, err := os.ReadFile(destPath)
		if err != nil {
			t.Fatalf("Failed to read destination: %v", err)
		}
		if !bytes.Equal(data, sourceContent) {
			t.Errorf("Destination has wrong content: got %q, want %q", data, sourceContent)
		}
	})
}
