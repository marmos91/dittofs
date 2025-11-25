package testing

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// RunWriteTests tests write operations (Write and WriteAt).
func (suite *CacheTestSuite) RunWriteTests(t *testing.T) {
	t.Run("WriteStoresData", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()
		ctx := testContext()

		id := metadata.ContentID("test-1")
		data := []byte("Hello, World!")

		if err := c.Write(ctx, id, data); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}

		// Verify data was stored
		retrieved, err := c.Read(ctx, id)
		if err != nil {
			t.Fatalf("Read() failed: %v", err)
		}

		if !bytes.Equal(retrieved, data) {
			t.Errorf("Retrieved data doesn't match written data.\nWant: %q\nGot:  %q", data, retrieved)
		}
	})

	t.Run("WriteReplacesExistingData", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()
		ctx := testContext()

		id := metadata.ContentID("test-2")
		originalData := []byte("original data")
		newData := []byte("new data")

		// Write original data
		if err := c.Write(ctx, id, originalData); err != nil {
			t.Fatalf("First Write() failed: %v", err)
		}

		// Overwrite with new data
		if err := c.Write(ctx, id, newData); err != nil {
			t.Fatalf("Second Write() failed: %v", err)
		}

		// Verify new data replaced original
		retrieved, err := c.Read(ctx, id)
		if err != nil {
			t.Fatalf("Read() failed: %v", err)
		}

		if !bytes.Equal(retrieved, newData) {
			t.Errorf("Retrieved data doesn't match new data.\nWant: %q\nGot:  %q", newData, retrieved)
		}
	})

	t.Run("WriteEmptyDataSucceeds", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()
		ctx := testContext()

		id := metadata.ContentID("test-3")
		data := []byte{}

		if err := c.Write(ctx, id, data); err != nil {
			t.Fatalf("Write() with empty data failed: %v", err)
		}

		// Verify empty data was stored
		retrieved, err := c.Read(ctx, id)
		if err != nil {
			t.Fatalf("Read() failed: %v", err)
		}

		if len(retrieved) != 0 {
			t.Errorf("Expected empty data, got %d bytes", len(retrieved))
		}
	})

	t.Run("WriteAtAppendsAtEnd", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()
		ctx := testContext()

		id := metadata.ContentID("test-4")
		data1 := []byte("Hello, ")
		data2 := []byte("World!")

		// Write initial data
		if err := c.Write(ctx, id, data1); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}

		// Append more data
		if err := c.WriteAt(ctx, id, data2, int64(len(data1))); err != nil {
			t.Fatalf("WriteAt() failed: %v", err)
		}

		// Verify combined data
		retrieved, err := c.Read(ctx, id)
		if err != nil {
			t.Fatalf("Read() failed: %v", err)
		}

		expected := append(data1, data2...)
		if !bytes.Equal(retrieved, expected) {
			t.Errorf("Retrieved data doesn't match expected.\nWant: %q\nGot:  %q", expected, retrieved)
		}
	})

	t.Run("WriteAtOverwritesMiddle", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()
		ctx := testContext()

		id := metadata.ContentID("test-5")
		originalData := []byte("Hello, World!")
		overwrite := []byte("XXX")

		// Write original data
		if err := c.Write(ctx, id, originalData); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}

		// Overwrite middle portion
		if err := c.WriteAt(ctx, id, overwrite, 7); err != nil {
			t.Fatalf("WriteAt() failed: %v", err)
		}

		// Verify overwritten data
		retrieved, err := c.Read(ctx, id)
		if err != nil {
			t.Fatalf("Read() failed: %v", err)
		}

		expected := []byte("Hello, XXXld!")
		if !bytes.Equal(retrieved, expected) {
			t.Errorf("Retrieved data doesn't match expected.\nWant: %q\nGot:  %q", expected, retrieved)
		}
	})

	t.Run("WriteAtWithGapFillsZeros", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()
		ctx := testContext()

		id := metadata.ContentID("test-6")
		data1 := []byte("Hello")
		data2 := []byte("World")

		// Write initial data
		if err := c.Write(ctx, id, data1); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}

		// Write with gap (sparse file behavior)
		gapOffset := int64(10)
		if err := c.WriteAt(ctx, id, data2, gapOffset); err != nil {
			t.Fatalf("WriteAt() failed: %v", err)
		}

		// Verify gap is filled with zeros
		retrieved, err := c.Read(ctx, id)
		if err != nil {
			t.Fatalf("Read() failed: %v", err)
		}

		// Expected: "Hello" + zeros + "World"
		expected := make([]byte, gapOffset+int64(len(data2)))
		copy(expected[0:], data1)
		// Middle is zeros (default)
		copy(expected[gapOffset:], data2)

		if !bytes.Equal(retrieved, expected) {
			t.Errorf("Retrieved data doesn't match expected.\nWant: %v\nGot:  %v", expected, retrieved)
		}
	})

	t.Run("WriteAtCreatesNewContent", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()
		ctx := testContext()

		id := metadata.ContentID("test-7")
		data := []byte("Hello")

		// WriteAt on non-existent content should create it
		if err := c.WriteAt(ctx, id, data, 0); err != nil {
			t.Fatalf("WriteAt() on new content failed: %v", err)
		}

		// Verify data was stored
		retrieved, err := c.Read(ctx, id)
		if err != nil {
			t.Fatalf("Read() failed: %v", err)
		}

		if !bytes.Equal(retrieved, data) {
			t.Errorf("Retrieved data doesn't match written data.\nWant: %q\nGot:  %q", data, retrieved)
		}
	})

	t.Run("WriteAtWithNegativeOffsetFails", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()
		ctx := testContext()

		id := metadata.ContentID("test-8")
		data := []byte("Hello")

		err := c.WriteAt(ctx, id, data, -1)
		if err == nil {
			t.Error("WriteAt() with negative offset should fail")
		}
	})

	t.Run("WriteRespectsCancellation", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		id := metadata.ContentID("test-9")
		data := []byte("Hello")

		err := c.Write(ctx, id, data)
		if err == nil {
			t.Error("Write() should fail with cancelled context")
		}
	})

	t.Run("WriteAtRespectsCancellation", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		id := metadata.ContentID("test-10")
		data := []byte("Hello")

		err := c.WriteAt(ctx, id, data, 0)
		if err == nil {
			t.Error("WriteAt() should fail with cancelled context")
		}
	})

	t.Run("WriteLargeDataSucceeds", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()
		ctx := testContext()

		id := metadata.ContentID("test-11")
		// Write 10MB of data
		largeData := make([]byte, 10*1024*1024)
		for i := range largeData {
			largeData[i] = byte(i % 256)
		}

		if err := c.Write(ctx, id, largeData); err != nil {
			t.Fatalf("Write() with large data failed: %v", err)
		}

		// Verify size
		if size := c.Size(id); size != int64(len(largeData)) {
			t.Errorf("Size() returned %d, expected %d", size, len(largeData))
		}
	})
}
