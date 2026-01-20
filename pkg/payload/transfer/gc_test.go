package transfer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/payload/store/memory"
)

// ============================================================================
// parsePayloadIDFromBlockKey Tests
// ============================================================================

func TestParsePayloadIDFromBlockKey(t *testing.T) {
	tests := []struct {
		name     string
		blockKey string
		expected string
	}{
		{
			name:     "standard block key",
			blockKey: "export/file.txt/chunk-0/block-0",
			expected: "export/file.txt",
		},
		{
			name:     "nested path",
			blockKey: "export/deep/nested/path/document.pdf/chunk-2/block-5",
			expected: "export/deep/nested/path/document.pdf",
		},
		{
			name:     "file at root of share",
			blockKey: "myshare/readme.txt/chunk-0/block-0",
			expected: "myshare/readme.txt",
		},
		{
			name:     "multiple chunks",
			blockKey: "export/large-file.bin/chunk-100/block-3",
			expected: "export/large-file.bin",
		},
		{
			name:     "empty string",
			blockKey: "",
			expected: "",
		},
		{
			name:     "no chunk marker",
			blockKey: "export/file.txt",
			expected: "",
		},
		{
			name:     "chunk at start",
			blockKey: "/chunk-0/block-0",
			expected: "",
		},
		{
			name:     "only chunk marker",
			blockKey: "chunk-0/block-0",
			expected: "",
		},
		{
			name:     "path with hyphen",
			blockKey: "export/my-file/chunk-0/block-0",
			expected: "export/my-file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parsePayloadIDFromBlockKey(tt.blockKey)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// ============================================================================
// Mock Reconciler for GC Testing using real memory store
// ============================================================================

// gcTestReconciler implements MetadataReconciler using real memory stores.
type gcTestReconciler struct {
	stores map[string]metadata.MetadataStore
}

func newGCTestReconciler() *gcTestReconciler {
	return &gcTestReconciler{
		stores: make(map[string]metadata.MetadataStore),
	}
}

// addShare creates a new memory store for the given share.
// Returns the store so test can add files to it.
func (r *gcTestReconciler) addShare(shareName string) metadata.MetadataStore {
	store := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	r.stores[shareName] = store
	return store
}

func (r *gcTestReconciler) GetMetadataStoreForShare(shareName string) (metadata.MetadataStore, error) {
	store, exists := r.stores[shareName]
	if !exists {
		return nil, fmt.Errorf("share %q not found", shareName)
	}
	return store, nil
}

// ============================================================================
// Helper to create a file with a specific PayloadID in the store
// ============================================================================

func createFileWithPayloadID(ctx context.Context, t testing.TB, store metadata.MetadataStore, shareName, payloadID string) {
	t.Helper()

	// Register the share first (required for GetRootHandle to work)
	share := &metadata.Share{
		Name: shareName,
	}
	err := store.CreateShare(ctx, share)
	if err != nil {
		// Ignore "already exists" errors
		var storeErr *metadata.StoreError
		if !errors.As(err, &storeErr) || storeErr.Code != metadata.ErrAlreadyExists {
			require.NoError(t, err)
		}
	}

	// Create root directory for the share if needed
	rootAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0755,
	}
	_, err = store.CreateRootDirectory(ctx, shareName, rootAttr)
	if err != nil {
		// Ignore errors - CreateRootDirectory returns success for existing roots
		var storeErr *metadata.StoreError
		if !errors.As(err, &storeErr) || storeErr.Code != metadata.ErrAlreadyExists {
			require.NoError(t, err)
		}
	}

	// Get root handle
	rootHandle, err := store.GetRootHandle(ctx, shareName)
	require.NoError(t, err)

	// Generate unique filename from payloadID (extract file part after share name)
	filename := "file-" + payloadID
	if len(filename) > 50 {
		filename = filename[:50]
	}

	// Generate handle for the file
	handle, err := store.GenerateHandle(ctx, shareName, "/"+filename)
	require.NoError(t, err)

	// Decode handle to get UUID
	_, fileID, err := metadata.DecodeFileHandle(handle)
	require.NoError(t, err)

	// Create a file with the given PayloadID
	fileAttr := &metadata.FileAttr{
		Type:      metadata.FileTypeRegular,
		Mode:      0644,
		Size:      1024, // Arbitrary size
		PayloadID: metadata.PayloadID(payloadID),
	}

	// Create the file
	file := &metadata.File{
		ShareName: shareName,
		Path:      "/" + filename,
		FileAttr:  *fileAttr,
		ID:        fileID,
	}

	// Store the file
	err = store.PutFile(ctx, file)
	require.NoError(t, err)

	// Link to parent
	err = store.SetChild(ctx, rootHandle, filename, handle)
	require.NoError(t, err)
}

// ============================================================================
// CollectGarbage Tests
// ============================================================================

func TestCollectGarbage_Empty(t *testing.T) {
	ctx := context.Background()
	blockStore := memory.New()
	defer func() { _ = blockStore.Close() }()

	reconciler := newGCTestReconciler()

	stats := CollectGarbage(ctx, blockStore, reconciler, nil)

	assert.NotNil(t, stats)
	assert.Equal(t, 0, stats.BlocksScanned)
	assert.Equal(t, 0, stats.OrphanBlocks)
	assert.Equal(t, int64(0), stats.BytesReclaimed)
}

func TestCollectGarbage_NoOrphans(t *testing.T) {
	ctx := context.Background()
	blockStore := memory.New()
	defer func() { _ = blockStore.Close() }()

	// Add a block to the store
	blockKey := "export/test-file.txt/chunk-0/block-0"
	require.NoError(t, blockStore.WriteBlock(ctx, blockKey, []byte("test data")))

	// Create reconciler with a share and file that owns this block
	reconciler := newGCTestReconciler()
	store := reconciler.addShare("/export")
	createFileWithPayloadID(ctx, t, store, "/export", "export/test-file.txt")

	stats := CollectGarbage(ctx, blockStore, reconciler, nil)

	assert.Equal(t, 1, stats.BlocksScanned)
	assert.Equal(t, 0, stats.OrphanBlocks)
	assert.Equal(t, int64(0), stats.BytesReclaimed)

	// Verify block still exists
	data, err := blockStore.ReadBlock(ctx, blockKey)
	assert.NoError(t, err)
	assert.Equal(t, []byte("test data"), data)
}

func TestCollectGarbage_DeletesOrphans(t *testing.T) {
	ctx := context.Background()
	blockStore := memory.New()
	defer func() { _ = blockStore.Close() }()

	// Add an orphan block (no file references it)
	blockKey := "export/deleted-file.txt/chunk-0/block-0"
	require.NoError(t, blockStore.WriteBlock(ctx, blockKey, []byte("orphan data")))

	// Create reconciler with share but no file for this payload
	reconciler := newGCTestReconciler()
	reconciler.addShare("/export")

	stats := CollectGarbage(ctx, blockStore, reconciler, nil)

	assert.Equal(t, 1, stats.BlocksScanned)
	assert.Equal(t, 1, stats.OrphanBlocks)
	assert.Equal(t, int64(BlockSize), stats.BytesReclaimed) // GC estimates by block size

	// Verify block was deleted
	_, err := blockStore.ReadBlock(ctx, blockKey)
	assert.Error(t, err)
}

func TestCollectGarbage_MixedOrphansAndValid(t *testing.T) {
	ctx := context.Background()
	blockStore := memory.New()
	defer func() { _ = blockStore.Close() }()

	// Add a valid block
	validKey := "export/valid-file.txt/chunk-0/block-0"
	require.NoError(t, blockStore.WriteBlock(ctx, validKey, []byte("valid")))

	// Add an orphan block
	orphanKey := "export/orphan-file.txt/chunk-0/block-0"
	require.NoError(t, blockStore.WriteBlock(ctx, orphanKey, []byte("orphan")))

	// Create reconciler with share and only the valid file
	reconciler := newGCTestReconciler()
	store := reconciler.addShare("/export")
	createFileWithPayloadID(ctx, t, store, "/export", "export/valid-file.txt")

	stats := CollectGarbage(ctx, blockStore, reconciler, nil)

	assert.Equal(t, 2, stats.BlocksScanned)
	assert.Equal(t, 1, stats.OrphanBlocks)
	assert.Equal(t, int64(BlockSize), stats.BytesReclaimed)

	// Valid block should still exist
	data, err := blockStore.ReadBlock(ctx, validKey)
	assert.NoError(t, err)
	assert.Equal(t, []byte("valid"), data)

	// Orphan should be deleted
	_, err = blockStore.ReadBlock(ctx, orphanKey)
	assert.Error(t, err)
}

func TestCollectGarbage_UnknownShare(t *testing.T) {
	ctx := context.Background()
	blockStore := memory.New()
	defer func() { _ = blockStore.Close() }()

	// Add a block for an unknown share
	blockKey := "unknown-share/file.txt/chunk-0/block-0"
	require.NoError(t, blockStore.WriteBlock(ctx, blockKey, []byte("data")))

	// Create reconciler with a different share
	reconciler := newGCTestReconciler()
	reconciler.addShare("/export")

	stats := CollectGarbage(ctx, blockStore, reconciler, nil)

	// Block should be deleted (unknown share = orphan)
	assert.Equal(t, 1, stats.BlocksScanned)
	assert.Equal(t, 1, stats.OrphanBlocks)
}

func TestCollectGarbage_ProgressCallback(t *testing.T) {
	ctx := context.Background()
	blockStore := memory.New()
	defer func() { _ = blockStore.Close() }()

	// Add some orphan blocks (one per "file" to trigger callback per file)
	for i := 0; i < 5; i++ {
		blockKey := fmt.Sprintf("export/file%d.txt/chunk-0/block-0", i)
		require.NoError(t, blockStore.WriteBlock(ctx, blockKey, []byte("data")))
	}

	reconciler := newGCTestReconciler()
	reconciler.addShare("/export")

	var progressCalls int
	progress := func(stats GCStats) {
		progressCalls++
		assert.GreaterOrEqual(t, stats.OrphanFiles, 0)
		assert.LessOrEqual(t, stats.OrphanBlocks, stats.BlocksScanned)
	}

	CollectGarbage(ctx, blockStore, reconciler, &GCOptions{ProgressCallback: progress})

	// Progress should have been called once per orphan file
	assert.Equal(t, 5, progressCalls)
}

func TestCollectGarbage_DryRun(t *testing.T) {
	ctx := context.Background()
	blockStore := memory.New()
	defer func() { _ = blockStore.Close() }()

	// Add an orphan block
	blockKey := "export/orphan.txt/chunk-0/block-0"
	require.NoError(t, blockStore.WriteBlock(ctx, blockKey, []byte("orphan data")))

	reconciler := newGCTestReconciler()
	reconciler.addShare("/export")

	// Run with dry run - should NOT delete
	stats := CollectGarbage(ctx, blockStore, reconciler, &GCOptions{DryRun: true})

	assert.Equal(t, 1, stats.OrphanBlocks)

	// Block should still exist
	data, err := blockStore.ReadBlock(ctx, blockKey)
	assert.NoError(t, err)
	assert.Equal(t, []byte("orphan data"), data)
}
