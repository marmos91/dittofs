package testing

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// RunBasicTests tests basic cache operations like existence checks, size queries, and lifecycle.
func (suite *CacheTestSuite) RunBasicTests(t *testing.T) {
	t.Run("ExistsReturnsFalseForNonExistentContent", func(t *testing.T) {
		c := suite.NewCache()
		defer c.Close()

		id := metadata.ContentID("test-content-1")

		if c.Exists(id) {
			t.Error("Exists() should return false for non-existent content")
		}
	})

	t.Run("ExistsReturnsTrueAfterWrite", func(t *testing.T) {
		c := suite.NewCache()
		defer c.Close()
		ctx := testContext()

		id := metadata.ContentID("test-content-2")
		data := []byte("Hello, World!")

		if err := c.Write(ctx, id, data); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}

		if !c.Exists(id) {
			t.Error("Exists() should return true after Write()")
		}
	})

	t.Run("SizeReturnsZeroForNonExistentContent", func(t *testing.T) {
		c := suite.NewCache()
		defer c.Close()

		id := metadata.ContentID("test-content-3")

		if size := c.Size(id); size != 0 {
			t.Errorf("Size() should return 0 for non-existent content, got %d", size)
		}
	})

	t.Run("SizeReturnsCorrectSizeAfterWrite", func(t *testing.T) {
		c := suite.NewCache()
		defer c.Close()
		ctx := testContext()

		id := metadata.ContentID("test-content-4")
		data := []byte("Hello, World!")

		if err := c.Write(ctx, id, data); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}

		expectedSize := int64(len(data))
		if size := c.Size(id); size != expectedSize {
			t.Errorf("Size() returned %d, expected %d", size, expectedSize)
		}
	})

	t.Run("LastWriteReturnsZeroTimeForNonExistentContent", func(t *testing.T) {
		c := suite.NewCache()
		defer c.Close()

		id := metadata.ContentID("test-content-5")

		lastWrite := c.LastWrite(id)
		if !lastWrite.IsZero() {
			t.Errorf("LastWrite() should return zero time for non-existent content, got %v", lastWrite)
		}
	})

	t.Run("LastWriteReturnsRecentTimeAfterWrite", func(t *testing.T) {
		c := suite.NewCache()
		defer c.Close()
		ctx := testContext()

		id := metadata.ContentID("test-content-6")
		data := []byte("Hello, World!")

		beforeWrite := time.Now()
		if err := c.Write(ctx, id, data); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}
		afterWrite := time.Now()

		lastWrite := c.LastWrite(id)
		if lastWrite.Before(beforeWrite) || lastWrite.After(afterWrite) {
			t.Errorf("LastWrite() returned %v, expected time between %v and %v",
				lastWrite, beforeWrite, afterWrite)
		}
	})

	t.Run("ListReturnsEmptyForNewCache", func(t *testing.T) {
		c := suite.NewCache()
		defer c.Close()

		list := c.List()
		if len(list) != 0 {
			t.Errorf("List() should return empty list for new cache, got %d items", len(list))
		}
	})

	t.Run("ListReturnsAllCachedContent", func(t *testing.T) {
		c := suite.NewCache()
		defer c.Close()
		ctx := testContext()

		ids := []metadata.ContentID{
			"content-1",
			"content-2",
			"content-3",
		}

		for _, id := range ids {
			if err := c.Write(ctx, id, []byte("test data")); err != nil {
				t.Fatalf("Write() failed for %s: %v", id, err)
			}
		}

		list := c.List()
		if len(list) != len(ids) {
			t.Fatalf("List() returned %d items, expected %d", len(list), len(ids))
		}

		// Verify all IDs are present
		idMap := make(map[metadata.ContentID]bool)
		for _, id := range list {
			idMap[id] = true
		}

		for _, expectedID := range ids {
			if !idMap[expectedID] {
				t.Errorf("List() missing content ID %s", expectedID)
			}
		}
	})

	t.Run("TotalSizeReturnsZeroForNewCache", func(t *testing.T) {
		c := suite.NewCache()
		defer c.Close()

		if total := c.TotalSize(); total != 0 {
			t.Errorf("TotalSize() should return 0 for new cache, got %d", total)
		}
	})

	t.Run("TotalSizeReturnsSumOfAllCachedData", func(t *testing.T) {
		c := suite.NewCache()
		defer c.Close()
		ctx := testContext()

		data1 := []byte("data1")      // 5 bytes
		data2 := []byte("data22")     // 6 bytes
		data3 := []byte("data333")    // 7 bytes
		expectedTotal := int64(18)

		if err := c.Write(ctx, "content-1", data1); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}
		if err := c.Write(ctx, "content-2", data2); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}
		if err := c.Write(ctx, "content-3", data3); err != nil {
			t.Fatalf("Write() failed: %v", err)
		}

		if total := c.TotalSize(); total != expectedTotal {
			t.Errorf("TotalSize() returned %d, expected %d", total, expectedTotal)
		}
	})

	t.Run("MaxSizeReturnsConfiguredLimit", func(t *testing.T) {
		c := suite.NewCache()
		defer c.Close()

		// MaxSize should return whatever was configured
		// (could be 0 for unlimited or a positive value)
		maxSize := c.MaxSize()
		if maxSize < 0 {
			t.Errorf("MaxSize() returned negative value: %d", maxSize)
		}
	})

	t.Run("CloseIsIdempotent", func(t *testing.T) {
		c := suite.NewCache()

		if err := c.Close(); err != nil {
			t.Fatalf("First Close() failed: %v", err)
		}

		if err := c.Close(); err != nil {
			t.Errorf("Second Close() should succeed (idempotent), got error: %v", err)
		}
	})
}
