package testing

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// RunCacheCoherencyTests tests the cache coherency (validation) methods.
func (suite *CacheTestSuite) RunCacheCoherencyTests(t *testing.T) {
	t.Run("GetCachedMetadataReturnsFalseForNonExistentEntry", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		_, _, ok := c.GetCachedMetadata("non-existent")
		if ok {
			t.Error("Expected ok=false for non-existent entry")
		}
	})

	t.Run("GetCachedMetadataReturnsFalseForNewEntry", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx := testContext()
		id := metadata.ContentID("test-file")

		err := c.Write(ctx, id, []byte("hello"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		// New entries from writes don't have cached metadata until SetCachedMetadata is called
		_, _, ok := c.GetCachedMetadata(id)
		if ok {
			t.Error("Expected ok=false for entry without cached metadata")
		}
	})

	t.Run("SetCachedMetadataStoresMetadata", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx := testContext()
		id := metadata.ContentID("test-file")

		err := c.Write(ctx, id, []byte("hello"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		expectedMtime := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
		expectedSize := uint64(12345)

		c.SetCachedMetadata(id, expectedMtime, expectedSize)

		mtime, size, ok := c.GetCachedMetadata(id)
		if !ok {
			t.Fatal("Expected ok=true after SetCachedMetadata")
		}
		if !mtime.Equal(expectedMtime) {
			t.Errorf("Expected mtime %v, got %v", expectedMtime, mtime)
		}
		if size != expectedSize {
			t.Errorf("Expected size %d, got %d", expectedSize, size)
		}
	})

	t.Run("SetCachedMetadataIsNoOpForNonExistentEntry", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		// Should not panic or create entry
		c.SetCachedMetadata("non-existent", time.Now(), 100)

		_, _, ok := c.GetCachedMetadata("non-existent")
		if ok {
			t.Error("Expected ok=false for non-existent entry")
		}
	})

	t.Run("WriteResetsCachedMetadata", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx := testContext()
		id := metadata.ContentID("test-file")

		// Create entry with metadata
		err := c.Write(ctx, id, []byte("initial"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}
		c.SetCachedMetadata(id, time.Now(), 7)
		c.SetState(id, cache.StateCached)

		// Write new data - should reset metadata
		err = c.Write(ctx, id, []byte("updated"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		_, _, ok := c.GetCachedMetadata(id)
		if ok {
			t.Error("Expected cached metadata to be cleared after write")
		}
	})

	t.Run("IsValidReturnsFalseForNonExistentEntry", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		valid := c.IsValid("non-existent", time.Now(), 100)
		if valid {
			t.Error("Expected IsValid=false for non-existent entry")
		}
	})

	t.Run("IsValidReturnsTrueForDirtyBufferingEntry", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx := testContext()
		id := metadata.ContentID("test-file")

		err := c.Write(ctx, id, []byte("hello"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		// Dirty entry should always be valid (we're the source of truth)
		// Even with mismatched metadata
		valid := c.IsValid(id, time.Now().Add(-time.Hour), 9999)
		if !valid {
			t.Error("Expected IsValid=true for dirty (Buffering) entry")
		}
	})

	t.Run("IsValidReturnsTrueForDirtyUploadingEntry", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx := testContext()
		id := metadata.ContentID("test-file")

		err := c.Write(ctx, id, []byte("hello"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}
		c.SetState(id, cache.StateUploading)

		// Dirty entry should always be valid
		valid := c.IsValid(id, time.Now().Add(-time.Hour), 9999)
		if !valid {
			t.Error("Expected IsValid=true for dirty (Uploading) entry")
		}
	})

	t.Run("IsValidReturnsFalseForCleanEntryWithoutMetadata", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx := testContext()
		id := metadata.ContentID("test-file")

		err := c.Write(ctx, id, []byte("hello"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}
		c.SetState(id, cache.StateCached)
		// Don't set cached metadata

		valid := c.IsValid(id, time.Now(), 5)
		if valid {
			t.Error("Expected IsValid=false for clean entry without cached metadata")
		}
	})

	t.Run("IsValidReturnsTrueForMatchingMetadata", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx := testContext()
		id := metadata.ContentID("test-file")

		err := c.Write(ctx, id, []byte("hello"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		mtime := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
		size := uint64(5)

		c.SetCachedMetadata(id, mtime, size)
		c.SetState(id, cache.StateCached)

		valid := c.IsValid(id, mtime, size)
		if !valid {
			t.Error("Expected IsValid=true for matching metadata")
		}
	})

	t.Run("IsValidReturnsFalseForMtimeMismatch", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx := testContext()
		id := metadata.ContentID("test-file")

		err := c.Write(ctx, id, []byte("hello"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		cachedMtime := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
		currentMtime := time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC) // Different
		size := uint64(5)

		c.SetCachedMetadata(id, cachedMtime, size)
		c.SetState(id, cache.StateCached)

		valid := c.IsValid(id, currentMtime, size)
		if valid {
			t.Error("Expected IsValid=false for mtime mismatch")
		}
	})

	t.Run("IsValidReturnsFalseForSizeMismatch", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx := testContext()
		id := metadata.ContentID("test-file")

		err := c.Write(ctx, id, []byte("hello"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		mtime := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
		cachedSize := uint64(5)
		currentSize := uint64(10) // Different

		c.SetCachedMetadata(id, mtime, cachedSize)
		c.SetState(id, cache.StateCached)

		valid := c.IsValid(id, mtime, currentSize)
		if valid {
			t.Error("Expected IsValid=false for size mismatch")
		}
	})

	t.Run("IsValidReturnsFalseForBothMismatch", func(t *testing.T) {
		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx := testContext()
		id := metadata.ContentID("test-file")

		err := c.Write(ctx, id, []byte("hello"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		cachedMtime := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
		currentMtime := time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC)
		cachedSize := uint64(5)
		currentSize := uint64(10)

		c.SetCachedMetadata(id, cachedMtime, cachedSize)
		c.SetState(id, cache.StateCached)

		valid := c.IsValid(id, currentMtime, currentSize)
		if valid {
			t.Error("Expected IsValid=false for both mtime and size mismatch")
		}
	})

	t.Run("CacheCoherencyWorkflow", func(t *testing.T) {
		// Simulates a complete workflow:
		// 1. Write data
		// 2. Flush and finalize
		// 3. Read validates against metadata
		// 4. External modification invalidates cache

		c := suite.NewCache()
		defer func() { _ = c.Close() }()

		ctx := testContext()
		id := metadata.ContentID("test-file")

		// Step 1: Write data
		err := c.Write(ctx, id, []byte("original content"))
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}
		if state := c.GetState(id); state != cache.StateBuffering {
			t.Errorf("Expected Buffering, got %v", state)
		}

		// Step 2: Simulate flush
		c.SetState(id, cache.StateUploading)
		c.SetFlushedOffset(id, 16)

		// Step 3: Finalize
		originalMtime := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
		originalSize := uint64(16)
		c.SetCachedMetadata(id, originalMtime, originalSize)
		c.SetState(id, cache.StateCached)

		// Verify cache is valid with original metadata
		if !c.IsValid(id, originalMtime, originalSize) {
			t.Error("Expected cache to be valid with original metadata")
		}

		// Step 4: Simulate external modification (file was changed)
		newMtime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
		newSize := uint64(20)

		// Cache should now be invalid
		if c.IsValid(id, newMtime, newSize) {
			t.Error("Expected cache to be invalid after external modification")
		}
	})
}
