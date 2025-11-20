package e2e

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// FileSize represents a test file size
type FileSize struct {
	Name  string
	Bytes int64
}

var (
	// Standard file sizes for testing
	Size500KB = FileSize{Name: "500KB", Bytes: 500 * 1024}
	Size1MB   = FileSize{Name: "1MB", Bytes: 1 * 1024 * 1024}
	Size10MB  = FileSize{Name: "10MB", Bytes: 10 * 1024 * 1024}
	Size100MB = FileSize{Name: "100MB", Bytes: 100 * 1024 * 1024}
	Size1GB   = FileSize{Name: "1GB", Bytes: 1 * 1024 * 1024 * 1024}

	// All standard sizes
	StandardFileSizes = []FileSize{Size500KB, Size1MB, Size10MB, Size100MB}
)

// TestCreateFilesBySize tests creating files of specific sizes
func TestCreateFilesBySize(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		for _, size := range StandardFileSizes {
			t.Run(size.Name, func(t *testing.T) {
				testCreateFileOfSize(t, tc, size)
			})
		}
	})
}

// TestCreateFile_500KB tests creating a 500KB file
func TestCreateFile_500KB(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		testCreateFileOfSize(t, tc, Size500KB)
	})
}

// TestCreateFile_1MB tests creating a 1MB file
func TestCreateFile_1MB(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		testCreateFileOfSize(t, tc, Size1MB)
	})
}

// TestCreateFile_10MB tests creating a 10MB file
func TestCreateFile_10MB(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		testCreateFileOfSize(t, tc, Size10MB)
	})
}

// TestCreateFile_100MB tests creating a 100MB file
func TestCreateFile_100MB(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		testCreateFileOfSize(t, tc, Size100MB)
	})
}

// TestCreateMultipleFilesBySize tests creating multiple files of each size
// Smaller files (500KB, 1MB): 20 files
// Medium files (10MB): 5 files
// Large files (100MB): 3 files
func TestCreateMultipleFilesBySize(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		for _, size := range StandardFileSizes {
			t.Run(size.Name, func(t *testing.T) {
				count := 20
				// Reduce count for large files to keep test times reasonable
				if size.Bytes >= 100*1024*1024 { // 100MB
					count = 3
				} else if size.Bytes >= 10*1024*1024 { // 10MB or larger
					count = 5
				}
				testCreateMultipleFilesOfSize(t, tc, size, count)
			})
		}
	})
}

// TestReadFilesBySize tests reading files of different sizes
// Note: Skips 100MB to keep test times reasonable
func TestReadFilesBySize(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		for _, size := range []FileSize{Size500KB, Size1MB, Size10MB} {
			t.Run(size.Name, func(t *testing.T) {
				testReadFileOfSize(t, tc, size)
			})
		}
	})
}

// TestWriteThenReadBySize tests writing and reading files of different sizes
// Note: Skips 100MB to keep test times reasonable
func TestWriteThenReadBySize(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		for _, size := range []FileSize{Size500KB, Size1MB, Size10MB} {
			t.Run(size.Name, func(t *testing.T) {
				testWriteThenReadFileOfSize(t, tc, size)
			})
		}
	})
}

// TestOverwriteFilesBySize tests overwriting files of different sizes
// Note: Skips 100MB to keep test times reasonable
func TestOverwriteFilesBySize(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		for _, size := range []FileSize{Size500KB, Size1MB, Size10MB} {
			t.Run(size.Name, func(t *testing.T) {
				testOverwriteFileOfSize(t, tc, size)
			})
		}
	})
}

// TestDeleteFilesBySize tests deleting files of different sizes
// Note: Skips 100MB to keep test times reasonable
func TestDeleteFilesBySize(t *testing.T) {
	runOnAllConfigs(t, func(t *testing.T, tc *TestContext) {
		for _, size := range []FileSize{Size500KB, Size1MB, Size10MB} {
			t.Run(size.Name, func(t *testing.T) {
				testDeleteFileOfSize(t, tc, size)
			})
		}
	})
}

// ============================================================================
// Helper functions
// ============================================================================

// testCreateFileOfSize creates a file of the specified size and verifies it
func testCreateFileOfSize(t *testing.T, tc *TestContext, size FileSize) {
	t.Helper()

	filePath := tc.Path(fmt.Sprintf("file_%s.bin", size.Name))

	// Generate random data
	data := make([]byte, size.Bytes)
	_, err := rand.Read(data)
	if err != nil {
		t.Fatalf("Failed to generate random data: %v", err)
	}

	// Write file
	err = os.WriteFile(filePath, data, 0644)
	if err != nil {
		t.Fatalf("Failed to write %s file: %v", size.Name, err)
	}

	// Verify file size
	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("Failed to stat file: %v", err)
	}

	if info.Size() != size.Bytes {
		t.Errorf("Expected file size %d bytes, got %d bytes", size.Bytes, info.Size())
	}
}

// testCreateMultipleFilesOfSize creates multiple files of the specified size
func testCreateMultipleFilesOfSize(t *testing.T, tc *TestContext, size FileSize, count int) {
	t.Helper()

	basePath := tc.Path(fmt.Sprintf("files_%s", size.Name))

	// Create base folder
	err := os.Mkdir(basePath, 0755)
	if err != nil {
		t.Fatalf("Failed to create base folder: %v", err)
	}

	// Generate data once and reuse
	data := make([]byte, size.Bytes)
	_, err = rand.Read(data)
	if err != nil {
		t.Fatalf("Failed to generate random data: %v", err)
	}

	// Create multiple files
	for i := 0; i < count; i++ {
		filePath := filepath.Join(basePath, fmt.Sprintf("file_%d.bin", i))
		err := os.WriteFile(filePath, data, 0644)
		if err != nil {
			t.Fatalf("Failed to write file %d: %v", i, err)
		}
	}

	// Verify file count
	entries, err := os.ReadDir(basePath)
	if err != nil {
		t.Fatalf("Failed to read directory: %v", err)
	}

	if len(entries) != count {
		t.Errorf("Expected %d files, got %d", count, len(entries))
	}
}

// testReadFileOfSize creates and reads a file of the specified size
func testReadFileOfSize(t *testing.T, tc *TestContext, size FileSize) {
	t.Helper()

	filePath := tc.Path(fmt.Sprintf("read_%s.bin", size.Name))

	// Generate random data
	expectedData := make([]byte, size.Bytes)
	_, err := rand.Read(expectedData)
	if err != nil {
		t.Fatalf("Failed to generate random data: %v", err)
	}

	// Write file
	err = os.WriteFile(filePath, expectedData, 0644)
	if err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}

	// Read file back
	actualData, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	// Verify data matches
	if !bytes.Equal(expectedData, actualData) {
		t.Errorf("Read data does not match written data")
	}
}

// testWriteThenReadFileOfSize creates, writes, and reads a file
func testWriteThenReadFileOfSize(t *testing.T, tc *TestContext, size FileSize) {
	t.Helper()

	filePath := tc.Path(fmt.Sprintf("write_read_%s.bin", size.Name))

	// Generate random data
	expectedData := make([]byte, size.Bytes)
	_, err := rand.Read(expectedData)
	if err != nil {
		t.Fatalf("Failed to generate random data: %v", err)
	}

	// Write file
	err = os.WriteFile(filePath, expectedData, 0644)
	if err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}

	// Read file back
	actualData, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	// Verify size
	if int64(len(actualData)) != size.Bytes {
		t.Errorf("Expected %d bytes, got %d bytes", size.Bytes, len(actualData))
	}

	// Verify data
	if !bytes.Equal(expectedData, actualData) {
		t.Errorf("Read data does not match written data")
	}
}

// testOverwriteFileOfSize creates a file and then overwrites it
func testOverwriteFileOfSize(t *testing.T, tc *TestContext, size FileSize) {
	t.Helper()

	filePath := tc.Path(fmt.Sprintf("overwrite_%s.bin", size.Name))

	// Write initial data
	initialData := make([]byte, size.Bytes)
	for i := range initialData {
		initialData[i] = 0xAA
	}
	err := os.WriteFile(filePath, initialData, 0644)
	if err != nil {
		t.Fatalf("Failed to write initial file: %v", err)
	}

	// Overwrite with new data
	newData := make([]byte, size.Bytes)
	for i := range newData {
		newData[i] = 0xBB
	}
	err = os.WriteFile(filePath, newData, 0644)
	if err != nil {
		t.Fatalf("Failed to overwrite file: %v", err)
	}

	// Read and verify
	actualData, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	if !bytes.Equal(newData, actualData) {
		t.Errorf("Overwritten data does not match expected data")
	}
}

// testDeleteFileOfSize creates and deletes a file of the specified size
func testDeleteFileOfSize(t *testing.T, tc *TestContext, size FileSize) {
	t.Helper()

	filePath := tc.Path(fmt.Sprintf("delete_%s.bin", size.Name))

	// Create file
	data := make([]byte, size.Bytes)
	err := os.WriteFile(filePath, data, 0644)
	if err != nil {
		t.Fatalf("Failed to create file: %v", err)
	}

	// Verify file exists
	_, err = os.Stat(filePath)
	if err != nil {
		t.Fatalf("File should exist: %v", err)
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
}
