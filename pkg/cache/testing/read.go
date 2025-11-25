package testing

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// RunReadTests tests read operations (Read and ReadAt).
func (suite *CacheTestSuite) RunReadTests(t *testing.T) {
	t.Run("ReadReturnsEmptyForNonExistentContent", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()
		ctx := testContext()

		id := metadata.ContentID("test-1")

		data, err := c.Read(ctx, id)
		if err != nil {
			t.Fatalf("Read() failed: %v", err)
		}

		if len(data) != 0 {
			t.Errorf("Read() should return empty data for non-existent content, got %d bytes", len(data))
		}
	})

	t.Run("ReadReturnsWrittenData", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()
		ctx := testContext()

		id := metadata.ContentID("test-2")
		originalData := []byte("Hello, World!")

		if err := c.Write(ctx, id, originalData); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}

		retrieved, err := c.Read(ctx, id)
		if err != nil {
			t.Fatalf("Read() failed: %v", err)
		}

		if !bytes.Equal(retrieved, originalData) {
			t.Errorf("Read() data doesn't match written data.\nWant: %q\nGot:  %q", originalData, retrieved)
		}
	})

	t.Run("ReadReturnsIndependentCopy", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()
		ctx := testContext()

		id := metadata.ContentID("test-3")
		originalData := []byte("Hello, World!")

		if err := c.Write(ctx, id, originalData); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}

		// Read twice
		data1, err1 := c.Read(ctx, id)
		data2, err2 := c.Read(ctx, id)

		if err1 != nil || err2 != nil {
			t.Fatalf("Read() failed: %v, %v", err1, err2)
		}

		// Modify first copy
		data1[0] = 'X'

		// Verify second copy is unchanged
		if data2[0] != 'H' {
			t.Error("Read() should return independent copies")
		}
	})

	t.Run("ReadRespectsCancellation", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		id := metadata.ContentID("test-4")

		_, err := c.Read(ctx, id)
		if err == nil {
			t.Error("Read() should fail with cancelled context")
		}
	})

	t.Run("ReadAtReturnsEOFForNonExistentContent", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()
		ctx := testContext()

		id := metadata.ContentID("test-5")
		buf := make([]byte, 10)

		n, err := c.ReadAt(ctx, id, buf, 0)
		if err != io.EOF {
			t.Errorf("ReadAt() should return io.EOF for non-existent content, got: %v", err)
		}
		if n != 0 {
			t.Errorf("ReadAt() should return 0 bytes for non-existent content, got: %d", n)
		}
	})

	t.Run("ReadAtReturnsPartialData", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()
		ctx := testContext()

		id := metadata.ContentID("test-6")
		data := []byte("Hello, World!")

		if err := c.Write(ctx, id, data); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}

		// Read 5 bytes starting at offset 7
		buf := make([]byte, 5)
		n, err := c.ReadAt(ctx, id, buf, 7)
		if err != nil {
			t.Fatalf("ReadAt() failed: %v", err)
		}

		if n != 5 {
			t.Errorf("ReadAt() returned %d bytes, expected 5", n)
		}

		expected := []byte("World")
		if !bytes.Equal(buf, expected) {
			t.Errorf("ReadAt() data doesn't match.\nWant: %q\nGot:  %q", expected, buf)
		}
	})

	t.Run("ReadAtReturnsEOFAtEndOfData", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()
		ctx := testContext()

		id := metadata.ContentID("test-7")
		data := []byte("Hello")

		if err := c.Write(ctx, id, data); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}

		// Try to read beyond end
		buf := make([]byte, 10)
		n, err := c.ReadAt(ctx, id, buf, 5)
		if err != io.EOF {
			t.Errorf("ReadAt() should return io.EOF at end of data, got: %v", err)
		}
		if n != 0 {
			t.Errorf("ReadAt() should return 0 bytes at end of data, got: %d", n)
		}
	})

	t.Run("ReadAtReturnsPartialReadWithEOF", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()
		ctx := testContext()

		id := metadata.ContentID("test-8")
		data := []byte("Hello")

		if err := c.Write(ctx, id, data); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}

		// Try to read more than available
		buf := make([]byte, 10)
		n, err := c.ReadAt(ctx, id, buf, 3)
		if err != io.EOF {
			t.Errorf("ReadAt() should return io.EOF when reading past end, got: %v", err)
		}

		// Should have read "lo" (2 bytes)
		if n != 2 {
			t.Errorf("ReadAt() returned %d bytes, expected 2", n)
		}

		expected := []byte("lo")
		if !bytes.Equal(buf[:n], expected) {
			t.Errorf("ReadAt() data doesn't match.\nWant: %q\nGot:  %q", expected, buf[:n])
		}
	})

	t.Run("ReadAtWithNegativeOffsetFails", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()
		ctx := testContext()

		id := metadata.ContentID("test-9")
		data := []byte("Hello")

		if err := c.Write(ctx, id, data); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}

		buf := make([]byte, 10)
		_, err := c.ReadAt(ctx, id, buf, -1)
		if err == nil {
			t.Error("ReadAt() with negative offset should fail")
		}
	})

	t.Run("ReadAtRespectsCancellation", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		id := metadata.ContentID("test-10")
		buf := make([]byte, 10)

		_, err := c.ReadAt(ctx, id, buf, 0)
		if err == nil {
			t.Error("ReadAt() should fail with cancelled context")
		}
	})

	t.Run("ReadAtMultipleChunks", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()
		ctx := testContext()

		id := metadata.ContentID("test-11")
		data := []byte("0123456789ABCDEF")

		if err := c.Write(ctx, id, data); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}

		// Read in 4-byte chunks
		chunkSize := 4
		result := make([]byte, 0, len(data))

		for offset := 0; offset < len(data); offset += chunkSize {
			buf := make([]byte, chunkSize)
			n, err := c.ReadAt(ctx, id, buf, int64(offset))
			if err != nil && err != io.EOF {
				t.Fatalf("ReadAt() at offset %d failed: %v", offset, err)
			}
			result = append(result, buf[:n]...)
		}

		if !bytes.Equal(result, data) {
			t.Errorf("Chunked read doesn't match original data.\nWant: %q\nGot:  %q", data, result)
		}
	})
}
