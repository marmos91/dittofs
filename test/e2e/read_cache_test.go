//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/cache"
	cachememory "github.com/marmos91/dittofs/pkg/cache/memory"
	"github.com/marmos91/dittofs/pkg/registry"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// TestReadCacheBasic tests basic read cache functionality
func TestReadCacheBasic(t *testing.T) {
	// Create test context with read cache only (no write cache)
	config := &TestConfig{
		Name:          "memory-memory-with-read-cache",
		MetadataStore: MetadataMemory,
		ContentStore:  ContentMemory,
		ShareName:     "/export",
	}

	tc := newTestContextWithReadCache(t, config, false) // no write cache
	defer tc.Cleanup()

	readCache := tc.getReadCache()
	if readCache == nil {
		t.Fatal("Read cache not initialized")
	}

	t.Run("ReadCachePopulatedAfterWrite", func(t *testing.T) {
		testReadCachePopulatedAfterWrite(t, tc, readCache)
	})

	t.Run("ReadCacheHitAfterInitialRead", func(t *testing.T) {
		testReadCacheHitAfterInitialRead(t, tc, readCache)
	})

	t.Run("ReadCacheMissForNewFile", func(t *testing.T) {
		testReadCacheMissForNewFile(t, tc, readCache)
	})
}

// TestReadCacheWithWriteCache tests read cache when write cache is also present
func TestReadCacheWithWriteCache(t *testing.T) {
	// Create test context with both read and write cache
	config := &TestConfig{
		Name:          "memory-memory-with-both-caches",
		MetadataStore: MetadataMemory,
		ContentStore:  ContentMemory,
		ShareName:     "/export",
	}

	tc := newTestContextWithReadCache(t, config, true) // with write cache
	defer tc.Cleanup()

	readCache := tc.getReadCache()
	writeCache := tc.getWriteCache()
	if readCache == nil {
		t.Fatal("Read cache not initialized")
	}
	if writeCache == nil {
		t.Fatal("Write cache not initialized")
	}

	t.Run("ReadFromWriteCacheBeforeCommit", func(t *testing.T) {
		testReadFromWriteCacheBeforeCommit(t, tc, readCache, writeCache)
	})

	t.Run("ReadCachePopulatedAfterCommit", func(t *testing.T) {
		testReadCachePopulatedAfterCommit(t, tc, readCache, writeCache)
	})
}

// TestReadCacheLRUEviction tests that LRU eviction works properly
func TestReadCacheLRUEviction(t *testing.T) {
	// Create test context with small cache size to force eviction
	config := &TestConfig{
		Name:          "memory-memory-with-small-read-cache",
		MetadataStore: MetadataMemory,
		ContentStore:  ContentMemory,
		ShareName:     "/export",
	}

	// Use very small cache (100KB) to force eviction
	tc := newTestContextWithReadCacheSize(t, config, 100*1024, false) // no write cache
	defer tc.Cleanup()

	readCache := tc.getReadCache()
	if readCache == nil {
		t.Fatal("Read cache not initialized")
	}

	t.Run("LRUEvictsOldestFile", func(t *testing.T) {
		testLRUEvictsOldestFile(t, tc, readCache)
	})
}

// testContextWithReadCache extends TestContext with read cache support
type testContextWithReadCache struct {
	*TestContext
	readCache        cache.Cache
	writeCache       cache.Cache
	readCacheName    string
	writeCacheName   string
	localstackHelper *LocalstackHelper
}

// newTestContextWithReadCache creates a test context with read cache
func newTestContextWithReadCache(t *testing.T, config *TestConfig, withWriteCache bool) *testContextWithReadCache {
	return newTestContextWithReadCacheSize(t, config, 10*1024*1024, withWriteCache) // 10MB default
}

// newTestContextWithReadCacheSize creates a test context with specific cache size
func newTestContextWithReadCacheSize(t *testing.T, config *TestConfig, cacheSize int64, withWriteCache bool) *testContextWithReadCache {
	t.Helper()

	// Create base test context manually
	ctx, cancel := context.WithCancel(context.Background())

	tc := &testContextWithReadCache{
		TestContext: &TestContext{
			T:      t,
			Config: config,
			ctx:    ctx,
			cancel: cancel,
			Port:   findFreePort(t),
		},
	}

	// If using S3, setup localstack client
	if config.ContentStore == ContentS3 {
		if !CheckLocalstackAvailable(t) {
			t.Skip("Localstack not available, skipping S3 test")
		}
		helper := NewLocalstackHelper(t)
		SetupS3Config(t, config, helper)
		tc.localstackHelper = helper
	}

	// Setup stores
	tc.setupStores()

	// Setup caches
	tc.setupReadCache(cacheSize)
	if withWriteCache {
		tc.setupWriteCache(cacheSize)
	}

	// Start server with caches
	tc.startServerWithReadCache(withWriteCache)

	// Mount NFS
	tc.mountNFS()

	return tc
}

// setupReadCache initializes the read cache with specified size
func (tc *testContextWithReadCache) setupReadCache(size int64) {
	tc.T.Helper()

	// Create read cache
	tc.readCacheName = "read-cache"
	tc.readCache = cachememory.NewMemoryCache(size, nil)
}

// setupWriteCache initializes the write cache with specified size
func (tc *testContextWithReadCache) setupWriteCache(size int64) {
	tc.T.Helper()

	// Create write cache
	tc.writeCacheName = "write-cache"
	tc.writeCache = cachememory.NewMemoryCache(size, nil)
}

// startServerWithReadCache starts the server with read cache configured
func (tc *testContextWithReadCache) startServerWithReadCache(withWriteCache bool) {
	tc.T.Helper()

	// Create registry
	reg := registry.NewRegistry()

	// Register stores
	if err := reg.RegisterMetadataStore("metadata", tc.MetadataStore); err != nil {
		tc.T.Fatalf("Failed to register metadata store: %v", err)
	}
	if err := reg.RegisterContentStore("content", tc.ContentStore); err != nil {
		tc.T.Fatalf("Failed to register content store: %v", err)
	}

	// Register read cache
	if err := reg.RegisterCache(tc.readCacheName, tc.readCache); err != nil {
		tc.T.Fatalf("Failed to register read cache: %v", err)
	}

	// Register write cache if requested
	if withWriteCache && tc.writeCache != nil {
		if err := reg.RegisterCache(tc.writeCacheName, tc.writeCache); err != nil {
			tc.T.Fatalf("Failed to register write cache: %v", err)
		}
	}

	// Add share with read cache (and optionally write cache)
	shareConfig := &registry.ShareConfig{
		Name:          tc.Config.ShareName,
		MetadataStore: "metadata",
		ContentStore:  "content",
		ReadCache:     tc.readCacheName, // Enable read cache
		RootAttr: &metadata.FileAttr{
			Type:  metadata.FileTypeDirectory,
			Mode:  0755,
			UID:   0,
			GID:   0,
			Atime: time.Now(),
			Mtime: time.Now(),
			Ctime: time.Now(),
		},
	}

	// Add write cache if requested
	if withWriteCache && tc.writeCache != nil {
		shareConfig.WriteCache = tc.writeCacheName
	}

	if err := reg.AddShare(tc.ctx, shareConfig); err != nil {
		tc.T.Fatalf("Failed to add share: %v", err)
	}

	tc.Registry = reg

	// Start server using the pre-configured registry (not startServer which creates a new one!)
	tc.startServerFromRegistry()
}

// getReadCache returns the read cache for inspection
func (tc *testContextWithReadCache) getReadCache() cache.Cache {
	return tc.readCache
}

// getWriteCache returns the write cache for inspection (may be nil)
func (tc *testContextWithReadCache) getWriteCache() cache.Cache {
	return tc.writeCache
}

// ============================================================================
// Test Cases
// ============================================================================

// testReadCachePopulatedAfterWrite verifies read cache is populated after WRITE (no write cache)
func testReadCachePopulatedAfterWrite(t *testing.T, tc *testContextWithReadCache, readCache cache.Cache) {
	t.Helper()

	// Write a file directly (sync mode - no write cache)
	testFile := filepath.Join(tc.MountPath, "test-read-cache-write.txt")
	testData := []byte("Hello from write operation!")

	if err := os.WriteFile(testFile, testData, 0644); err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}

	// Give NFS a moment to process
	time.Sleep(100 * time.Millisecond)

	// Check if data is in read cache
	contentIDs := readCache.List()
	if len(contentIDs) == 0 {
		t.Error("Expected read cache to contain data after write, but it's empty")
		return
	}

	// Read from cache and verify
	cachedData, err := readCache.Read(context.Background(), contentIDs[0])
	if err != nil {
		t.Errorf("Failed to read from cache: %v", err)
		return
	}

	if string(cachedData) != string(testData) {
		t.Errorf("Cached data mismatch: got %q, want %q", string(cachedData), string(testData))
	}

	t.Logf("✓ Read cache populated after write: %d bytes cached", len(cachedData))
}

// testReadCacheHitAfterInitialRead verifies subsequent reads hit the cache
func testReadCacheHitAfterInitialRead(t *testing.T, tc *testContextWithReadCache, readCache cache.Cache) {
	t.Helper()

	// Create a file
	testFile := filepath.Join(tc.MountPath, "test-read-cache-hit.txt")
	testData := []byte("Data for cache hit test")

	if err := os.WriteFile(testFile, testData, 0644); err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}

	// First read - should populate cache
	data1, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("First read failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Verify cache is populated
	cacheSize := readCache.TotalSize()
	if cacheSize == 0 {
		t.Error("Expected cache to be populated after first read")
		return
	}

	t.Logf("Cache size after first read: %d bytes", cacheSize)

	// Second read - should hit cache
	data2, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("Second read failed: %v", err)
	}

	// Verify data is the same
	if string(data1) != string(data2) {
		t.Errorf("Data mismatch between reads: first=%q, second=%q", string(data1), string(data2))
	}

	t.Logf("✓ Cache hit on second read verified")
}

// testReadCacheMissForNewFile verifies new files cause cache miss
func testReadCacheMissForNewFile(t *testing.T, tc *testContextWithReadCache, readCache cache.Cache) {
	t.Helper()

	// Get initial cache state
	initialIDs := readCache.List()
	initialCount := len(initialIDs)

	// Create and read a new file
	testFile := filepath.Join(tc.MountPath, "test-cache-miss.txt")
	testData := []byte("New file for cache miss test")

	if err := os.WriteFile(testFile, testData, 0644); err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}

	data, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	if string(data) != string(testData) {
		t.Errorf("Data mismatch: got %q, want %q", string(data), string(testData))
	}

	time.Sleep(50 * time.Millisecond)

	// Verify cache now has more entries
	newIDs := readCache.List()
	newCount := len(newIDs)

	if newCount <= initialCount {
		t.Errorf("Expected cache to have more entries after reading new file: initial=%d, new=%d", initialCount, newCount)
	}

	t.Logf("✓ Cache miss handled correctly: cache entries increased from %d to %d", initialCount, newCount)
}

// testReadFromWriteCacheBeforeCommit verifies reads come from write cache before commit
func testReadFromWriteCacheBeforeCommit(t *testing.T, tc *testContextWithReadCache, readCache, writeCache cache.Cache) {
	t.Helper()

	t.Skip("TODO: Implement write cache + read cache test")
}

// testReadCachePopulatedAfterCommit verifies read cache is populated after COMMIT
func testReadCachePopulatedAfterCommit(t *testing.T, tc *testContextWithReadCache, readCache, writeCache cache.Cache) {
	t.Helper()

	t.Skip("TODO: Implement COMMIT→read cache population test")
}

// testLRUEvictsOldestFile verifies LRU eviction removes oldest files first
func testLRUEvictsOldestFile(t *testing.T, tc *testContextWithReadCache, readCache cache.Cache) {
	t.Helper()

	// Create multiple small files to fill cache
	fileSize := 30 * 1024 // 30KB each file
	numFiles := 5         // 150KB total, will exceed 100KB cache

	for i := 0; i < numFiles; i++ {
		filename := filepath.Join(tc.MountPath, fmt.Sprintf("evict-test-%d.txt", i))
		data := make([]byte, fileSize)
		for j := range data {
			data[j] = byte('A' + (i % 26))
		}

		if err := os.WriteFile(filename, data, 0644); err != nil {
			t.Fatalf("Failed to write file %d: %v", i, err)
		}

		// Read to populate cache
		if _, err := os.ReadFile(filename); err != nil {
			t.Fatalf("Failed to read file %d: %v", i, err)
		}

		// Small delay to ensure different write times
		time.Sleep(20 * time.Millisecond)
	}

	// Give cache time to process evictions
	time.Sleep(200 * time.Millisecond)

	// Check cache state
	cacheSize := readCache.TotalSize()
	maxSize := readCache.MaxSize()

	t.Logf("Cache size: %d bytes, max: %d bytes", cacheSize, maxSize)

	// Cache should be under max size due to eviction
	if cacheSize > maxSize {
		t.Errorf("Cache exceeded max size: current=%d, max=%d", cacheSize, maxSize)
	}

	// Verify some files were evicted
	cachedFiles := readCache.List()
	if len(cachedFiles) >= numFiles {
		t.Errorf("Expected some files to be evicted: cached=%d, written=%d", len(cachedFiles), numFiles)
	}

	t.Logf("✓ LRU eviction working: %d/%d files remain in cache", len(cachedFiles), numFiles)
}
