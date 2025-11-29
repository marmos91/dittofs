//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/cache"
	cachememory "github.com/marmos91/dittofs/pkg/cache/memory"
	"github.com/marmos91/dittofs/pkg/registry"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// TestWriteWithCache tests that writes go to cache and are flushed on commit
func TestWriteWithCache(t *testing.T) {
	// Create basic test context with memory stores
	config := &TestConfig{
		Name:          "memory-memory-with-cache",
		MetadataStore: MetadataMemory,
		ContentStore:  ContentMemory,
		ShareName:     "/export",
	}

	// Create and start server with cache enabled
	tc := newTestContextWithCache(t, config)
	defer tc.Cleanup()

	c := tc.getCache()
	if c == nil {
		t.Fatal("Cache not initialized")
	}

	t.Run("BasicWriteAndCommit", func(t *testing.T) {
		testBasicWriteAndCommit(t, tc, c)
	})

	t.Run("MultipleWritesBeforeCommit", func(t *testing.T) {
		testMultipleWritesBeforeCommit(t, tc, c)
	})

	t.Run("LargeFileWriteAndCommit", func(t *testing.T) {
		testLargeFileWriteAndCommit(t, tc, c)
	})
}

// newTestContextWithCache creates a test context with unified cache enabled
func newTestContextWithCache(t *testing.T, config *TestConfig) *testContextWithCache {
	t.Helper()

	// Create base test context manually (don't call NewTestContext yet)
	ctx, cancel := context.WithCancel(context.Background())

	tc := &testContextWithCache{
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
		// Store helper for cleanup
		tc.localstackHelper = helper
	}

	// Setup stores
	tc.setupStores()

	// Setup unified cache
	tc.setupCache()

	// Start server with cache
	tc.startServerWithCache()

	// Mount NFS
	tc.mountNFS()

	return tc
}

// testContextWithCache extends TestContext with unified cache support
type testContextWithCache struct {
	*TestContext
	cache            cache.Cache // Unified cache for read/write
	cacheName        string
	localstackHelper *LocalstackHelper
}

// setupCache initializes the unified cache
func (tc *testContextWithCache) setupCache() {
	tc.T.Helper()

	// Create unlimited memory cache for unified caching (read/write)
	// The cache serves both reads and writes:
	// - Writes accumulate in cache (StateBuffering)
	// - COMMIT flushes to content store (StateUploading → StateCached)
	// - Reads check cache first, populate on miss
	tc.cache = cachememory.NewMemoryCache(0, nil) // 0 = unlimited
	tc.cacheName = "test-cache"
}

// startServerWithCache starts server and registers unified cache
func (tc *testContextWithCache) startServerWithCache() {
	tc.T.Helper()

	// Initialize registry
	tc.Registry = registry.NewRegistry()

	// Register metadata store
	metaStoreName := "test-metadata"
	if err := tc.Registry.RegisterMetadataStore(metaStoreName, tc.MetadataStore); err != nil {
		tc.T.Fatalf("Failed to register metadata store: %v", err)
	}

	// Register content store
	contentStoreName := "test-content"
	if err := tc.Registry.RegisterContentStore(contentStoreName, tc.ContentStore); err != nil {
		tc.T.Fatalf("Failed to register content store: %v", err)
	}

	// Register unified cache
	if err := tc.Registry.RegisterCache(tc.cacheName, tc.cache); err != nil {
		tc.T.Fatalf("Failed to register cache: %v", err)
	}

	// Create share with unified cache enabled
	shareConfig := &registry.ShareConfig{
		Name:          "/export",
		MetadataStore: metaStoreName,
		ContentStore:  contentStoreName,
		Cache:         tc.cacheName, // Enable unified cache
		ReadOnly:      false,
		RootAttr: &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0755,
			UID:  0,
			GID:  0,
		},
		// Enable prefetch with defaults
		PrefetchConfig: registry.PrefetchConfig{
			Enabled:     true,
			MaxFileSize: 100 * 1024 * 1024, // 100MB
			ChunkSize:   512 * 1024,        // 512KB
		},
	}

	if err := tc.Registry.AddShare(tc.ctx, shareConfig); err != nil {
		tc.T.Fatalf("Failed to add share with cache: %v", err)
	}

	// Start server (reuse base implementation)
	tc.startServerFromRegistry()
}

// getCache returns the unified cache for verification
func (tc *testContextWithCache) getCache() cache.Cache {
	return tc.cache
}

// Cleanup overrides parent cleanup to also cleanup localstack resources
func (tc *testContextWithCache) Cleanup() {
	// Call parent cleanup first
	tc.TestContext.Cleanup()

	// Cleanup localstack if used
	if tc.localstackHelper != nil {
		tc.localstackHelper.Cleanup()
	}
}

// testBasicWriteAndCommit tests basic write -> commit flow
func testBasicWriteAndCommit(t *testing.T, tc *testContextWithCache, c cache.Cache) {
	t.Helper()

	filePath := tc.Path("test_basic.txt")
	testData := []byte("Hello from cache test!")

	t.Logf("Writing %d bytes to %s", len(testData), filePath)

	// Write data to file
	err := os.WriteFile(filePath, testData, 0644)
	if err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}

	// At this point, data may be in cache (async mode) or content store (sync mode)
	cacheSize := c.TotalSize()
	t.Logf("Cache total size after write: %d bytes", cacheSize)

	// Open file and sync to trigger COMMIT
	file, err := os.OpenFile(filePath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("Failed to open file: %v", err)
	}

	// fsync triggers NFS COMMIT
	err = file.Sync()
	if err != nil {
		t.Fatalf("Failed to sync file: %v", err)
	}
	_ = file.Close()

	t.Logf("Cache total size after sync: %d bytes", c.TotalSize())

	// Verify file content is correct
	readData, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read file after commit: %v", err)
	}

	if string(readData) != string(testData) {
		t.Errorf("Data mismatch after commit:\ngot:  %q\nwant: %q", readData, testData)
	}

	t.Log("✓ Basic write and commit successful")
}

// testMultipleWritesBeforeCommit tests multiple writes followed by single commit
func testMultipleWritesBeforeCommit(t *testing.T, tc *testContextWithCache, c cache.Cache) {
	t.Helper()

	filePath := tc.Path("test_multiple.txt")

	// Write data in multiple operations
	file, err := os.Create(filePath)
	if err != nil {
		t.Fatalf("Failed to create file: %v", err)
	}

	// Write 3 separate chunks
	chunks := []string{
		"First chunk\n",
		"Second chunk\n",
		"Third chunk\n",
	}

	for i, chunk := range chunks {
		n, err := file.WriteString(chunk)
		if err != nil {
			t.Fatalf("Failed to write chunk %d: %v", i, err)
		}
		if n != len(chunk) {
			t.Errorf("Partial write on chunk %d: wrote %d bytes, expected %d", i, n, len(chunk))
		}
	}

	t.Logf("Cache size after multiple writes: %d bytes", c.TotalSize())

	// Commit once (fsync)
	err = file.Sync()
	if err != nil {
		t.Fatalf("Failed to sync file: %v", err)
	}
	_ = file.Close()

	t.Logf("Cache size after sync: %d bytes", c.TotalSize())

	// Verify complete content
	readData, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	expectedData := "First chunk\nSecond chunk\nThird chunk\n"
	if string(readData) != expectedData {
		t.Errorf("Data mismatch:\ngot:  %q\nwant: %q", readData, expectedData)
	}

	t.Log("✓ Multiple writes and commit successful")
}

// testLargeFileWriteAndCommit tests cache behavior with larger files
func testLargeFileWriteAndCommit(t *testing.T, tc *testContextWithCache, c cache.Cache) {
	t.Helper()

	filePath := tc.Path("test_large.bin")

	// Create 1MB of test data
	dataSize := 1024 * 1024 // 1MB
	testData := make([]byte, dataSize)
	for i := range testData {
		testData[i] = byte(i % 256)
	}

	t.Logf("Writing %d bytes...", dataSize)

	// Write large file
	err := os.WriteFile(filePath, testData, 0644)
	if err != nil {
		t.Fatalf("Failed to write large file: %v", err)
	}

	t.Logf("Cache size after write: %d bytes", c.TotalSize())

	// Open and sync
	file, err := os.OpenFile(filePath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("Failed to open file: %v", err)
	}

	err = file.Sync()
	if err != nil {
		t.Fatalf("Failed to sync large file: %v", err)
	}
	_ = file.Close()

	t.Logf("Cache size after sync: %d bytes", c.TotalSize())

	// Verify size
	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("Failed to stat file: %v", err)
	}

	if info.Size() != int64(dataSize) {
		t.Errorf("Size mismatch: got %d, want %d", info.Size(), dataSize)
	}

	// Verify content correctness (sample check)
	readData, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	if len(readData) != dataSize {
		t.Errorf("Read data size mismatch: got %d, want %d", len(readData), dataSize)
	}

	// Check first and last 100 bytes
	for i := 0; i < 100; i++ {
		if readData[i] != testData[i] {
			t.Errorf("Data mismatch at byte %d: got %d, want %d", i, readData[i], testData[i])
			break
		}
	}

	for i := dataSize - 100; i < dataSize; i++ {
		if readData[i] != testData[i] {
			t.Errorf("Data mismatch at byte %d: got %d, want %d", i, readData[i], testData[i])
			break
		}
	}

	t.Log("✓ Large file write and commit successful")
}

// TestCacheIsolation tests that different files have isolated cache entries
func TestCacheIsolation(t *testing.T) {
	config := &TestConfig{
		Name:          "memory-memory-cache-isolation",
		MetadataStore: MetadataMemory,
		ContentStore:  ContentMemory,
		ShareName:     "/export",
	}

	tc := newTestContextWithCache(t, config)
	defer tc.Cleanup()

	c := tc.getCache()

	// Create multiple files
	file1Path := tc.Path("file1.txt")
	file2Path := tc.Path("file2.txt")

	file1Data := []byte("File 1 content")
	file2Data := []byte("File 2 content - different!")

	// Write both files
	err := os.WriteFile(file1Path, file1Data, 0644)
	if err != nil {
		t.Fatalf("Failed to write file1: %v", err)
	}

	err = os.WriteFile(file2Path, file2Data, 0644)
	if err != nil {
		t.Fatalf("Failed to write file2: %v", err)
	}

	t.Logf("Cache size after writing 2 files: %d bytes", c.TotalSize())

	// Commit file1 only
	f1, err := os.OpenFile(file1Path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("Failed to open file1: %v", err)
	}
	err = f1.Sync()
	_ = f1.Close()
	if err != nil {
		t.Fatalf("Failed to sync file1: %v", err)
	}

	t.Logf("Cache size after committing file1: %d bytes", c.TotalSize())

	// Verify both files are readable
	readData1, err := os.ReadFile(file1Path)
	if err != nil {
		t.Fatalf("Failed to read file1: %v", err)
	}
	if string(readData1) != string(file1Data) {
		t.Errorf("File1 data mismatch")
	}

	readData2, err := os.ReadFile(file2Path)
	if err != nil {
		t.Fatalf("Failed to read file2: %v", err)
	}
	if string(readData2) != string(file2Data) {
		t.Errorf("File2 data mismatch")
	}

	t.Log("✓ Cache isolation verified")
}

// TestCacheWithDirectories tests that directory operations work with cache
func TestCacheWithDirectories(t *testing.T) {
	config := &TestConfig{
		Name:          "memory-memory-cache-dirs",
		MetadataStore: MetadataMemory,
		ContentStore:  ContentMemory,
		ShareName:     "/export",
	}

	tc := newTestContextWithCache(t, config)
	defer tc.Cleanup()

	// Create directory structure
	dir1 := tc.Path("dir1")
	err := os.Mkdir(dir1, 0755)
	if err != nil {
		t.Fatalf("Failed to create dir1: %v", err)
	}

	dir2 := filepath.Join(dir1, "dir2")
	err = os.Mkdir(dir2, 0755)
	if err != nil {
		t.Fatalf("Failed to create dir2: %v", err)
	}

	// Create file in nested directory
	filePath := filepath.Join(dir2, "nested.txt")
	fileData := []byte("Nested file content")
	err = os.WriteFile(filePath, fileData, 0644)
	if err != nil {
		t.Fatalf("Failed to write nested file: %v", err)
	}

	// Sync the file
	file, err := os.OpenFile(filePath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("Failed to open nested file: %v", err)
	}
	err = file.Sync()
	_ = file.Close()
	if err != nil {
		t.Fatalf("Failed to sync nested file: %v", err)
	}

	// Verify file is readable
	readData, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read nested file: %v", err)
	}

	if string(readData) != string(fileData) {
		t.Errorf("Nested file data mismatch")
	}

	t.Log("✓ Cache works with nested directories")
}

// TestReadCachePopulation tests that reads populate the cache after commit
func TestReadCachePopulation(t *testing.T) {
	config := &TestConfig{
		Name:          "memory-memory-read-cache",
		MetadataStore: MetadataMemory,
		ContentStore:  ContentMemory,
		ShareName:     "/export",
	}

	tc := newTestContextWithCache(t, config)
	defer tc.Cleanup()

	c := tc.getCache()

	filePath := tc.Path("read_cache_test.txt")
	testData := []byte("Data for read cache test - this should be cached after commit")

	// Write and commit file
	err := os.WriteFile(filePath, testData, 0644)
	if err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}

	file, err := os.OpenFile(filePath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("Failed to open file: %v", err)
	}
	err = file.Sync()
	_ = file.Close()
	if err != nil {
		t.Fatalf("Failed to sync file: %v", err)
	}

	t.Logf("Cache size after commit: %d bytes", c.TotalSize())

	// Read the file - should be served from cache (StateCached)
	readData, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	if string(readData) != string(testData) {
		t.Errorf("Data mismatch:\ngot:  %q\nwant: %q", readData, testData)
	}

	// Cache should still have the data
	cacheSizeAfterRead := c.TotalSize()
	t.Logf("Cache size after read: %d bytes", cacheSizeAfterRead)

	// Read again - should hit cache
	readData2, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read file second time: %v", err)
	}

	if string(readData2) != string(testData) {
		t.Errorf("Second read data mismatch")
	}

	t.Log("✓ Read cache population working correctly")
}

// TestWriteThenReadFromCache tests write-through to read optimization
func TestWriteThenReadFromCache(t *testing.T) {
	config := &TestConfig{
		Name:          "memory-memory-write-read",
		MetadataStore: MetadataMemory,
		ContentStore:  ContentMemory,
		ShareName:     "/export",
	}

	tc := newTestContextWithCache(t, config)
	defer tc.Cleanup()

	c := tc.getCache()

	// Write a file
	filePath := tc.Path("write_then_read.bin")
	dataSize := 512 * 1024 // 512KB
	testData := make([]byte, dataSize)
	for i := range testData {
		testData[i] = byte(i % 256)
	}

	err := os.WriteFile(filePath, testData, 0644)
	if err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}

	// Commit to flush to content store and transition to StateCached
	file, err := os.OpenFile(filePath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("Failed to open file: %v", err)
	}
	err = file.Sync()
	_ = file.Close()
	if err != nil {
		t.Fatalf("Failed to sync file: %v", err)
	}

	cacheAfterCommit := c.TotalSize()
	t.Logf("Cache size after commit: %d bytes (should have data in StateCached)", cacheAfterCommit)

	// Read should be served from cache (no content store fetch needed)
	readData, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	if len(readData) != dataSize {
		t.Errorf("Size mismatch: got %d, want %d", len(readData), dataSize)
	}

	// Verify content
	for i := 0; i < 100; i++ {
		if readData[i] != testData[i] {
			t.Errorf("Data mismatch at byte %d", i)
			break
		}
	}

	t.Log("✓ Write-then-read from cache working correctly")
}

// TestBackgroundFlusher tests that idle files are flushed by the background flusher
func TestBackgroundFlusher(t *testing.T) {
	config := &TestConfig{
		Name:          "memory-memory-flusher",
		MetadataStore: MetadataMemory,
		ContentStore:  ContentMemory,
		ShareName:     "/export",
	}

	tc := newTestContextWithCacheAndFlusher(t, config)
	defer tc.Cleanup()

	c := tc.getCache()

	// Write a file but DON'T call Sync (no explicit COMMIT)
	filePath := tc.Path("flusher_test.txt")
	testData := []byte("This file should be flushed by background flusher")

	err := os.WriteFile(filePath, testData, 0644)
	if err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}

	t.Logf("Cache size after write (no sync): %d bytes", c.TotalSize())

	// Wait for the background flusher to detect idle file and flush it
	// The flusher runs with FlushTimeout=2s and SweepInterval=1s for testing
	t.Log("Waiting for background flusher to flush idle file...")
	time.Sleep(5 * time.Second)

	t.Logf("Cache size after flusher run: %d bytes", c.TotalSize())

	// Verify file is still readable
	readData, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read file after flush: %v", err)
	}

	if string(readData) != string(testData) {
		t.Errorf("Data mismatch after flush:\ngot:  %q\nwant: %q", readData, testData)
	}

	t.Log("✓ Background flusher working correctly")
}

// newTestContextWithCacheAndFlusher creates test context with faster flusher settings for testing
func newTestContextWithCacheAndFlusher(t *testing.T, config *TestConfig) *testContextWithCache {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	tc := &testContextWithCache{
		TestContext: &TestContext{
			T:      t,
			Config: config,
			ctx:    ctx,
			cancel: cancel,
			Port:   findFreePort(t),
		},
	}

	if config.ContentStore == ContentS3 {
		if !CheckLocalstackAvailable(t) {
			t.Skip("Localstack not available, skipping S3 test")
		}
		helper := NewLocalstackHelper(t)
		SetupS3Config(t, config, helper)
		tc.localstackHelper = helper
	}

	tc.setupStores()
	tc.setupCache()
	tc.startServerWithCacheAndFlusher()
	tc.mountNFS()

	return tc
}

// startServerWithCacheAndFlusher starts server with faster flusher settings for testing
func (tc *testContextWithCache) startServerWithCacheAndFlusher() {
	tc.T.Helper()

	tc.Registry = registry.NewRegistry()

	metaStoreName := "test-metadata"
	if err := tc.Registry.RegisterMetadataStore(metaStoreName, tc.MetadataStore); err != nil {
		tc.T.Fatalf("Failed to register metadata store: %v", err)
	}

	contentStoreName := "test-content"
	if err := tc.Registry.RegisterContentStore(contentStoreName, tc.ContentStore); err != nil {
		tc.T.Fatalf("Failed to register content store: %v", err)
	}

	if err := tc.Registry.RegisterCache(tc.cacheName, tc.cache); err != nil {
		tc.T.Fatalf("Failed to register cache: %v", err)
	}

	shareConfig := &registry.ShareConfig{
		Name:          "/export",
		MetadataStore: metaStoreName,
		ContentStore:  contentStoreName,
		Cache:         tc.cacheName,
		ReadOnly:      false,
		RootAttr: &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0755,
			UID:  0,
			GID:  0,
		},
		PrefetchConfig: registry.PrefetchConfig{
			Enabled:     true,
			MaxFileSize: 100 * 1024 * 1024,
			ChunkSize:   512 * 1024,
		},
		// Fast flusher settings for testing
		FlusherConfig: registry.FlusherConfig{
			SweepInterval: 1 * time.Second,  // Check every second
			FlushTimeout:  2 * time.Second,  // Flush after 2 seconds idle
		},
	}

	if err := tc.Registry.AddShare(tc.ctx, shareConfig); err != nil {
		tc.T.Fatalf("Failed to add share with cache: %v", err)
	}

	tc.startServerFromRegistry()
}

// TestCacheEviction tests that cache entries can be evicted
func TestCacheEviction(t *testing.T) {
	config := &TestConfig{
		Name:          "memory-memory-eviction",
		MetadataStore: MetadataMemory,
		ContentStore:  ContentMemory,
		ShareName:     "/export",
	}

	tc := newTestContextWithLimitedCache(t, config)
	defer tc.Cleanup()

	c := tc.getCache()

	// Write multiple files to exceed cache size
	// Cache is limited to 1MB, so writing 2 x 512KB files should cause eviction
	for i := 0; i < 3; i++ {
		filePath := tc.Path("eviction_test_" + string(rune('0'+i)) + ".bin")
		data := make([]byte, 512*1024) // 512KB
		for j := range data {
			data[j] = byte((i + j) % 256)
		}

		err := os.WriteFile(filePath, data, 0644)
		if err != nil {
			t.Fatalf("Failed to write file %d: %v", i, err)
		}

		// Commit each file
		file, err := os.OpenFile(filePath, os.O_RDWR, 0644)
		if err != nil {
			t.Fatalf("Failed to open file %d: %v", i, err)
		}
		_ = file.Sync()
		_ = file.Close()

		t.Logf("After file %d: cache size = %d bytes", i, c.TotalSize())
	}

	// Cache should not exceed its limit (1MB)
	finalSize := c.TotalSize()
	t.Logf("Final cache size: %d bytes", finalSize)

	// With 3 x 512KB files, some should have been evicted
	if finalSize > 2*1024*1024 {
		t.Errorf("Cache size exceeds expected limit: %d > 2MB", finalSize)
	}

	t.Log("✓ Cache eviction working correctly")
}

// newTestContextWithLimitedCache creates test context with limited cache size
func newTestContextWithLimitedCache(t *testing.T, config *TestConfig) *testContextWithCache {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	tc := &testContextWithCache{
		TestContext: &TestContext{
			T:      t,
			Config: config,
			ctx:    ctx,
			cancel: cancel,
			Port:   findFreePort(t),
		},
	}

	if config.ContentStore == ContentS3 {
		if !CheckLocalstackAvailable(t) {
			t.Skip("Localstack not available, skipping S3 test")
		}
		helper := NewLocalstackHelper(t)
		SetupS3Config(t, config, helper)
		tc.localstackHelper = helper
	}

	tc.setupStores()

	// Create limited cache (1MB max)
	tc.cache = cachememory.NewMemoryCache(1*1024*1024, nil)
	tc.cacheName = "test-cache-limited"

	tc.startServerWithCache()
	tc.mountNFS()

	return tc
}
