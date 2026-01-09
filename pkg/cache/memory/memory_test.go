package memory

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/cache"
	cachetest "github.com/marmos91/dittofs/pkg/cache/testing"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestMemoryCache runs the complete test suite for MemoryCache.
func TestMemoryCache(t *testing.T) {
	suite := &cachetest.CacheTestSuite{
		NewCache: func() cache.Cache {
			// Create cache with 100MB limit for testing
			var maxSize uint64 = 100 * 1024 * 1024
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

		if bufInfo.Size != uint64(len(data)) {
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

			if bufInfo.Size != uint64(len(expectedData)) {
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

		// Write small file (500 bytes) and mark as cached (clean)
		err := mc.Write(ctx, "test/small.txt", make([]byte, 500))
		if err != nil {
			t.Fatalf("Failed to write: %v", err)
		}
		mc.SetState("test/small.txt", cache.StateCached)

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

	t.Run("EvictOldestCleanFile", func(t *testing.T) {
		// Use larger cache size to prevent auto-eviction during writes
		// We want to test explicit eviction, not auto-eviction
		mc := NewMemoryCache(5000, nil).(*MemoryCache) // 5KB limit
		defer func() { _ = mc.Close() }()

		// Write three files with delays to ensure different timestamps
		files := []struct {
			id   metadata.ContentID
			size int
		}{
			{"test/old.txt", 700},    // Oldest - will be evicted
			{"test/medium.txt", 700}, // Middle
			{"test/new.txt", 700},    // Newest
		}

		for _, f := range files {
			err := mc.Write(ctx, f.id, make([]byte, f.size))
			if err != nil {
				t.Fatalf("Failed to write %s: %v", f.id, err)
			}
			// Mark as cached (clean) so they can be evicted
			mc.SetState(f.id, cache.StateCached)
			time.Sleep(50 * time.Millisecond) // Ensure different timestamps
		}

		// Verify all files exist before eviction
		for _, f := range files {
			if !mc.Exists(f.id) {
				t.Fatalf("File %s should exist before eviction", f.id)
			}
		}

		// All files start clean, total = 2100 bytes
		// Evict to reach target of 1400 bytes (should evict oldest)
		count, freed := mc.EvictLRU(1400)

		if count != 1 {
			t.Errorf("Expected 1 file evicted, got %d", count)
		}

		if freed != 700 {
			t.Errorf("Expected 700 bytes freed, got %d", freed)
		}

		// Oldest file should have been evicted
		if mc.Exists(files[0].id) {
			t.Error("Oldest file should have been evicted")
		}

		// Newer files should still exist
		if !mc.Exists(files[1].id) || !mc.Exists(files[2].id) {
			t.Error("Newer files should still exist")
		}
	})

	t.Run("EvictMultipleCleanFiles", func(t *testing.T) {
		mc := NewMemoryCache(5000, nil).(*MemoryCache)
		defer func() { _ = mc.Close() }()

		// Write multiple small files and mark them as cached
		for i := 0; i < 10; i++ {
			id := metadata.ContentID("test/file" + string(rune('0'+i)) + ".txt")
			err := mc.Write(ctx, id, make([]byte, 300))
			if err != nil {
				t.Fatalf("Failed to write: %v", err)
			}
			mc.SetState(id, cache.StateCached)
			time.Sleep(10 * time.Millisecond)
		}

		// Total = 3000 bytes, evict to 900 bytes
		count, freed := mc.EvictLRU(900)

		if count == 0 {
			t.Error("Expected at least one file to be evicted")
		}

		remaining := mc.TotalSize()
		if remaining > 900 {
			t.Errorf("Expected remaining size <= 900, got %d", remaining)
		}

		t.Logf("Evicted %d files, freed %d bytes, remaining %d bytes", count, freed, remaining)
	})

	t.Run("DirtyEntriesProtectedFromEviction", func(t *testing.T) {
		mc := NewMemoryCache(1000, nil).(*MemoryCache)
		defer func() { _ = mc.Close() }()

		// Write files - they start in StateBuffering (dirty)
		for i := 0; i < 5; i++ {
			id := metadata.ContentID("test/file" + string(rune('0'+i)) + ".txt")
			err := mc.Write(ctx, id, make([]byte, 300))
			if err != nil {
				t.Fatalf("Failed to write: %v", err)
			}
			time.Sleep(10 * time.Millisecond)
		}

		// Total = 1500 bytes, exceeds limit
		// But all entries are dirty (Buffering), so none can be evicted
		count, freed := mc.EvictLRU(500)

		if count != 0 || freed != 0 {
			t.Errorf("Expected no eviction of dirty entries, got count=%d, freed=%d", count, freed)
		}

		// All files should still exist
		for i := 0; i < 5; i++ {
			id := metadata.ContentID("test/file" + string(rune('0'+i)) + ".txt")
			if !mc.Exists(id) {
				t.Errorf("Dirty file %s should not have been evicted", id)
			}
		}

		// Cache size exceeds limit but that's OK for dirty entries
		if mc.TotalSize() != 1500 {
			t.Errorf("Expected total size 1500, got %d", mc.TotalSize())
		}
	})

	t.Run("OnlyCleanEntriesEvicted", func(t *testing.T) {
		mc := NewMemoryCache(2000, nil).(*MemoryCache)
		defer func() { _ = mc.Close() }()

		// Create mix of dirty and clean entries
		// Dirty (Buffering)
		_ = mc.Write(ctx, "dirty1", make([]byte, 400))
		time.Sleep(10 * time.Millisecond)

		// Clean (Cached)
		_ = mc.Write(ctx, "clean1", make([]byte, 400))
		mc.SetState("clean1", cache.StateCached)
		time.Sleep(10 * time.Millisecond)

		// Dirty (Uploading)
		_ = mc.Write(ctx, "dirty2", make([]byte, 400))
		mc.SetState("dirty2", cache.StateUploading)
		time.Sleep(10 * time.Millisecond)

		// Clean (Cached)
		_ = mc.Write(ctx, "clean2", make([]byte, 400))
		mc.SetState("clean2", cache.StateCached)
		time.Sleep(10 * time.Millisecond)

		// Total = 1600 bytes
		// Try to evict to 800 bytes - should only evict clean entries
		count, freed := mc.EvictLRU(800)

		// Should evict both clean entries (oldest first)
		if count != 2 {
			t.Errorf("Expected 2 clean files evicted, got %d", count)
		}
		if freed != 800 {
			t.Errorf("Expected 800 bytes freed, got %d", freed)
		}

		// Dirty entries should remain
		if !mc.Exists("dirty1") || !mc.Exists("dirty2") {
			t.Error("Dirty entries should not have been evicted")
		}

		// Clean entries should be gone
		if mc.Exists("clean1") || mc.Exists("clean2") {
			t.Error("Clean entries should have been evicted")
		}
	})

	t.Run("UnlimitedCacheNoEviction", func(t *testing.T) {
		mc := NewMemoryCache(0, nil).(*MemoryCache) // Unlimited
		defer func() { _ = mc.Close() }()

		// Write data and mark as cached
		err := mc.Write(ctx, "test/file.txt", make([]byte, 1000))
		if err != nil {
			t.Fatalf("Failed to write: %v", err)
		}
		mc.SetState("test/file.txt", cache.StateCached)

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

		// Write files totaling 3000 bytes, all marked as cached
		for i := 0; i < 6; i++ {
			id := metadata.ContentID("test/file" + string(rune('0'+i)) + ".txt")
			err := mc.Write(ctx, id, make([]byte, 500))
			if err != nil {
				t.Fatalf("Failed to write: %v", err)
			}
			mc.SetState(id, cache.StateCached)
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

	t.Run("EvictionUsesLastAccessNotLastWrite", func(t *testing.T) {
		mc := NewMemoryCache(2000, nil).(*MemoryCache)
		defer func() { _ = mc.Close() }()

		// Write two files
		_ = mc.Write(ctx, "file1", make([]byte, 500))
		mc.SetState("file1", cache.StateCached)
		time.Sleep(50 * time.Millisecond)

		_ = mc.Write(ctx, "file2", make([]byte, 500))
		mc.SetState("file2", cache.StateCached)
		time.Sleep(50 * time.Millisecond)

		// file1 was written first, but let's read it to update lastAccess
		buf := make([]byte, 100)
		_, _ = mc.ReadAt(ctx, "file1", buf, 0)

		// Now file1 has more recent lastAccess than file2
		// Eviction should evict file2 (older lastAccess)
		count, _ := mc.EvictLRU(500)

		if count != 1 {
			t.Errorf("Expected 1 file evicted, got %d", count)
		}

		// file2 should be evicted (older lastAccess)
		if mc.Exists("file2") {
			t.Error("file2 should have been evicted (older lastAccess)")
		}

		// file1 should remain (recent lastAccess from read)
		if !mc.Exists("file1") {
			t.Error("file1 should remain (recent lastAccess)")
		}
	})
}

// =============================================================================
// Write Gathering Tests
// =============================================================================
//
// Write gathering is based on Linux kernel's "wdelay" optimization (fs/nfsd/vfs.c).
// These tests verify that the cache correctly tracks active writers and detects
// when write gathering should delay COMMIT operations.

// TestWriteGathering_BeginEndWrite tests the BeginWrite/EndWrite tracking.
func TestWriteGathering_BeginEndWrite(t *testing.T) {
	ctx := context.Background()
	mc := NewMemoryCache(1024*1024, nil)
	defer func() { _ = mc.Close() }()

	contentID := metadata.ContentID("test/file.txt")

	t.Run("NoActiveWritersInitially", func(t *testing.T) {
		// BeginWrite on non-existent entry should be a no-op
		mc.BeginWrite(contentID)
		if got := mc.GetActiveWriters(contentID); got != 0 {
			t.Errorf("Expected 0 active writers for non-existent entry, got %d", got)
		}
	})

	// Create the entry
	err := mc.WriteAt(ctx, contentID, []byte("test data"), 0)
	if err != nil {
		t.Fatalf("Failed to write: %v", err)
	}

	t.Run("SingleWriter", func(t *testing.T) {
		mc.BeginWrite(contentID)
		if got := mc.GetActiveWriters(contentID); got != 1 {
			t.Errorf("Expected 1 active writer after BeginWrite, got %d", got)
		}

		mc.EndWrite(contentID)
		if got := mc.GetActiveWriters(contentID); got != 0 {
			t.Errorf("Expected 0 active writers after EndWrite, got %d", got)
		}
	})

	t.Run("MultipleWriters", func(t *testing.T) {
		mc.BeginWrite(contentID)
		mc.BeginWrite(contentID)
		mc.BeginWrite(contentID)
		if got := mc.GetActiveWriters(contentID); got != 3 {
			t.Errorf("Expected 3 active writers, got %d", got)
		}

		mc.EndWrite(contentID)
		if got := mc.GetActiveWriters(contentID); got != 2 {
			t.Errorf("Expected 2 active writers after one EndWrite, got %d", got)
		}

		mc.EndWrite(contentID)
		mc.EndWrite(contentID)
		if got := mc.GetActiveWriters(contentID); got != 0 {
			t.Errorf("Expected 0 active writers after all EndWrite, got %d", got)
		}
	})

	t.Run("EndWriteDoesNotGoNegative", func(t *testing.T) {
		// Extra EndWrite calls should not go negative
		mc.EndWrite(contentID)
		mc.EndWrite(contentID)
		if got := mc.GetActiveWriters(contentID); got != 0 {
			t.Errorf("Expected 0 active writers (not negative), got %d", got)
		}
	})
}

// TestWriteGathering_ShouldGatherWrites tests the ShouldGatherWrites detection.
func TestWriteGathering_ShouldGatherWrites(t *testing.T) {
	ctx := context.Background()
	mc := NewMemoryCache(1024*1024, nil)
	defer func() { _ = mc.Close() }()

	contentID := metadata.ContentID("test/file.txt")

	t.Run("NonExistentEntry", func(t *testing.T) {
		if mc.ShouldGatherWrites(contentID, 10*time.Millisecond) {
			t.Error("Expected ShouldGatherWrites to return false for non-existent entry")
		}
	})

	// Create the entry
	err := mc.WriteAt(ctx, contentID, []byte("test data"), 0)
	if err != nil {
		t.Fatalf("Failed to write: %v", err)
	}

	t.Run("RecentWriteTriggersGathering", func(t *testing.T) {
		// Write just happened, so it should trigger gathering
		if !mc.ShouldGatherWrites(contentID, 1*time.Second) {
			t.Error("Expected ShouldGatherWrites to return true immediately after write")
		}
	})

	t.Run("OldWriteDoesNotTriggerGathering", func(t *testing.T) {
		// Wait longer than the threshold
		time.Sleep(50 * time.Millisecond)
		if mc.ShouldGatherWrites(contentID, 10*time.Millisecond) {
			t.Error("Expected ShouldGatherWrites to return false after threshold passed")
		}
	})

	t.Run("MultipleWritersTriggersGathering", func(t *testing.T) {
		// Even with old write time, multiple active writers should trigger gathering
		mc.BeginWrite(contentID)
		mc.BeginWrite(contentID)
		defer func() {
			mc.EndWrite(contentID)
			mc.EndWrite(contentID)
		}()

		if !mc.ShouldGatherWrites(contentID, 1*time.Nanosecond) {
			t.Error("Expected ShouldGatherWrites to return true with multiple active writers")
		}
	})
}

// TestWriteGathering_ConcurrentAccess tests thread safety of write gathering.
func TestWriteGathering_ConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	mc := NewMemoryCache(1024*1024, nil)
	defer func() { _ = mc.Close() }()

	contentID := metadata.ContentID("test/concurrent.txt")

	// Create the entry
	err := mc.WriteAt(ctx, contentID, []byte("initial data"), 0)
	if err != nil {
		t.Fatalf("Failed to write: %v", err)
	}

	// Simulate concurrent writers
	const numWriters = 100
	done := make(chan bool, numWriters)

	for i := 0; i < numWriters; i++ {
		go func() {
			mc.BeginWrite(contentID)
			// Simulate some work
			time.Sleep(time.Millisecond)
			mc.EndWrite(contentID)
			done <- true
		}()
	}

	// Wait for all goroutines to finish
	for i := 0; i < numWriters; i++ {
		<-done
	}

	// After all goroutines finish, active writers should be 0
	if got := mc.GetActiveWriters(contentID); got != 0 {
		t.Errorf("Expected 0 active writers after all goroutines finish, got %d", got)
	}
}
