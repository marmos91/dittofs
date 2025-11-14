package suites

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/framework"
)

// TestBasicOperations tests fundamental file operations
func TestBasicOperations(t *testing.T, storeType framework.StoreType) {
	ctx := framework.NewTestContext(t, storeType)

	t.Run("CreateAndReadFile", func(t *testing.T) {
		content := []byte("Hello, DittoFS!")
		err := ctx.WriteFile("test.txt", content, 0644)
		if err != nil {
			t.Fatalf("Failed to write file: %v", err)
		}

		ctx.AssertFileExists("test.txt")
		ctx.AssertFileContent("test.txt", content)
	})

	t.Run("CreateEmptyFile", func(t *testing.T) {
		err := ctx.WriteFile("empty.txt", []byte{}, 0644)
		if err != nil {
			t.Fatalf("Failed to create empty file: %v", err)
		}

		ctx.AssertFileExists("empty.txt")
		ctx.AssertFileContent("empty.txt", []byte{})

		info, err := ctx.Stat("empty.txt")
		if err != nil {
			t.Fatalf("Failed to stat file: %v", err)
		}
		if info.Size() != 0 {
			t.Errorf("Empty file size = %d, want 0", info.Size())
		}
	})

	t.Run("OverwriteFile", func(t *testing.T) {
		// Create initial file
		err := ctx.WriteFile("overwrite.txt", []byte("initial"), 0644)
		if err != nil {
			t.Fatalf("Failed to write initial file: %v", err)
		}
		ctx.AssertFileContent("overwrite.txt", []byte("initial"))

		// Overwrite with new content
		err = ctx.WriteFile("overwrite.txt", []byte("overwritten"), 0644)
		if err != nil {
			t.Fatalf("Failed to overwrite file: %v", err)
		}
		ctx.AssertFileContent("overwrite.txt", []byte("overwritten"))
	})

	t.Run("DeleteFile", func(t *testing.T) {
		// Create file
		err := ctx.WriteFile("delete.txt", []byte("delete me"), 0644)
		if err != nil {
			t.Fatalf("Failed to write file: %v", err)
		}
		ctx.AssertFileExists("delete.txt")

		// Delete file
		err = ctx.Remove("delete.txt")
		if err != nil {
			t.Fatalf("Failed to delete file: %v", err)
		}
		ctx.AssertFileNotExists("delete.txt")
	})

	t.Run("AppendToFile", func(t *testing.T) {
		// Create initial file
		err := ctx.WriteFile("append.txt", []byte("line1\n"), 0644)
		if err != nil {
			t.Fatalf("Failed to write file: %v", err)
		}

		// Append to file using OpenFile
		path := ctx.Path("append.txt")
		file, err := openFileAppend(path)
		if err != nil {
			t.Fatalf("Failed to open file for append: %v", err)
		}
		_, err = file.Write([]byte("line2\n"))
		if err != nil {
			closeErr := file.Close()
			if closeErr != nil {
				t.Fatalf("Failed to append to file: %v; additionally, failed to close file: %v", err, closeErr)
			}
			t.Fatalf("Failed to append to file: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("Failed to close file: %v", err)
		}

		// Verify content
		expected := []byte("line1\nline2\n")
		ctx.AssertFileContent("append.txt", expected)
	})

	t.Run("LargeFile", func(t *testing.T) {
		// Create a 1MB file
		size := 1024 * 1024
		content := bytes.Repeat([]byte("A"), size)

		err := ctx.WriteFile("large.txt", content, 0644)
		if err != nil {
			t.Fatalf("Failed to write large file: %v", err)
		}

		// Verify size
		info, err := ctx.Stat("large.txt")
		if err != nil {
			t.Fatalf("Failed to stat large file: %v", err)
		}
		if info.Size() != int64(size) {
			t.Errorf("Large file size = %d, want %d", info.Size(), size)
		}

		// Verify content (read back)
		actual, err := ctx.ReadFile("large.txt")
		if err != nil {
			t.Fatalf("Failed to read large file: %v", err)
		}
		if len(actual) != size {
			t.Errorf("Read size = %d, want %d", len(actual), size)
		}
		if !bytes.Equal(actual, content) {
			t.Error("Large file content mismatch")
		}
	})

	t.Run("BinaryFile", func(t *testing.T) {
		// Create binary content with all byte values
		content := make([]byte, 256)
		for i := range content {
			content[i] = byte(i)
		}

		err := ctx.WriteFile("binary.dat", content, 0644)
		if err != nil {
			t.Fatalf("Failed to write binary file: %v", err)
		}

		ctx.AssertFileContent("binary.dat", content)
	})

	t.Run("FilenameWithSpecialChars", func(t *testing.T) {
		// Test various special characters in filenames
		filenames := []string{
			"file with spaces.txt",
			"file-with-dashes.txt",
			"file_with_underscores.txt",
			"file.multiple.dots.txt",
		}

		for _, filename := range filenames {
			t.Run(filename, func(t *testing.T) {
				content := []byte("content for " + filename)
				err := ctx.WriteFile(filename, content, 0644)
				if err != nil {
					t.Fatalf("Failed to create file %q: %v", filename, err)
				}
				ctx.AssertFileContent(filename, content)

				// Clean up
				_ = ctx.Remove(filename)
			})
		}
	})
}
