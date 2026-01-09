package registry

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/cache"
	cacheMemory "github.com/marmos91/dittofs/pkg/cache/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadataMemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	contentMemory "github.com/marmos91/dittofs/pkg/store/content/memory"
)

// Helper to create memory content store for testing
func mustCreateMemoryContentStore() *contentMemory.MemoryContentStore {
	store, err := contentMemory.NewMemoryContentStore(context.Background())
	if err != nil {
		panic(err)
	}
	return store
}

// Helper to create memory cache for testing
func mustCreateMemoryCache() cache.Cache {
	// 100MB cache for testing
	return cacheMemory.NewMemoryCache(100*1024*1024, nil)
}

// Helper to create a basic ShareConfig for testing
func testShareConfig(name, metadataStore, contentStore string, readOnly bool) *ShareConfig {
	return &ShareConfig{
		Name:          name,
		MetadataStore: metadataStore,
		ContentStore:  contentStore,
		ReadOnly:      readOnly,
		RootAttr:      &metadata.FileAttr{}, // Empty attr, AddShare will apply defaults
	}
}

// Helper to create ShareConfig with cache configuration
func testShareConfigWithCache(name, metadataStore, contentStore, cache string, readOnly bool) *ShareConfig {
	return &ShareConfig{
		Name:          name,
		MetadataStore: metadataStore,
		ContentStore:  contentStore,
		Cache:         cache,
		ReadOnly:      readOnly,
		RootAttr:      &metadata.FileAttr{}, // Empty attr, AddShare will apply defaults
	}
}

func TestNewRegistry(t *testing.T) {
	reg := NewRegistry()
	if reg == nil {
		t.Fatal("NewRegistry returned nil")
	}
	if reg.CountMetadataStores() != 0 {
		t.Errorf("Expected 0 metadata stores, got %d", reg.CountMetadataStores())
	}
	if reg.CountContentStores() != 0 {
		t.Errorf("Expected 0 content stores, got %d", reg.CountContentStores())
	}
	if reg.CountCaches() != 0 {
		t.Errorf("Expected 0 caches, got %d", reg.CountCaches())
	}
	if reg.CountShares() != 0 {
		t.Errorf("Expected 0 shares, got %d", reg.CountShares())
	}
}

func TestRegisterMetadataStore(t *testing.T) {
	reg := NewRegistry()
	store := metadataMemory.NewMemoryMetadataStoreWithDefaults()

	// Test successful registration
	err := reg.RegisterMetadataStore("test-meta", store)
	if err != nil {
		t.Fatalf("Failed to register metadata store: %v", err)
	}

	if reg.CountMetadataStores() != 1 {
		t.Errorf("Expected 1 metadata store, got %d", reg.CountMetadataStores())
	}

	// Test duplicate registration
	err = reg.RegisterMetadataStore("test-meta", store)
	if err == nil {
		t.Error("Expected error when registering duplicate metadata store")
	}

	// Test nil store
	err = reg.RegisterMetadataStore("nil-store", nil)
	if err == nil {
		t.Error("Expected error when registering nil metadata store")
	}

	// Test empty name
	err = reg.RegisterMetadataStore("", store)
	if err == nil {
		t.Error("Expected error when registering metadata store with empty name")
	}
}

func TestRegisterContentStore(t *testing.T) {
	reg := NewRegistry()
	store := mustCreateMemoryContentStore()

	// Test successful registration
	err := reg.RegisterContentStore("test-content", store)
	if err != nil {
		t.Fatalf("Failed to register content store: %v", err)
	}

	if reg.CountContentStores() != 1 {
		t.Errorf("Expected 1 content store, got %d", reg.CountContentStores())
	}

	// Test duplicate registration
	err = reg.RegisterContentStore("test-content", store)
	if err == nil {
		t.Error("Expected error when registering duplicate content store")
	}

	// Test nil store
	err = reg.RegisterContentStore("nil-store", nil)
	if err == nil {
		t.Error("Expected error when registering nil content store")
	}

	// Test empty name
	err = reg.RegisterContentStore("", store)
	if err == nil {
		t.Error("Expected error when registering content store with empty name")
	}
}

func TestAddShare(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()
	contentStore := mustCreateMemoryContentStore()

	_ = reg.RegisterMetadataStore("meta1", metaStore)
	_ = reg.RegisterContentStore("content1", contentStore)

	// Test successful share creation
	ctx := context.Background()
	err := reg.AddShare(ctx, testShareConfig("/export", "meta1", "content1", false))
	if err != nil {
		t.Fatalf("Failed to add share: %v", err)
	}

	if reg.CountShares() != 1 {
		t.Errorf("Expected 1 share, got %d", reg.CountShares())
	}

	// Test duplicate share
	err = reg.AddShare(ctx, testShareConfig("/export", "meta1", "content1", false))
	if err == nil {
		t.Error("Expected error when adding duplicate share")
	}

	// Test non-existent metadata store
	err = reg.AddShare(ctx, testShareConfig("/export2", "nonexistent", "content1", false))
	if err == nil {
		t.Error("Expected error when adding share with non-existent metadata store")
	}

	// Test non-existent content store
	err = reg.AddShare(ctx, testShareConfig("/export2", "meta1", "nonexistent", false))
	if err == nil {
		t.Error("Expected error when adding share with non-existent content store")
	}

	// Test empty share name
	err = reg.AddShare(ctx, testShareConfig("", "meta1", "content1", false))
	if err == nil {
		t.Error("Expected error when adding share with empty name")
	}
}

func TestRemoveShare(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()
	contentStore := mustCreateMemoryContentStore()
	ctx := context.Background()

	_ = reg.RegisterMetadataStore("meta1", metaStore)
	_ = reg.RegisterContentStore("content1", contentStore)
	_ = reg.AddShare(ctx, testShareConfig("/export", "meta1", "content1", false))

	// Test successful removal
	err := reg.RemoveShare("/export")
	if err != nil {
		t.Fatalf("Failed to remove share: %v", err)
	}

	if reg.CountShares() != 0 {
		t.Errorf("Expected 0 shares after removal, got %d", reg.CountShares())
	}

	// Test removing non-existent share
	err = reg.RemoveShare("/export")
	if err == nil {
		t.Error("Expected error when removing non-existent share")
	}
}

func TestGetShare(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()
	contentStore := mustCreateMemoryContentStore()

	_ = reg.RegisterMetadataStore("meta1", metaStore)
	_ = reg.RegisterContentStore("content1", contentStore)
	_ = reg.AddShare(context.Background(), testShareConfig("/export", "meta1", "content1", true))

	// Test successful retrieval
	share, err := reg.GetShare("/export")
	if err != nil {
		t.Fatalf("Failed to get share: %v", err)
	}
	if share == nil {
		t.Fatal("GetShare returned nil share")
	}
	if share.Name != "/export" {
		t.Errorf("Expected share name '/export', got %q", share.Name)
	}
	if share.ReadOnly != true {
		t.Error("Expected share to be read-only")
	}
	if share.MetadataStore != "meta1" {
		t.Errorf("Expected metadata store 'meta1', got %q", share.MetadataStore)
	}
	if share.ContentStore != "content1" {
		t.Errorf("Expected content store 'content1', got %q", share.ContentStore)
	}

	// Test non-existent share
	_, err = reg.GetShare("/nonexistent")
	if err == nil {
		t.Error("Expected error when getting non-existent share")
	}
}

func TestGetMetadataStore(t *testing.T) {
	reg := NewRegistry()
	store := metadataMemory.NewMemoryMetadataStoreWithDefaults()

	_ = reg.RegisterMetadataStore("meta1", store)

	// Test successful retrieval
	retrieved, err := reg.GetMetadataStore("meta1")
	if err != nil {
		t.Fatalf("Failed to get metadata store: %v", err)
	}
	if retrieved != store {
		t.Error("Retrieved store is not the same as registered store")
	}

	// Test non-existent store
	_, err = reg.GetMetadataStore("nonexistent")
	if err == nil {
		t.Error("Expected error when getting non-existent metadata store")
	}
}

func TestGetContentStore(t *testing.T) {
	reg := NewRegistry()
	store := mustCreateMemoryContentStore()

	_ = reg.RegisterContentStore("content1", store)

	// Test successful retrieval
	retrieved, err := reg.GetContentStore("content1")
	if err != nil {
		t.Fatalf("Failed to get content store: %v", err)
	}
	if retrieved == nil {
		t.Error("Retrieved content store is nil")
	}

	// Test non-existent store
	_, err = reg.GetContentStore("nonexistent")
	if err == nil {
		t.Error("Expected error when getting non-existent content store")
	}
}

func TestGetStoresForShare(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()
	contentStore := mustCreateMemoryContentStore()

	_ = reg.RegisterMetadataStore("meta1", metaStore)
	_ = reg.RegisterContentStore("content1", contentStore)
	_ = reg.AddShare(context.Background(), testShareConfig("/export", "meta1", "content1", false))

	// Test getting metadata store for share
	ms, err := reg.GetMetadataStoreForShare("/export")
	if err != nil {
		t.Fatalf("Failed to get metadata store for share: %v", err)
	}
	if ms != metaStore {
		t.Error("Retrieved metadata store is not the same as registered store")
	}

	// Test getting content store for share
	cs, err := reg.GetContentStoreForShare("/export")
	if err != nil {
		t.Fatalf("Failed to get content store for share: %v", err)
	}
	if cs == nil {
		t.Error("Retrieved content store is nil")
	}

	// Test non-existent share
	_, err = reg.GetMetadataStoreForShare("/nonexistent")
	if err == nil {
		t.Error("Expected error when getting metadata store for non-existent share")
	}

	_, err = reg.GetContentStoreForShare("/nonexistent")
	if err == nil {
		t.Error("Expected error when getting content store for non-existent share")
	}
}

func TestListShares(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()
	contentStore := mustCreateMemoryContentStore()

	_ = reg.RegisterMetadataStore("meta1", metaStore)
	_ = reg.RegisterContentStore("content1", contentStore)

	// Empty list
	shares := reg.ListShares()
	if len(shares) != 0 {
		t.Errorf("Expected 0 shares, got %d", len(shares))
	}

	// Add shares
	_ = reg.AddShare(context.Background(), testShareConfig("/export1", "meta1", "content1", false))
	_ = reg.AddShare(context.Background(), testShareConfig("/export2", "meta1", "content1", false))
	_ = reg.AddShare(context.Background(), testShareConfig("/export3", "meta1", "content1", false))

	shares = reg.ListShares()
	if len(shares) != 3 {
		t.Errorf("Expected 3 shares, got %d", len(shares))
	}

	// Verify all names are present
	nameSet := make(map[string]bool)
	for _, name := range shares {
		nameSet[name] = true
	}
	if !nameSet["/export1"] || !nameSet["/export2"] || !nameSet["/export3"] {
		t.Error("Missing expected share names in list")
	}
}

func TestListStores(t *testing.T) {
	reg := NewRegistry()

	// Add metadata stores
	_ = reg.RegisterMetadataStore("meta1", metadataMemory.NewMemoryMetadataStoreWithDefaults())
	_ = reg.RegisterMetadataStore("meta2", metadataMemory.NewMemoryMetadataStoreWithDefaults())

	metaStores := reg.ListMetadataStores()
	if len(metaStores) != 2 {
		t.Errorf("Expected 2 metadata stores, got %d", len(metaStores))
	}

	// Add content stores
	_ = reg.RegisterContentStore("content1", mustCreateMemoryContentStore())
	_ = reg.RegisterContentStore("content2", mustCreateMemoryContentStore())
	_ = reg.RegisterContentStore("content3", mustCreateMemoryContentStore())

	contentStores := reg.ListContentStores()
	if len(contentStores) != 3 {
		t.Errorf("Expected 3 content stores, got %d", len(contentStores))
	}
}

func TestListSharesUsingStore(t *testing.T) {
	reg := NewRegistry()
	metaStore1 := metadataMemory.NewMemoryMetadataStoreWithDefaults()
	metaStore2 := metadataMemory.NewMemoryMetadataStoreWithDefaults()
	contentStore1 := mustCreateMemoryContentStore()
	contentStore2 := mustCreateMemoryContentStore()

	_ = reg.RegisterMetadataStore("meta1", metaStore1)
	_ = reg.RegisterMetadataStore("meta2", metaStore2)
	_ = reg.RegisterContentStore("content1", contentStore1)
	_ = reg.RegisterContentStore("content2", contentStore2)

	// Create shares with different store combinations
	_ = reg.AddShare(context.Background(), testShareConfig("/export1", "meta1", "content1", false))
	_ = reg.AddShare(context.Background(), testShareConfig("/export2", "meta1", "content2", false))
	_ = reg.AddShare(context.Background(), testShareConfig("/export3", "meta2", "content1", false))

	// Test metadata store usage
	sharesUsingMeta1 := reg.ListSharesUsingMetadataStore("meta1")
	if len(sharesUsingMeta1) != 2 {
		t.Errorf("Expected 2 shares using meta1, got %d", len(sharesUsingMeta1))
	}

	sharesUsingMeta2 := reg.ListSharesUsingMetadataStore("meta2")
	if len(sharesUsingMeta2) != 1 {
		t.Errorf("Expected 1 share using meta2, got %d", len(sharesUsingMeta2))
	}

	// Test content store usage
	sharesUsingContent1 := reg.ListSharesUsingContentStore("content1")
	if len(sharesUsingContent1) != 2 {
		t.Errorf("Expected 2 shares using content1, got %d", len(sharesUsingContent1))
	}

	sharesUsingContent2 := reg.ListSharesUsingContentStore("content2")
	if len(sharesUsingContent2) != 1 {
		t.Errorf("Expected 1 share using content2, got %d", len(sharesUsingContent2))
	}

	// Test non-existent store
	sharesUsingNonexistent := reg.ListSharesUsingMetadataStore("nonexistent")
	if len(sharesUsingNonexistent) != 0 {
		t.Errorf("Expected 0 shares using non-existent store, got %d", len(sharesUsingNonexistent))
	}
}

func TestMultipleSharesSameStore(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()
	contentStore := mustCreateMemoryContentStore()

	_ = reg.RegisterMetadataStore("shared-meta", metaStore)
	_ = reg.RegisterContentStore("shared-content", contentStore)

	// Create multiple shares using the same stores
	_ = reg.AddShare(context.Background(), testShareConfig("/export1", "shared-meta", "shared-content", false))
	_ = reg.AddShare(context.Background(), testShareConfig("/export2", "shared-meta", "shared-content", true))
	_ = reg.AddShare(context.Background(), testShareConfig("/export3", "shared-meta", "shared-content", false))

	if reg.CountShares() != 3 {
		t.Errorf("Expected 3 shares, got %d", reg.CountShares())
	}

	// Verify each share has correct configuration
	share1, _ := reg.GetShare("/export1")
	share2, _ := reg.GetShare("/export2")
	share3, _ := reg.GetShare("/export3")

	if share1.ReadOnly != false {
		t.Error("Share1 should not be read-only")
	}
	if share2.ReadOnly != true {
		t.Error("Share2 should be read-only")
	}
	if share3.ReadOnly != false {
		t.Error("Share3 should not be read-only")
	}

	// Verify all point to same store names
	if share1.MetadataStore != "shared-meta" || share2.MetadataStore != "shared-meta" || share3.MetadataStore != "shared-meta" {
		t.Error("All shares should use 'shared-meta' metadata store")
	}
}

func TestConcurrentAccess(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()
	contentStore := mustCreateMemoryContentStore()

	_ = reg.RegisterMetadataStore("meta1", metaStore)
	_ = reg.RegisterContentStore("content1", contentStore)
	_ = reg.AddShare(context.Background(), testShareConfig("/export", "meta1", "content1", false))

	// Simulate concurrent reads
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			_, _ = reg.GetShare("/export")
			_ = reg.ListShares()
			_, _ = reg.GetMetadataStoreForShare("/export")
			_, _ = reg.GetContentStoreForShare("/export")
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}
}

// ============================================================================
// Cache Tests
// ============================================================================

func TestRegisterCache(t *testing.T) {
	reg := NewRegistry()
	cache := mustCreateMemoryCache()

	// Test successful registration
	err := reg.RegisterCache("test-cache", cache)
	if err != nil {
		t.Fatalf("Failed to register cache: %v", err)
	}

	if reg.CountCaches() != 1 {
		t.Errorf("Expected 1 cache, got %d", reg.CountCaches())
	}

	// Test duplicate registration
	err = reg.RegisterCache("test-cache", cache)
	if err == nil {
		t.Error("Expected error when registering duplicate cache")
	}

	// Test nil cache
	err = reg.RegisterCache("nil-cache", nil)
	if err == nil {
		t.Error("Expected error when registering nil cache")
	}

	// Test empty name
	err = reg.RegisterCache("", cache)
	if err == nil {
		t.Error("Expected error when registering cache with empty name")
	}
}

func TestGetCache(t *testing.T) {
	reg := NewRegistry()
	cache := mustCreateMemoryCache()

	_ = reg.RegisterCache("cache1", cache)

	// Test successful retrieval
	retrieved, err := reg.GetCache("cache1")
	if err != nil {
		t.Fatalf("Failed to get cache: %v", err)
	}
	if retrieved != cache {
		t.Error("Retrieved cache is not the same as registered cache")
	}

	// Test non-existent cache
	_, err = reg.GetCache("nonexistent")
	if err == nil {
		t.Error("Expected error when getting non-existent cache")
	}
}

func TestAddShareWithCache(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()
	contentStore := mustCreateMemoryContentStore()
	testCache := mustCreateMemoryCache()

	_ = reg.RegisterMetadataStore("meta1", metaStore)
	_ = reg.RegisterContentStore("content1", contentStore)
	_ = reg.RegisterCache("test-cache", testCache)

	ctx := context.Background()

	// Test share with cache
	err := reg.AddShare(ctx, testShareConfigWithCache("/export1", "meta1", "content1", "test-cache", false))
	if err != nil {
		t.Fatalf("Failed to add share with cache: %v", err)
	}

	share1, _ := reg.GetShare("/export1")
	if share1.Cache != "test-cache" {
		t.Errorf("Expected cache 'test-cache', got %q", share1.Cache)
	}

	// Test share with no cache (sync mode)
	err = reg.AddShare(ctx, testShareConfig("/export2", "meta1", "content1", false))
	if err != nil {
		t.Fatalf("Failed to add share without cache: %v", err)
	}

	share2, _ := reg.GetShare("/export2")
	if share2.Cache != "" {
		t.Errorf("Expected empty cache, got %q", share2.Cache)
	}
}

func TestAddShareWithNonexistentCache(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()
	contentStore := mustCreateMemoryContentStore()

	_ = reg.RegisterMetadataStore("meta1", metaStore)
	_ = reg.RegisterContentStore("content1", contentStore)

	ctx := context.Background()

	// Test non-existent cache
	err := reg.AddShare(ctx, testShareConfigWithCache("/export1", "meta1", "content1", "nonexistent-cache", false))
	if err == nil {
		t.Error("Expected error when adding share with non-existent cache")
	}
}

func TestGetCacheForShare(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()
	contentStore := mustCreateMemoryContentStore()
	testCache := mustCreateMemoryCache()

	_ = reg.RegisterMetadataStore("meta1", metaStore)
	_ = reg.RegisterContentStore("content1", contentStore)
	_ = reg.RegisterCache("test-cache", testCache)

	ctx := context.Background()
	_ = reg.AddShare(ctx, testShareConfigWithCache("/export", "meta1", "content1", "test-cache", false))

	// Test getting cache for share
	c := reg.GetCacheForShare("/export")
	if c != testCache {
		t.Error("Retrieved cache is not the same as registered cache")
	}

	// Test non-existent share returns nil
	c = reg.GetCacheForShare("/nonexistent")
	if c != nil {
		t.Error("Expected nil cache for non-existent share")
	}
}

func TestGetCacheForShareWithNoCache(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()
	contentStore := mustCreateMemoryContentStore()

	_ = reg.RegisterMetadataStore("meta1", metaStore)
	_ = reg.RegisterContentStore("content1", contentStore)

	ctx := context.Background()
	_ = reg.AddShare(ctx, testShareConfig("/export", "meta1", "content1", false))

	// Test getting cache for share without cache (should return nil)
	c := reg.GetCacheForShare("/export")
	if c != nil {
		t.Error("Expected nil cache for share without cache configured")
	}
}

func TestListCaches(t *testing.T) {
	reg := NewRegistry()

	// Empty list
	caches := reg.ListCaches()
	if len(caches) != 0 {
		t.Errorf("Expected 0 caches, got %d", len(caches))
	}

	// Add caches
	_ = reg.RegisterCache("cache1", mustCreateMemoryCache())
	_ = reg.RegisterCache("cache2", mustCreateMemoryCache())
	_ = reg.RegisterCache("cache3", mustCreateMemoryCache())

	caches = reg.ListCaches()
	if len(caches) != 3 {
		t.Errorf("Expected 3 caches, got %d", len(caches))
	}

	// Verify all names are present
	nameSet := make(map[string]bool)
	for _, name := range caches {
		nameSet[name] = true
	}
	if !nameSet["cache1"] || !nameSet["cache2"] || !nameSet["cache3"] {
		t.Error("Missing expected cache names in list")
	}
}

func TestListSharesUsingCache(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()
	contentStore := mustCreateMemoryContentStore()
	cache1 := mustCreateMemoryCache()
	cache2 := mustCreateMemoryCache()

	_ = reg.RegisterMetadataStore("meta1", metaStore)
	_ = reg.RegisterContentStore("content1", contentStore)
	_ = reg.RegisterCache("cache1", cache1)
	_ = reg.RegisterCache("cache2", cache2)

	ctx := context.Background()

	// Create shares with different cache configurations
	_ = reg.AddShare(ctx, testShareConfigWithCache("/export1", "meta1", "content1", "cache1", false))
	_ = reg.AddShare(ctx, testShareConfigWithCache("/export2", "meta1", "content1", "cache1", false))
	_ = reg.AddShare(ctx, testShareConfigWithCache("/export3", "meta1", "content1", "cache2", false))
	_ = reg.AddShare(ctx, testShareConfig("/export4", "meta1", "content1", false)) // No cache

	// Test cache usage
	sharesUsingCache1 := reg.ListSharesUsingCache("cache1")
	if len(sharesUsingCache1) != 2 {
		t.Errorf("Expected 2 shares using cache1, got %d", len(sharesUsingCache1))
	}

	sharesUsingCache2 := reg.ListSharesUsingCache("cache2")
	if len(sharesUsingCache2) != 1 {
		t.Errorf("Expected 1 share using cache2, got %d", len(sharesUsingCache2))
	}

	// Test non-existent cache
	sharesUsingNonexistent := reg.ListSharesUsingCache("nonexistent")
	if len(sharesUsingNonexistent) != 0 {
		t.Errorf("Expected 0 shares using non-existent cache, got %d", len(sharesUsingNonexistent))
	}
}

func TestMultipleSharesSameCache(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()
	contentStore := mustCreateMemoryContentStore()
	sharedCache := mustCreateMemoryCache()

	_ = reg.RegisterMetadataStore("meta1", metaStore)
	_ = reg.RegisterContentStore("content1", contentStore)
	_ = reg.RegisterCache("shared-cache", sharedCache)

	ctx := context.Background()

	// Create multiple shares using the same cache
	_ = reg.AddShare(ctx, testShareConfigWithCache("/export1", "meta1", "content1", "shared-cache", false))
	_ = reg.AddShare(ctx, testShareConfigWithCache("/export2", "meta1", "content1", "shared-cache", false))
	_ = reg.AddShare(ctx, testShareConfigWithCache("/export3", "meta1", "content1", "shared-cache", false))

	if reg.CountShares() != 3 {
		t.Errorf("Expected 3 shares, got %d", reg.CountShares())
	}

	// Verify all point to same cache name
	share1, _ := reg.GetShare("/export1")
	share2, _ := reg.GetShare("/export2")
	share3, _ := reg.GetShare("/export3")

	if share1.Cache != "shared-cache" || share2.Cache != "shared-cache" || share3.Cache != "shared-cache" {
		t.Error("All shares should use 'shared-cache' cache")
	}

	// Verify they all resolve to the same cache instance
	cache1 := reg.GetCacheForShare("/export1")
	cache2 := reg.GetCacheForShare("/export2")
	cache3 := reg.GetCacheForShare("/export3")

	if cache1 != sharedCache || cache2 != sharedCache || cache3 != sharedCache {
		t.Error("All shares should resolve to the same cache instance")
	}
}
