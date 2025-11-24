package testing

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// RunManagementTests tests cache management operations (Remove, RemoveAll, Close).
func (suite *CacheTestSuite) RunManagementTests(t *testing.T) {
	t.Run("RemoveDeletesCachedData", func(t *testing.T) {
		c := suite.NewCache()
		defer c.Close()
		ctx := testContext()

		id := metadata.ContentID("test-1")
		data := []byte("Hello, World!")

		// Write data
		if err := c.Write(ctx, id, data); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}

		// Verify it exists
		if !c.Exists(id) {
			t.Fatal("Content should exist before Remove()")
		}

		// Remove it
		if err := c.Remove(id); err != nil {
			t.Fatalf("Remove() failed: %v", err)
		}

		// Verify it's gone
		if c.Exists(id) {
			t.Error("Content should not exist after Remove()")
		}

		if size := c.Size(id); size != 0 {
			t.Errorf("Size() should return 0 after Remove(), got %d", size)
		}
	})

	t.Run("RemoveIsIdempotent", func(t *testing.T) {
		c := suite.NewCache()
		defer c.Close()

		id := metadata.ContentID("test-2")

		// Remove non-existent content (should succeed)
		if err := c.Remove(id); err != nil {
			t.Errorf("Remove() on non-existent content should succeed, got: %v", err)
		}

		// Remove again (should still succeed)
		if err := c.Remove(id); err != nil {
			t.Errorf("Second Remove() should succeed, got: %v", err)
		}
	})

	t.Run("RemoveUpdatesTotalSize", func(t *testing.T) {
		c := suite.NewCache()
		defer c.Close()
		ctx := testContext()

		id1 := metadata.ContentID("content-1")
		id2 := metadata.ContentID("content-2")
		data1 := []byte("data1") // 5 bytes
		data2 := []byte("data2") // 5 bytes

		// Write two files
		if err := c.Write(ctx, id1, data1); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}
		if err := c.Write(ctx, id2, data2); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}

		// Check total size
		if total := c.TotalSize(); total != 10 {
			t.Errorf("TotalSize() should be 10, got %d", total)
		}

		// Remove one file
		if err := c.Remove(id1); err != nil {
			t.Fatalf("Remove() failed: %v", err)
		}

		// Check total size decreased
		if total := c.TotalSize(); total != 5 {
			t.Errorf("TotalSize() should be 5 after Remove(), got %d", total)
		}
	})

	t.Run("RemoveUpdatesListSize", func(t *testing.T) {
		c := suite.NewCache()
		defer c.Close()
		ctx := testContext()

		id1 := metadata.ContentID("content-1")
		id2 := metadata.ContentID("content-2")
		data := []byte("test data")

		// Write two files
		if err := c.Write(ctx, id1, data); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}
		if err := c.Write(ctx, id2, data); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}

		// Check list size
		if len(c.List()) != 2 {
			t.Errorf("List() should have 2 items")
		}

		// Remove one file
		if err := c.Remove(id1); err != nil {
			t.Fatalf("Remove() failed: %v", err)
		}

		// Check list size decreased
		list := c.List()
		if len(list) != 1 {
			t.Errorf("List() should have 1 item after Remove(), got %d", len(list))
		}

		// Verify correct item remains
		if list[0] != id2 {
			t.Errorf("List() should contain %s, got %s", id2, list[0])
		}
	})

	t.Run("RemoveAllClearsAllCachedData", func(t *testing.T) {
		c := suite.NewCache()
		defer c.Close()
		ctx := testContext()

		ids := []metadata.ContentID{"content-1", "content-2", "content-3"}
		data := []byte("test data")

		// Write multiple files
		for _, id := range ids {
			if err := c.Write(ctx, id, data); err != nil {
				t.Fatalf("Write() failed for %s: %v", id, err)
			}
		}

		// Verify they exist
		if len(c.List()) != 3 {
			t.Fatal("Should have 3 items before RemoveAll()")
		}

		// Remove all
		if err := c.RemoveAll(); err != nil {
			t.Fatalf("RemoveAll() failed: %v", err)
		}

		// Verify all gone
		if len(c.List()) != 0 {
			t.Errorf("List() should be empty after RemoveAll(), got %d items", len(c.List()))
		}

		if total := c.TotalSize(); total != 0 {
			t.Errorf("TotalSize() should be 0 after RemoveAll(), got %d", total)
		}

		// Verify individual files are gone
		for _, id := range ids {
			if c.Exists(id) {
				t.Errorf("Content %s should not exist after RemoveAll()", id)
			}
		}
	})

	t.Run("RemoveAllIsIdempotent", func(t *testing.T) {
		c := suite.NewCache()
		defer c.Close()

		// RemoveAll on empty cache (should succeed)
		if err := c.RemoveAll(); err != nil {
			t.Errorf("RemoveAll() on empty cache should succeed, got: %v", err)
		}

		// RemoveAll again (should still succeed)
		if err := c.RemoveAll(); err != nil {
			t.Errorf("Second RemoveAll() should succeed, got: %v", err)
		}
	})

	t.Run("CloseReleasesResources", func(t *testing.T) {
		c := suite.NewCache()
		ctx := testContext()

		id := metadata.ContentID("test-7")
		data := []byte("Hello, World!")

		// Write some data
		if err := c.Write(ctx, id, data); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}

		// Close the cache
		if err := c.Close(); err != nil {
			t.Fatalf("Close() failed: %v", err)
		}

		// Operations after Close should fail
		err := c.Write(ctx, id, data)
		if err == nil {
			t.Error("Write() should fail after Close()")
		}
	})

	t.Run("OperationsAfterCloseReturnErrors", func(t *testing.T) {
		c := suite.NewCache()
		ctx := testContext()

		id := metadata.ContentID("test-8")
		data := []byte("test")

		// Close the cache
		if err := c.Close(); err != nil {
			t.Fatalf("Close() failed: %v", err)
		}

		// All write operations should fail
		if err := c.Write(ctx, id, data); err == nil {
			t.Error("Write() should fail after Close()")
		}

		if err := c.WriteAt(ctx, id, data, 0); err == nil {
			t.Error("WriteAt() should fail after Close()")
		}

		if err := c.Remove(id); err == nil {
			t.Error("Remove() should fail after Close()")
		}

		if err := c.RemoveAll(); err == nil {
			t.Error("RemoveAll() should fail after Close()")
		}

		// Read operations should handle closed cache gracefully
		// (they might return empty or error, either is acceptable)
		_, _ = c.Read(ctx, id)
		_, _ = c.ReadAt(ctx, id, data, 0)
	})
}
