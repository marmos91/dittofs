package badger

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// Test helpers

// createTestStore creates a BadgerMetadataStore with custom configuration for testing
func createTestStore(t *testing.T, config BadgerMetadataStoreConfig) *BadgerMetadataStore {
	t.Helper()
	ctx := context.Background()
	store, err := NewBadgerMetadataStore(ctx, config)
	if err != nil {
		t.Fatalf("Failed to create test store: %v", err)
	}
	return store
}

// createTestShare creates a test share and returns the share info
type testShare struct {
	Name       string
	RootHandle metadata.FileHandle
}

func createTestShare(t *testing.T, store *BadgerMetadataStore, name string) *testShare {
	t.Helper()
	ctx := context.Background()

	rootAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0777,
		UID:  0,
		GID:  0,
		Size: 4096,
	}

	err := store.AddShare(ctx, name, metadata.ShareOptions{
		ReadOnly:    false,
		Async:       false,
		RequireAuth: false,
	}, rootAttr)
	if err != nil {
		t.Fatalf("Failed to create test share: %v", err)
	}

	rootHandle, err := store.GetShareRoot(ctx, name)
	if err != nil {
		t.Fatalf("Failed to get share root: %v", err)
	}

	return &testShare{
		Name:       name,
		RootHandle: rootHandle,
	}
}

// createAuthContext creates an authentication context for testing
func createAuthContext(clientAddr string) *metadata.AuthContext {
	uid := uint32(0)
	gid := uint32(0)
	return &metadata.AuthContext{
		Context:    context.Background(),
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: &uid,
			GID: &gid,
		},
		ClientAddr: clientAddr,
	}
}

// TestReadDirCache tests the ReadDirectory caching functionality
func TestReadDirCache(t *testing.T) {
	t.Run("CacheHit", func(t *testing.T) {
		store := createTestStore(t, BadgerMetadataStoreConfig{
			DBPath: t.TempDir(),
			Capabilities: metadata.FilesystemCapabilities{
				MaxReadSize: 1048576,
			},
			CacheEnabled:    true,
			CacheTTL:        5 * time.Second,
			CacheMaxEntries: 100,
		})
		defer func() { _ = store.Close() }()

		// Create test share and directory with files
		share := createTestShare(t, store, "/test")
		rootHandle := share.RootHandle
		ctx := createAuthContext("127.0.0.1")

		// Create some files
		for i := 0; i < 5; i++ {
			name := fmt.Sprintf("file%d.txt", i)
			_, err := store.Create(ctx, rootHandle, name, &metadata.FileAttr{
				Type: metadata.FileTypeRegular,
				Mode: 0644,
			})
			if err != nil {
				t.Fatalf("Failed to create file %s: %v", name, err)
			}
		}

		// First read - should be a cache miss
		initialMisses := store.readdirCache.misses
		page1, err := store.ReadDirectory(ctx, rootHandle, "", 1024*1024)
		if err != nil {
			t.Fatalf("First ReadDirectory failed: %v", err)
		}
		if store.readdirCache.misses != initialMisses+1 {
			t.Errorf("Expected cache miss on first read, misses: %d -> %d",
				initialMisses, store.readdirCache.misses)
		}

		// Second read - should be a cache hit
		initialHits := store.readdirCache.hits
		page2, err := store.ReadDirectory(ctx, rootHandle, "", 1024*1024)
		if err != nil {
			t.Fatalf("Second ReadDirectory failed: %v", err)
		}
		if store.readdirCache.hits != initialHits+1 {
			t.Errorf("Expected cache hit on second read, hits: %d -> %d",
				initialHits, store.readdirCache.hits)
		}

		// Verify same results
		if len(page1.Entries) != len(page2.Entries) {
			t.Errorf("Cache returned different number of entries: %d vs %d",
				len(page1.Entries), len(page2.Entries))
		}
	})

	t.Run("CacheInvalidationOnCreate", func(t *testing.T) {
		store := createTestStore(t, BadgerMetadataStoreConfig{
			DBPath: t.TempDir(),
			Capabilities: metadata.FilesystemCapabilities{
				MaxReadSize: 1048576,
			},
			CacheEnabled:           true,
			CacheTTL:               5 * time.Second,
			CacheMaxEntries:        100,
			CacheInvalidateOnWrite: true,
		})
		defer func() { _ = store.Close() }()

		share := createTestShare(t, store, "/test")
		rootHandle := share.RootHandle
		ctx := createAuthContext("127.0.0.1")

		// First read - populate cache
		page1, err := store.ReadDirectory(ctx, rootHandle, "", 1024*1024)
		if err != nil {
			t.Fatalf("First ReadDirectory failed: %v", err)
		}
		initialCount := len(page1.Entries)

		// Create a new file - should invalidate cache
		_, err = store.Create(ctx, rootHandle, "newfile.txt", &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
		})
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// Next read should be a cache miss (invalidated)
		initialMisses := store.readdirCache.misses
		page2, err := store.ReadDirectory(ctx, rootHandle, "", 1024*1024)
		if err != nil {
			t.Fatalf("Second ReadDirectory failed: %v", err)
		}
		if store.readdirCache.misses != initialMisses+1 {
			t.Errorf("Expected cache miss after create, misses: %d -> %d",
				initialMisses, store.readdirCache.misses)
		}

		// Should have one more entry
		if len(page2.Entries) != initialCount+1 {
			t.Errorf("Expected %d entries after create, got %d",
				initialCount+1, len(page2.Entries))
		}
	})

	t.Run("CacheInvalidationOnRemove", func(t *testing.T) {
		store := createTestStore(t, BadgerMetadataStoreConfig{
			DBPath: t.TempDir(),
			Capabilities: metadata.FilesystemCapabilities{
				MaxReadSize: 1048576,
			},
			CacheEnabled:           true,
			CacheTTL:               5 * time.Second,
			CacheMaxEntries:        100,
			CacheInvalidateOnWrite: true,
		})
		defer func() { _ = store.Close() }()

		share := createTestShare(t, store, "/test")
		rootHandle := share.RootHandle
		ctx := createAuthContext("127.0.0.1")

		// Create a file
		_, err := store.Create(ctx, rootHandle, "test.txt", &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
		})
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// Read directory - populate cache
		page1, err := store.ReadDirectory(ctx, rootHandle, "", 1024*1024)
		if err != nil {
			t.Fatalf("First ReadDirectory failed: %v", err)
		}
		initialCount := len(page1.Entries)

		// Remove the file - should invalidate cache
		_, err = store.RemoveFile(ctx, rootHandle, "test.txt")
		if err != nil {
			t.Fatalf("Failed to remove file: %v", err)
		}

		// Next read should be a cache miss
		initialMisses := store.readdirCache.misses
		page2, err := store.ReadDirectory(ctx, rootHandle, "", 1024*1024)
		if err != nil {
			t.Fatalf("Second ReadDirectory failed: %v", err)
		}
		if store.readdirCache.misses != initialMisses+1 {
			t.Errorf("Expected cache miss after remove, misses: %d -> %d",
				initialMisses, store.readdirCache.misses)
		}

		// Should have one fewer entry
		if len(page2.Entries) != initialCount-1 {
			t.Errorf("Expected %d entries after remove, got %d",
				initialCount-1, len(page2.Entries))
		}
	})

	t.Run("CacheTTLExpiration", func(t *testing.T) {
		store := createTestStore(t, BadgerMetadataStoreConfig{
			DBPath: t.TempDir(),
			Capabilities: metadata.FilesystemCapabilities{
				MaxReadSize: 1048576,
			},
			CacheEnabled:    true,
			CacheTTL:        100 * time.Millisecond, // Very short TTL
			CacheMaxEntries: 100,
		})
		defer func() { _ = store.Close() }()

		share := createTestShare(t, store, "/test")
		rootHandle := share.RootHandle
		ctx := createAuthContext("127.0.0.1")

		// First read - populate cache
		_, err := store.ReadDirectory(ctx, rootHandle, "", 1024*1024)
		if err != nil {
			t.Fatalf("First ReadDirectory failed: %v", err)
		}

		// Immediate second read - should hit cache
		initialHits := store.readdirCache.hits
		_, err = store.ReadDirectory(ctx, rootHandle, "", 1024*1024)
		if err != nil {
			t.Fatalf("Second ReadDirectory failed: %v", err)
		}
		if store.readdirCache.hits != initialHits+1 {
			t.Errorf("Expected cache hit before TTL expiration")
		}

		// Wait for TTL to expire
		time.Sleep(150 * time.Millisecond)

		// Third read - should miss cache (expired)
		initialMisses := store.readdirCache.misses
		_, err = store.ReadDirectory(ctx, rootHandle, "", 1024*1024)
		if err != nil {
			t.Fatalf("Third ReadDirectory failed: %v", err)
		}
		if store.readdirCache.misses != initialMisses+1 {
			t.Errorf("Expected cache miss after TTL expiration")
		}
	})

	t.Run("CacheLRUEviction", func(t *testing.T) {
		store := createTestStore(t, BadgerMetadataStoreConfig{
			DBPath: t.TempDir(),
			Capabilities: metadata.FilesystemCapabilities{
				MaxReadSize: 1048576,
			},
			CacheEnabled:    true,
			CacheTTL:        5 * time.Second,
			CacheMaxEntries: 2, // Very small cache
		})
		defer func() { _ = store.Close() }()

		share := createTestShare(t, store, "/test")
		rootHandle := share.RootHandle
		ctx := createAuthContext("127.0.0.1")

		// Create 3 subdirectories
		var dirs []metadata.FileHandle
		for i := 0; i < 3; i++ {
			name := fmt.Sprintf("dir%d", i)
			handle, err := store.Create(ctx, rootHandle, name, &metadata.FileAttr{
				Type: metadata.FileTypeDirectory,
				Mode: 0755,
			})
			if err != nil {
				t.Fatalf("Failed to create directory %s: %v", name, err)
			}
			dirs = append(dirs, handle)
		}

		// Read all 3 directories to populate cache
		for i, dir := range dirs {
			_, err := store.ReadDirectory(ctx, dir, "", 1024*1024)
			if err != nil {
				t.Fatalf("Failed to read directory %d: %v", i, err)
			}
		}

		// Cache should only hold 2 entries (LRU eviction)
		store.readdirCache.mu.RLock()
		cacheSize := len(store.readdirCache.cache)
		store.readdirCache.mu.RUnlock()

		if cacheSize != 2 {
			t.Errorf("Expected cache size 2 after LRU eviction, got %d", cacheSize)
		}

		// First directory should have been evicted, reading it should miss
		initialMisses := store.readdirCache.misses
		_, err := store.ReadDirectory(ctx, dirs[0], "", 1024*1024)
		if err != nil {
			t.Fatalf("Failed to re-read first directory: %v", err)
		}
		if store.readdirCache.misses != initialMisses+1 {
			t.Errorf("Expected cache miss for evicted entry")
		}
	})

	t.Run("CacheDisabled", func(t *testing.T) {
		store := createTestStore(t, BadgerMetadataStoreConfig{
			DBPath: t.TempDir(),
			Capabilities: metadata.FilesystemCapabilities{
				MaxReadSize: 1048576,
			},
			CacheEnabled: false, // Cache disabled
		})
		defer func() { _ = store.Close() }()

		share := createTestShare(t, store, "/test")
		rootHandle := share.RootHandle
		ctx := createAuthContext("127.0.0.1")

		// Multiple reads should all miss cache (cache disabled)
		for i := 0; i < 3; i++ {
			_, err := store.ReadDirectory(ctx, rootHandle, "", 1024*1024)
			if err != nil {
				t.Fatalf("ReadDirectory %d failed: %v", i, err)
			}
		}

		// Should have 0 hits and 0 misses (cache not used)
		if store.readdirCache.hits != 0 {
			t.Errorf("Expected 0 cache hits when disabled, got %d", store.readdirCache.hits)
		}
		if store.readdirCache.misses != 0 {
			t.Errorf("Expected 0 cache misses when disabled, got %d", store.readdirCache.misses)
		}
	})
}

// TestLookupCache tests the Lookup caching functionality
func TestLookupCache(t *testing.T) {
	t.Run("CacheHit", func(t *testing.T) {
		store := createTestStore(t, BadgerMetadataStoreConfig{
			DBPath: t.TempDir(),
			Capabilities: metadata.FilesystemCapabilities{
				MaxReadSize: 1048576,
			},
			CacheEnabled:    true,
			CacheTTL:        5 * time.Second,
			CacheMaxEntries: 100,
		})
		defer func() { _ = store.Close() }()

		share := createTestShare(t, store, "/test")
		rootHandle := share.RootHandle
		ctx := createAuthContext("127.0.0.1")

		// Create a test file
		_, err := store.Create(ctx, rootHandle, "test.txt", &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
		})
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// First lookup - should be a cache miss
		initialMisses := store.lookupCache.misses
		handle1, attr1, err := store.Lookup(ctx, rootHandle, "test.txt")
		if err != nil {
			t.Fatalf("First Lookup failed: %v", err)
		}
		if store.lookupCache.misses != initialMisses+1 {
			t.Errorf("Expected cache miss on first lookup")
		}

		// Second lookup - should be a cache hit
		initialHits := store.lookupCache.hits
		handle2, attr2, err := store.Lookup(ctx, rootHandle, "test.txt")
		if err != nil {
			t.Fatalf("Second Lookup failed: %v", err)
		}
		if store.lookupCache.hits != initialHits+1 {
			t.Errorf("Expected cache hit on second lookup")
		}

		// Verify same results
		if !bytes.Equal(handle1, handle2) {
			t.Errorf("Cache returned different handle")
		}
		if attr1.Size != attr2.Size || attr1.Mode != attr2.Mode {
			t.Errorf("Cache returned different attributes")
		}
	})

	t.Run("CacheInvalidationOnCreate", func(t *testing.T) {
		store := createTestStore(t, BadgerMetadataStoreConfig{
			DBPath: t.TempDir(),
			Capabilities: metadata.FilesystemCapabilities{
				MaxReadSize: 1048576,
			},
			CacheEnabled:           true,
			CacheTTL:               5 * time.Second,
			CacheMaxEntries:        100,
			CacheInvalidateOnWrite: true,
		})
		defer func() { _ = store.Close() }()

		share := createTestShare(t, store, "/test")
		rootHandle := share.RootHandle
		ctx := createAuthContext("127.0.0.1")

		// Create initial file
		_, err := store.Create(ctx, rootHandle, "file1.txt", &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
		})
		if err != nil {
			t.Fatalf("Failed to create file1: %v", err)
		}

		// Lookup to populate cache
		_, _, err = store.Lookup(ctx, rootHandle, "file1.txt")
		if err != nil {
			t.Fatalf("First Lookup failed: %v", err)
		}

		// Create another file in same directory - should invalidate lookup cache for this directory
		_, err = store.Create(ctx, rootHandle, "file2.txt", &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
		})
		if err != nil {
			t.Fatalf("Failed to create file2: %v", err)
		}

		// Next lookup should be a cache miss (invalidated)
		initialMisses := store.lookupCache.misses
		_, _, err = store.Lookup(ctx, rootHandle, "file1.txt")
		if err != nil {
			t.Fatalf("Second Lookup failed: %v", err)
		}
		if store.lookupCache.misses != initialMisses+1 {
			t.Errorf("Expected cache miss after create in same directory")
		}
	})

	t.Run("CacheInvalidationOnMove", func(t *testing.T) {
		store := createTestStore(t, BadgerMetadataStoreConfig{
			DBPath: t.TempDir(),
			Capabilities: metadata.FilesystemCapabilities{
				MaxReadSize: 1048576,
			},
			CacheEnabled:           true,
			CacheTTL:               5 * time.Second,
			CacheMaxEntries:        100,
			CacheInvalidateOnWrite: true,
		})
		defer func() { _ = store.Close() }()

		share := createTestShare(t, store, "/test")
		rootHandle := share.RootHandle
		ctx := createAuthContext("127.0.0.1")

		// Create file and directory
		_, err := store.Create(ctx, rootHandle, "file.txt", &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
		})
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}
		dirHandle, err := store.Create(ctx, rootHandle, "dir", &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0755,
		})
		if err != nil {
			t.Fatalf("Failed to create directory: %v", err)
		}

		// Lookup file to populate cache
		_, _, err = store.Lookup(ctx, rootHandle, "file.txt")
		if err != nil {
			t.Fatalf("Lookup failed: %v", err)
		}

		// Move file to directory - should invalidate both source and dest directory caches
		err = store.Move(ctx, rootHandle, "file.txt", dirHandle, "file.txt")
		if err != nil {
			t.Fatalf("Move failed: %v", err)
		}

		// Lookup in source directory should fail (file was moved)
		_, _, err = store.Lookup(ctx, rootHandle, "file.txt")
		if err == nil {
			t.Error("Expected error looking up moved file")
		}
	})

	t.Run("DotAndDotDotNotCached", func(t *testing.T) {
		store := createTestStore(t, BadgerMetadataStoreConfig{
			DBPath: t.TempDir(),
			Capabilities: metadata.FilesystemCapabilities{
				MaxReadSize: 1048576,
			},
			CacheEnabled:    true,
			CacheTTL:        5 * time.Second,
			CacheMaxEntries: 100,
		})
		defer func() { _ = store.Close() }()

		share := createTestShare(t, store, "/test")
		rootHandle := share.RootHandle
		ctx := createAuthContext("127.0.0.1")

		// Lookup "." multiple times
		initialHits := store.lookupCache.hits
		initialMisses := store.lookupCache.misses

		for i := 0; i < 3; i++ {
			_, _, err := store.Lookup(ctx, rootHandle, ".")
			if err != nil {
				t.Fatalf("Lookup '.' failed: %v", err)
			}
		}

		// Should not hit cache (special entries not cached)
		if store.lookupCache.hits != initialHits {
			t.Errorf("Expected no cache hits for '.', got %d new hits",
				store.lookupCache.hits-initialHits)
		}
		if store.lookupCache.misses != initialMisses {
			t.Errorf("Expected no cache misses for '.', got %d new misses",
				store.lookupCache.misses-initialMisses)
		}
	})

	t.Run("CacheTTLExpiration", func(t *testing.T) {
		store := createTestStore(t, BadgerMetadataStoreConfig{
			DBPath: t.TempDir(),
			Capabilities: metadata.FilesystemCapabilities{
				MaxReadSize: 1048576,
			},
			CacheEnabled:    true,
			CacheTTL:        100 * time.Millisecond, // Very short TTL
			CacheMaxEntries: 100,
		})
		defer func() { _ = store.Close() }()

		share := createTestShare(t, store, "/test")
		rootHandle := share.RootHandle
		ctx := createAuthContext("127.0.0.1")

		// Create a test file
		_, err := store.Create(ctx, rootHandle, "test.txt", &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
		})
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// First lookup - populate cache
		_, _, err = store.Lookup(ctx, rootHandle, "test.txt")
		if err != nil {
			t.Fatalf("First Lookup failed: %v", err)
		}

		// Immediate second lookup - should hit cache
		initialHits := store.lookupCache.hits
		_, _, err = store.Lookup(ctx, rootHandle, "test.txt")
		if err != nil {
			t.Fatalf("Second Lookup failed: %v", err)
		}
		if store.lookupCache.hits != initialHits+1 {
			t.Errorf("Expected cache hit before TTL expiration")
		}

		// Wait for TTL to expire
		time.Sleep(150 * time.Millisecond)

		// Third lookup - should miss cache (expired)
		initialMisses := store.lookupCache.misses
		_, _, err = store.Lookup(ctx, rootHandle, "test.txt")
		if err != nil {
			t.Fatalf("Third Lookup failed: %v", err)
		}
		if store.lookupCache.misses != initialMisses+1 {
			t.Errorf("Expected cache miss after TTL expiration")
		}
	})
}

// TestGetFileCache tests the GetFile caching functionality
func TestGetFileCache(t *testing.T) {
	t.Run("CacheHit", func(t *testing.T) {
		store := createTestStore(t, BadgerMetadataStoreConfig{
			DBPath: t.TempDir(),
			Capabilities: metadata.FilesystemCapabilities{
				MaxReadSize: 1048576,
			},
			CacheEnabled:    true,
			CacheTTL:        5 * time.Second,
			CacheMaxEntries: 100,
		})
		defer func() { _ = store.Close() }()

		share := createTestShare(t, store, "/test")
		rootHandle := share.RootHandle
		ctx := createAuthContext("127.0.0.1")

		// Create a test file
		fileHandle, err := store.Create(ctx, rootHandle, "test.txt", &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
		})
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// First GetFile - should be a cache miss
		initialMisses := store.getfileCache.misses
		attr1, err := store.GetFile(context.Background(), fileHandle)
		if err != nil {
			t.Fatalf("First GetFile failed: %v", err)
		}
		if store.getfileCache.misses != initialMisses+1 {
			t.Errorf("Expected cache miss on first GetFile")
		}

		// Second GetFile - should be a cache hit
		initialHits := store.getfileCache.hits
		attr2, err := store.GetFile(context.Background(), fileHandle)
		if err != nil {
			t.Fatalf("Second GetFile failed: %v", err)
		}
		if store.getfileCache.hits != initialHits+1 {
			t.Errorf("Expected cache hit on second GetFile")
		}

		// Verify same results
		if attr1.Size != attr2.Size || attr1.Mode != attr2.Mode {
			t.Errorf("Cache returned different attributes")
		}
	})

	t.Run("CacheInvalidationOnSetAttr", func(t *testing.T) {
		store := createTestStore(t, BadgerMetadataStoreConfig{
			DBPath: t.TempDir(),
			Capabilities: metadata.FilesystemCapabilities{
				MaxReadSize: 1048576,
			},
			CacheEnabled:           true,
			CacheTTL:               5 * time.Second,
			CacheMaxEntries:        100,
			CacheInvalidateOnWrite: true,
		})
		defer func() { _ = store.Close() }()

		share := createTestShare(t, store, "/test")
		rootHandle := share.RootHandle
		ctx := createAuthContext("127.0.0.1")

		// Create a test file
		fileHandle, err := store.Create(ctx, rootHandle, "test.txt", &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
		})
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// GetFile to populate cache
		_, err = store.GetFile(context.Background(), fileHandle)
		if err != nil {
			t.Fatalf("GetFile failed: %v", err)
		}

		// Modify attributes - should invalidate cache
		newMode := uint32(0755)
		err = store.SetFileAttributes(ctx, fileHandle, &metadata.SetAttrs{
			Mode: &newMode,
		})
		if err != nil {
			t.Fatalf("SetFileAttributes failed: %v", err)
		}

		// Next GetFile should be a cache miss
		initialMisses := store.getfileCache.misses
		attr, err := store.GetFile(context.Background(), fileHandle)
		if err != nil {
			t.Fatalf("GetFile after SetFileAttributes failed: %v", err)
		}
		if store.getfileCache.misses != initialMisses+1 {
			t.Errorf("Expected cache miss after SetFileAttributes")
		}

		// Verify new mode
		if attr.Mode != 0755 {
			t.Errorf("Expected mode 0755, got %04o", attr.Mode)
		}
	})

	t.Run("CacheInvalidationOnWrite", func(t *testing.T) {
		store := createTestStore(t, BadgerMetadataStoreConfig{
			DBPath: t.TempDir(),
			Capabilities: metadata.FilesystemCapabilities{
				MaxReadSize: 1048576,
			},
			CacheEnabled:           true,
			CacheTTL:               5 * time.Second,
			CacheMaxEntries:        100,
			CacheInvalidateOnWrite: true,
		})
		defer func() { _ = store.Close() }()

		share := createTestShare(t, store, "/test")
		rootHandle := share.RootHandle
		ctx := createAuthContext("127.0.0.1")

		// Create a test file
		fileHandle, err := store.Create(ctx, rootHandle, "test.txt", &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
		})
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// GetFile to populate cache
		attr1, err := store.GetFile(context.Background(), fileHandle)
		if err != nil {
			t.Fatalf("GetFile failed: %v", err)
		}
		initialSize := attr1.Size

		// Write to file - should invalidate cache
		intent, err := store.PrepareWrite(ctx, fileHandle, 100)
		if err != nil {
			t.Fatalf("PrepareWrite failed: %v", err)
		}
		_, err = store.CommitWrite(ctx, intent)
		if err != nil {
			t.Fatalf("CommitWrite failed: %v", err)
		}

		// Next GetFile should be a cache miss
		initialMisses := store.getfileCache.misses
		attr2, err := store.GetFile(context.Background(), fileHandle)
		if err != nil {
			t.Fatalf("GetFile after write failed: %v", err)
		}
		if store.getfileCache.misses != initialMisses+1 {
			t.Errorf("Expected cache miss after write")
		}

		// Verify size changed
		if attr2.Size == initialSize {
			t.Errorf("Expected size to change after write")
		}
	})

	t.Run("CacheInvalidationOnRemove", func(t *testing.T) {
		store := createTestStore(t, BadgerMetadataStoreConfig{
			DBPath: t.TempDir(),
			Capabilities: metadata.FilesystemCapabilities{
				MaxReadSize: 1048576,
			},
			CacheEnabled:           true,
			CacheTTL:               5 * time.Second,
			CacheMaxEntries:        100,
			CacheInvalidateOnWrite: true,
		})
		defer func() { _ = store.Close() }()

		share := createTestShare(t, store, "/test")
		rootHandle := share.RootHandle
		ctx := createAuthContext("127.0.0.1")

		// Create a test file
		fileHandle, err := store.Create(ctx, rootHandle, "test.txt", &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
		})
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// GetFile to populate cache
		_, err = store.GetFile(context.Background(), fileHandle)
		if err != nil {
			t.Fatalf("GetFile failed: %v", err)
		}

		// Remove file
		_, err = store.RemoveFile(ctx, rootHandle, "test.txt")
		if err != nil {
			t.Fatalf("RemoveFile failed: %v", err)
		}

		// GetFile should now fail (file doesn't exist)
		_, err = store.GetFile(context.Background(), fileHandle)
		if err == nil {
			t.Error("Expected error getting removed file")
		}
	})

	t.Run("CacheTTLExpiration", func(t *testing.T) {
		store := createTestStore(t, BadgerMetadataStoreConfig{
			DBPath: t.TempDir(),
			Capabilities: metadata.FilesystemCapabilities{
				MaxReadSize: 1048576,
			},
			CacheEnabled:    true,
			CacheTTL:        100 * time.Millisecond, // Very short TTL
			CacheMaxEntries: 100,
		})
		defer func() { _ = store.Close() }()

		share := createTestShare(t, store, "/test")
		rootHandle := share.RootHandle
		ctx := createAuthContext("127.0.0.1")

		// Create a test file
		fileHandle, err := store.Create(ctx, rootHandle, "test.txt", &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
		})
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// First GetFile - populate cache
		_, err = store.GetFile(context.Background(), fileHandle)
		if err != nil {
			t.Fatalf("First GetFile failed: %v", err)
		}

		// Immediate second GetFile - should hit cache
		initialHits := store.getfileCache.hits
		_, err = store.GetFile(context.Background(), fileHandle)
		if err != nil {
			t.Fatalf("Second GetFile failed: %v", err)
		}
		if store.getfileCache.hits != initialHits+1 {
			t.Errorf("Expected cache hit before TTL expiration")
		}

		// Wait for TTL to expire
		time.Sleep(150 * time.Millisecond)

		// Third GetFile - should miss cache (expired)
		initialMisses := store.getfileCache.misses
		_, err = store.GetFile(context.Background(), fileHandle)
		if err != nil {
			t.Fatalf("Third GetFile failed: %v", err)
		}
		if store.getfileCache.misses != initialMisses+1 {
			t.Errorf("Expected cache miss after TTL expiration")
		}
	})

	t.Run("CacheDisabled", func(t *testing.T) {
		store := createTestStore(t, BadgerMetadataStoreConfig{
			DBPath: t.TempDir(),
			Capabilities: metadata.FilesystemCapabilities{
				MaxReadSize: 1048576,
			},
			CacheEnabled: false, // Cache disabled
		})
		defer func() { _ = store.Close() }()

		share := createTestShare(t, store, "/test")
		rootHandle := share.RootHandle
		ctx := createAuthContext("127.0.0.1")

		// Create a test file
		fileHandle, err := store.Create(ctx, rootHandle, "test.txt", &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
		})
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// Multiple GetFile calls should all bypass cache
		for i := 0; i < 3; i++ {
			_, err := store.GetFile(context.Background(), fileHandle)
			if err != nil {
				t.Fatalf("GetFile %d failed: %v", i, err)
			}
		}

		// Should have 0 hits and 0 misses (cache not used)
		if store.getfileCache.hits != 0 {
			t.Errorf("Expected 0 cache hits when disabled, got %d", store.getfileCache.hits)
		}
		if store.getfileCache.misses != 0 {
			t.Errorf("Expected 0 cache misses when disabled, got %d", store.getfileCache.misses)
		}
	})
}

// TestCacheStatistics tests that cache statistics are properly tracked
func TestCacheStatistics(t *testing.T) {
	store := createTestStore(t, BadgerMetadataStoreConfig{
		DBPath: t.TempDir(),
		Capabilities: metadata.FilesystemCapabilities{
			MaxReadSize: 1048576,
		},
		CacheEnabled:    true,
		CacheTTL:        5 * time.Second,
		CacheMaxEntries: 100,
	})
	defer func() { _ = store.Close() }()

	share := createTestShare(t, store, "/test")
	rootHandle := share.RootHandle
	ctx := createAuthContext("127.0.0.1")

	// Create test files
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("file%d.txt", i)
		_, err := store.Create(ctx, rootHandle, name, &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
		})
		if err != nil {
			t.Fatalf("Failed to create file %s: %v", name, err)
		}
	}

	// Track initial statistics
	initialReaddirHits := store.readdirCache.hits
	initialReaddirMisses := store.readdirCache.misses
	initialLookupHits := store.lookupCache.hits
	initialLookupMisses := store.lookupCache.misses

	// Perform operations
	// ReadDirectory twice (1 miss + 1 hit)
	_, err := store.ReadDirectory(ctx, rootHandle, "", 1024*1024)
	if err != nil {
		t.Fatalf("ReadDirectory failed: %v", err)
	}
	_, err = store.ReadDirectory(ctx, rootHandle, "", 1024*1024)
	if err != nil {
		t.Fatalf("ReadDirectory failed: %v", err)
	}

	// Lookup same file twice (1 miss + 1 hit)
	_, _, err = store.Lookup(ctx, rootHandle, "file0.txt")
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}
	_, _, err = store.Lookup(ctx, rootHandle, "file0.txt")
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}

	// Verify statistics
	if store.readdirCache.hits != initialReaddirHits+1 {
		t.Errorf("Expected 1 ReadDir cache hit, got %d",
			store.readdirCache.hits-initialReaddirHits)
	}
	if store.readdirCache.misses != initialReaddirMisses+1 {
		t.Errorf("Expected 1 ReadDir cache miss, got %d",
			store.readdirCache.misses-initialReaddirMisses)
	}
	if store.lookupCache.hits != initialLookupHits+1 {
		t.Errorf("Expected 1 Lookup cache hit, got %d",
			store.lookupCache.hits-initialLookupHits)
	}
	if store.lookupCache.misses != initialLookupMisses+1 {
		t.Errorf("Expected 1 Lookup cache miss, got %d",
			store.lookupCache.misses-initialLookupMisses)
	}
}
