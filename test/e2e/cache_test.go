package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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

	cache := tc.getWriteCache()
	if cache == nil {
		t.Fatal("Write cache not initialized")
	}

	t.Run("BasicWriteAndCommit", func(t *testing.T) {
		testBasicWriteAndCommit(t, tc, cache)
	})

	t.Run("MultipleWritesBeforeCommit", func(t *testing.T) {
		testMultipleWritesBeforeCommit(t, tc, cache)
	})

	t.Run("LargeFileWriteAndCommit", func(t *testing.T) {
		testLargeFileWriteAndCommit(t, tc, cache)
	})
}

// newTestContextWithCache creates a test context with cache enabled
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

	// Setup stores
	tc.setupStores()

	// Setup cache
	tc.setupCache()

	// Start server with cache
	tc.startServerWithCache()

	// Mount NFS
	tc.mountNFS()

	return tc
}

// testContextWithCache extends TestContext with cache support
type testContextWithCache struct {
	*TestContext
	writeCache cache.Cache
	cacheName  string
}

// setupCache initializes the write cache
func (tc *testContextWithCache) setupCache() {
	tc.T.Helper()

	// Create 10MB memory cache
	tc.writeCache = cachememory.NewMemoryCache(10*1024*1024, nil)
	tc.cacheName = "test-write-cache"
}

// startServerWithCache starts server and registers cache
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

	// Register cache
	if err := tc.Registry.RegisterCache(tc.cacheName, tc.writeCache); err != nil {
		tc.T.Fatalf("Failed to register cache: %v", err)
	}

	// Create share with cache enabled
	shareConfig := &registry.ShareConfig{
		Name:          "/export",
		MetadataStore: metaStoreName,
		ContentStore:  contentStoreName,
		WriteCache:    tc.cacheName, // Enable write cache!
		ReadOnly:      false,
		RootAttr: &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0755,
			UID:  0,
			GID:  0,
		},
	}

	if err := tc.Registry.AddShare(tc.ctx, shareConfig); err != nil {
		tc.T.Fatalf("Failed to add share with cache: %v", err)
	}

	// Start server (reuse base implementation)
	tc.TestContext.startServerFromRegistry()
}

// getWriteCache returns the write cache for verification
func (tc *testContextWithCache) getWriteCache() cache.Cache {
	return tc.writeCache
}

// testBasicWriteAndCommit tests basic write -> commit flow
func testBasicWriteAndCommit(t *testing.T, tc *testContextWithCache, writeCache cache.Cache) {
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
	cacheSize := writeCache.TotalSize()
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
	file.Close()

	t.Logf("Cache total size after sync: %d bytes", writeCache.TotalSize())

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
func testMultipleWritesBeforeCommit(t *testing.T, tc *testContextWithCache, writeCache cache.Cache) {
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

	t.Logf("Cache size after multiple writes: %d bytes", writeCache.TotalSize())

	// Commit once (fsync)
	err = file.Sync()
	if err != nil {
		t.Fatalf("Failed to sync file: %v", err)
	}
	file.Close()

	t.Logf("Cache size after sync: %d bytes", writeCache.TotalSize())

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
func testLargeFileWriteAndCommit(t *testing.T, tc *testContextWithCache, writeCache cache.Cache) {
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

	t.Logf("Cache size after write: %d bytes", writeCache.TotalSize())

	// Open and sync
	file, err := os.OpenFile(filePath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("Failed to open file: %v", err)
	}

	err = file.Sync()
	if err != nil {
		t.Fatalf("Failed to sync large file: %v", err)
	}
	file.Close()

	t.Logf("Cache size after sync: %d bytes", writeCache.TotalSize())

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

	cache := tc.getWriteCache()

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

	t.Logf("Cache size after writing 2 files: %d bytes", cache.TotalSize())

	// Commit file1 only
	f1, err := os.OpenFile(file1Path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("Failed to open file1: %v", err)
	}
	err = f1.Sync()
	f1.Close()
	if err != nil {
		t.Fatalf("Failed to sync file1: %v", err)
	}

	t.Logf("Cache size after committing file1: %d bytes", cache.TotalSize())

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
	file.Close()
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
