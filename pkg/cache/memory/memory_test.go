package memory

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/cache"
	cachetest "github.com/marmos91/dittofs/pkg/cache/testing"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// TestMemoryCache runs the complete test suite for MemoryCache.
func TestMemoryCache(t *testing.T) {
	suite := &cachetest.CacheTestSuite{
		NewCache: func() cache.Cache {
			// Create cache with 100MB limit for testing
			maxSize := int64(100 * 1024 * 1024)
			return NewMemoryCache(maxSize, nil)
		},
	}

	suite.Run(t)
}

// TestMemoryCacheUnlimited tests MemoryCache with no size limit.
func TestMemoryCacheUnlimited(t *testing.T) {
	suite := &cachetest.CacheTestSuite{
		NewCache: func() cache.Cache {
			// Create cache with no limit (maxSize = 0)
			return NewMemoryCache(0, nil)
		},
	}

	suite.Run(t)
}

// TestGetInfo tests the GetInfo helper function
func TestGetInfo(t *testing.T) {
	ctx := context.Background()
	c := NewMemoryCache(1024*1024, nil) // 1MB cache
	defer func() { _ = c.Close() }()

	t.Run("EmptyCache", func(t *testing.T) {
		info := GetInfo(c)
		if len(info) != 0 {
			t.Errorf("Expected empty info for empty cache, got %d entries", len(info))
		}
	})

	t.Run("SingleFile", func(t *testing.T) {
		contentID := metadata.ContentID("test/file1.txt")
		data := []byte("Hello, World!")

		err := c.Write(ctx, contentID, data)
		if err != nil {
			t.Fatalf("Failed to write: %v", err)
		}

		info := GetInfo(c)
		if len(info) != 1 {
			t.Fatalf("Expected 1 entry, got %d", len(info))
		}

		bufInfo, exists := info[contentID]
		if !exists {
			t.Fatal("Expected content ID in info map")
		}

		if bufInfo.Size != int64(len(data)) {
			t.Errorf("Expected size %d, got %d", len(data), bufInfo.Size)
		}

		if bufInfo.LastWrite.IsZero() {
			t.Error("Expected non-zero last write time")
		}

		// Check that LastWrite is recent (within last second)
		if time.Since(bufInfo.LastWrite) > time.Second {
			t.Errorf("LastWrite time seems too old: %v", bufInfo.LastWrite)
		}
	})

	t.Run("MultipleFiles", func(t *testing.T) {
		// Clear cache
		_ = c.RemoveAll()

		files := map[metadata.ContentID][]byte{
			"test/file1.txt": []byte("File 1 content"),
			"test/file2.txt": []byte("File 2 content with more data"),
			"test/file3.txt": []byte("File 3"),
		}

		// Write all files
		for id, data := range files {
			err := c.Write(ctx, id, data)
			if err != nil {
				t.Fatalf("Failed to write %s: %v", id, err)
			}
			time.Sleep(10 * time.Millisecond) // Ensure different timestamps
		}

		info := GetInfo(c)
		if len(info) != len(files) {
			t.Fatalf("Expected %d entries, got %d", len(files), len(info))
		}

		// Verify each file's info
		for id, expectedData := range files {
			bufInfo, exists := info[id]
			if !exists {
				t.Errorf("Expected %s in info map", id)
				continue
			}

			if bufInfo.Size != int64(len(expectedData)) {
				t.Errorf("For %s: expected size %d, got %d", id, len(expectedData), bufInfo.Size)
			}

			if bufInfo.LastWrite.IsZero() {
				t.Errorf("For %s: expected non-zero last write time", id)
			}
		}
	})
}

// TestEvictLRU tests manual LRU eviction
func TestEvictLRU(t *testing.T) {
	ctx := context.Background()

	t.Run("NoEvictionNeeded", func(t *testing.T) {
		mc := NewMemoryCache(1024, nil).(*MemoryCache) // 1KB limit
		defer func() { _ = mc.Close() }()

		// Write small file (500 bytes)
		err := mc.Write(ctx, "test/small.txt", make([]byte, 500))
		if err != nil {
			t.Fatalf("Failed to write: %v", err)
		}

		// Evict with target larger than current size
		count, freed := mc.EvictLRU(1024)
		if count != 0 || freed != 0 {
			t.Errorf("Expected no eviction (count=0, freed=0), got count=%d, freed=%d", count, freed)
		}

		// Verify file still exists
		if !mc.Exists("test/small.txt") {
			t.Error("File should still exist after no eviction")
		}
	})

	t.Run("EvictOldestFile", func(t *testing.T) {
		mc := NewMemoryCache(2048, nil).(*MemoryCache) // 2KB limit
		defer func() { _ = mc.Close() }()

		// Write three files with delays to ensure different timestamps
		// With automatic eviction enabled, the third write should trigger eviction
		files := []struct {
			id   metadata.ContentID
			size int
		}{
			{"test/old.txt", 700},    // Oldest - will be evicted automatically
			{"test/medium.txt", 700}, // Middle - should remain
			{"test/new.txt", 700},    // Newest - should remain
		}

		for _, f := range files {
			err := mc.Write(ctx, f.id, make([]byte, f.size))
			if err != nil {
				t.Fatalf("Failed to write %s: %v", f.id, err)
			}
			time.Sleep(50 * time.Millisecond) // Ensure different timestamps
		}

		// After all writes, automatic eviction should have happened
		// Writing 3rd file (700 bytes) when cache has 1400 bytes triggers eviction
		// Target = (2048 * 0.90) - 700 = 1143 bytes
		// Should evict file 1 (700 bytes), leaving file 2 (700 bytes)
		// Then write file 3, resulting in files 2 & 3 (1400 bytes total)

		// Oldest file should have been auto-evicted
		if mc.Exists(files[0].id) {
			t.Error("Oldest file should have been auto-evicted during writes")
		}

		// Newer files should still exist
		if !mc.Exists(files[1].id) || !mc.Exists(files[2].id) {
			t.Error("Newer files should still exist")
		}

		// Verify cache size is under max
		totalSize := mc.TotalSize()
		if totalSize > mc.MaxSize() {
			t.Errorf("Cache size %d exceeds max %d after auto-eviction", totalSize, mc.MaxSize())
		}

		// Expected size: 2 files × 700 bytes = 1400 bytes
		expectedSize := int64(1400)
		if totalSize != expectedSize {
			t.Errorf("Expected cache size %d, got %d", expectedSize, totalSize)
		}
	})

	t.Run("EvictMultipleFiles", func(t *testing.T) {
		mc := NewMemoryCache(2048, nil).(*MemoryCache) // 2KB limit
		defer func() { _ = mc.Close() }()

		// Write multiple small files
		// With automatic eviction, writes 7-10 will trigger eviction
		for i := 0; i < 10; i++ {
			id := metadata.ContentID("test/file" + string(rune('0'+i)) + ".txt")
			err := mc.Write(ctx, id, make([]byte, 300))
			if err != nil {
				t.Fatalf("Failed to write: %v", err)
			}
			time.Sleep(10 * time.Millisecond)
		}

		// After automatic eviction during writes, cache should have newest 4 files
		// Files 6,7,8,9 = 1200 bytes (auto-eviction keeps it under max)
		sizeAfterWrites := mc.TotalSize()
		if sizeAfterWrites > mc.MaxSize() {
			t.Errorf("Cache size %d exceeds max %d after auto-eviction", sizeAfterWrites, mc.MaxSize())
		}

		// Now manually evict to a lower target
		count, freed := mc.EvictLRU(900) // Target 900 bytes (3 files)

		if count == 0 {
			t.Error("Expected at least one file to be evicted")
		}

		// Should have freed enough to reach target
		remaining := mc.TotalSize()
		if remaining > 900 {
			t.Errorf("Expected remaining size <= 900, got %d", remaining)
		}

		t.Logf("Evicted %d files, freed %d bytes, remaining %d bytes", count, freed, remaining)
	})

	t.Run("UnlimitedCacheNoEviction", func(t *testing.T) {
		mc := NewMemoryCache(0, nil).(*MemoryCache) // Unlimited
		defer func() { _ = mc.Close() }()

		// Write data
		err := mc.Write(ctx, "test/file.txt", make([]byte, 1000))
		if err != nil {
			t.Fatalf("Failed to write: %v", err)
		}

		// Try to evict - should do nothing for unlimited cache
		count, freed := mc.EvictLRU(100)
		if count != 0 || freed != 0 {
			t.Errorf("Unlimited cache should not evict, got count=%d, freed=%d", count, freed)
		}

		if !mc.Exists("test/file.txt") {
			t.Error("File should still exist in unlimited cache")
		}
	})

	t.Run("CustomTargetSize", func(t *testing.T) {
		mc := NewMemoryCache(5000, nil).(*MemoryCache)
		defer func() { _ = mc.Close() }()

		// Write files totaling 3000 bytes
		for i := 0; i < 6; i++ {
			id := metadata.ContentID("test/file" + string(rune('0'+i)) + ".txt")
			err := mc.Write(ctx, id, make([]byte, 500))
			if err != nil {
				t.Fatalf("Failed to write: %v", err)
			}
			time.Sleep(10 * time.Millisecond)
		}

		// Evict to custom target of 1000 bytes
		count, freed := mc.EvictLRU(1000)

		remaining := mc.TotalSize()
		if remaining > 1000 {
			t.Errorf("Expected remaining <= 1000, got %d", remaining)
		}

		if count == 0 {
			t.Error("Expected some files to be evicted")
		}

		t.Logf("Evicted %d files to reach custom target, freed %d bytes", count, freed)
	})
}
