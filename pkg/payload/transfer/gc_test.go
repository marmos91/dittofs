package transfer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
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
// Mock Reconciler for GC Testing
// ============================================================================

// gcTestReconciler implements MetadataReconciler with minimal functionality for GC testing.
// It wraps simple file existence tracking without implementing the full MetadataStore interface.
type gcTestReconciler struct {
	shares map[string]*gcTestStore
}

func newGCTestReconciler() *gcTestReconciler {
	return &gcTestReconciler{
		shares: make(map[string]*gcTestStore),
	}
}

func (r *gcTestReconciler) addShare(shareName string) *gcTestStore {
	store := &gcTestStore{files: make(map[string]bool)}
	r.shares[shareName] = store
	return store
}

func (r *gcTestReconciler) GetMetadataStoreForShare(shareName string) (metadata.MetadataStore, error) {
	store, exists := r.shares[shareName]
	if !exists {
		return nil, errors.New("share not found")
	}
	return store, nil
}

// gcTestStore implements metadata.MetadataStore with minimal functionality.
// Only GetFileByPayloadID is actually implemented; other methods panic.
type gcTestStore struct {
	files map[string]bool
}

func (s *gcTestStore) addFile(payloadID string) {
	s.files[payloadID] = true
}

func (s *gcTestStore) GetFileByPayloadID(_ context.Context, payloadID metadata.PayloadID) (*metadata.File, error) {
	if s.files[string(payloadID)] {
		return &metadata.File{}, nil
	}
	return nil, errors.New("file not found")
}

// Required interface methods - minimal stubs for MetadataStore
// All methods except GetFileByPayloadID will panic if called.
func (s *gcTestStore) GetFile(context.Context, metadata.FileHandle) (*metadata.File, error) {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) PutFile(context.Context, *metadata.File) error {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) DeleteFile(context.Context, metadata.FileHandle) error {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) GetChild(context.Context, metadata.FileHandle, string) (metadata.FileHandle, error) {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) SetChild(context.Context, metadata.FileHandle, string, metadata.FileHandle) error {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) DeleteChild(context.Context, metadata.FileHandle, string) error {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) ListChildren(context.Context, metadata.FileHandle, string, int) ([]metadata.DirEntry, string, error) {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) GetParent(context.Context, metadata.FileHandle) (metadata.FileHandle, error) {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) SetParent(context.Context, metadata.FileHandle, metadata.FileHandle) error {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) GetLinkCount(context.Context, metadata.FileHandle) (uint32, error) {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) SetLinkCount(context.Context, metadata.FileHandle, uint32) error {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) WithTransaction(context.Context, func(metadata.Transaction) error) error {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) GenerateHandle(context.Context, string, string) (metadata.FileHandle, error) {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) GetFilesystemMeta(context.Context, string) (*metadata.FilesystemMeta, error) {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) PutFilesystemMeta(context.Context, string, *metadata.FilesystemMeta) error {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) GetRootHandle(context.Context, string) (metadata.FileHandle, error) {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) GetShareOptions(context.Context, string) (*metadata.ShareOptions, error) {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) CreateRootDirectory(context.Context, string, *metadata.FileAttr) (*metadata.File, error) {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) CreateShare(context.Context, *metadata.Share) error {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) UpdateShareOptions(context.Context, string, *metadata.ShareOptions) error {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) DeleteShare(context.Context, string) error {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) ListShares(context.Context) ([]string, error) {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) SetServerConfig(context.Context, metadata.MetadataServerConfig) error {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) GetServerConfig(context.Context) (metadata.MetadataServerConfig, error) {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) GetFilesystemCapabilities(context.Context, metadata.FileHandle) (*metadata.FilesystemCapabilities, error) {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) SetFilesystemCapabilities(metadata.FilesystemCapabilities) {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) GetFilesystemStatistics(context.Context, metadata.FileHandle) (*metadata.FilesystemStatistics, error) {
	panic("not implemented for GC tests")
}
func (s *gcTestStore) Healthcheck(context.Context) error { return nil }
func (s *gcTestStore) Close() error                      { return nil }

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
	assert.Equal(t, 0, stats.OrphanFiles)
	assert.Equal(t, 0, stats.OrphanBlocks)
	assert.Equal(t, 0, stats.Errors)
}

func TestCollectGarbage_NoOrphans(t *testing.T) {
	ctx := context.Background()
	blockStore := memory.New()
	defer func() { _ = blockStore.Close() }()

	// Create blocks for a file
	payloadID := "export/test-file.txt"
	require.NoError(t, blockStore.WriteBlock(ctx, payloadID+"/chunk-0/block-0", make([]byte, 1024)))
	require.NoError(t, blockStore.WriteBlock(ctx, payloadID+"/chunk-0/block-1", make([]byte, 1024)))

	// Set up reconciler with the file in metadata
	reconciler := newGCTestReconciler()
	store := reconciler.addShare("/export")
	store.addFile(payloadID)

	stats := CollectGarbage(ctx, blockStore, reconciler, nil)

	assert.Equal(t, 2, stats.BlocksScanned)
	assert.Equal(t, 0, stats.OrphanFiles)
	assert.Equal(t, 0, stats.OrphanBlocks)
	assert.Equal(t, 0, stats.Errors)
}

func TestCollectGarbage_WithOrphans(t *testing.T) {
	ctx := context.Background()
	blockStore := memory.New()
	defer func() { _ = blockStore.Close() }()

	// Create blocks for two files
	validPayloadID := "export/valid-file.txt"
	orphanPayloadID := "export/orphan-file.txt"

	require.NoError(t, blockStore.WriteBlock(ctx, validPayloadID+"/chunk-0/block-0", make([]byte, 1024)))
	require.NoError(t, blockStore.WriteBlock(ctx, orphanPayloadID+"/chunk-0/block-0", make([]byte, 1024)))
	require.NoError(t, blockStore.WriteBlock(ctx, orphanPayloadID+"/chunk-0/block-1", make([]byte, 1024)))

	// Set up reconciler with only the valid file in metadata
	reconciler := newGCTestReconciler()
	store := reconciler.addShare("/export")
	store.addFile(validPayloadID) // orphanPayloadID is NOT in metadata

	stats := CollectGarbage(ctx, blockStore, reconciler, nil)

	assert.Equal(t, 3, stats.BlocksScanned)
	assert.Equal(t, 1, stats.OrphanFiles)
	assert.Equal(t, 2, stats.OrphanBlocks)
	assert.Equal(t, 0, stats.Errors)

	// Verify orphan blocks were deleted
	keys, _ := blockStore.ListByPrefix(ctx, orphanPayloadID)
	assert.Empty(t, keys, "orphan blocks should be deleted")

	// Verify valid blocks still exist
	keys, _ = blockStore.ListByPrefix(ctx, validPayloadID)
	assert.Len(t, keys, 1, "valid blocks should remain")
}

func TestCollectGarbage_DryRun(t *testing.T) {
	ctx := context.Background()
	blockStore := memory.New()
	defer func() { _ = blockStore.Close() }()

	// Create orphan blocks
	orphanPayloadID := "export/orphan-file.txt"
	require.NoError(t, blockStore.WriteBlock(ctx, orphanPayloadID+"/chunk-0/block-0", make([]byte, 1024)))
	require.NoError(t, blockStore.WriteBlock(ctx, orphanPayloadID+"/chunk-0/block-1", make([]byte, 1024)))

	// Set up reconciler with NO files in metadata
	reconciler := newGCTestReconciler()
	reconciler.addShare("/export")

	stats := CollectGarbage(ctx, blockStore, reconciler, &GCOptions{DryRun: true})

	assert.Equal(t, 2, stats.BlocksScanned)
	assert.Equal(t, 1, stats.OrphanFiles)
	assert.Equal(t, 2, stats.OrphanBlocks)

	// Verify blocks were NOT deleted (dry run)
	keys, _ := blockStore.ListByPrefix(ctx, orphanPayloadID)
	assert.Len(t, keys, 2, "blocks should NOT be deleted in dry run")
}

func TestCollectGarbage_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	blockStore := memory.New()
	defer func() { _ = blockStore.Close() }()

	// Create many orphan files
	reconciler := newGCTestReconciler()
	reconciler.addShare("/export")

	for i := 0; i < 100; i++ {
		payloadID := fmt.Sprintf("export/orphan-%d.txt", i)
		require.NoError(t, blockStore.WriteBlock(ctx, payloadID+"/chunk-0/block-0", make([]byte, 1024)))
	}

	// Cancel immediately
	cancel()

	stats := CollectGarbage(ctx, blockStore, reconciler, nil)

	// Should return early due to cancellation
	// Exact counts depend on timing, but should be less than 100
	assert.True(t, stats.OrphanFiles < 100 || stats.OrphanFiles == 0,
		"should stop early on cancellation")
}

func TestCollectGarbage_SharePrefix(t *testing.T) {
	ctx := context.Background()
	blockStore := memory.New()
	defer func() { _ = blockStore.Close() }()

	// Create blocks in two shares
	require.NoError(t, blockStore.WriteBlock(ctx, "share1/file.txt/chunk-0/block-0", make([]byte, 1024)))
	require.NoError(t, blockStore.WriteBlock(ctx, "share2/file.txt/chunk-0/block-0", make([]byte, 1024)))

	// Set up reconciler with NO files (all orphans)
	reconciler := newGCTestReconciler()
	reconciler.addShare("/share1")
	reconciler.addShare("/share2")

	// Scan only share1
	stats := CollectGarbage(ctx, blockStore, reconciler, &GCOptions{SharePrefix: "share1/"})

	assert.Equal(t, 1, stats.BlocksScanned, "should only scan share1")
	assert.Equal(t, 1, stats.OrphanFiles)
}

func TestCollectGarbage_MaxOrphansLimit(t *testing.T) {
	ctx := context.Background()
	blockStore := memory.New()
	defer func() { _ = blockStore.Close() }()

	// Create 10 orphan files
	reconciler := newGCTestReconciler()
	reconciler.addShare("/export")

	for i := 0; i < 10; i++ {
		payloadID := fmt.Sprintf("export/orphan-%d.txt", i)
		require.NoError(t, blockStore.WriteBlock(ctx, payloadID+"/chunk-0/block-0", make([]byte, 1024)))
	}

	// Limit to 3 orphans
	stats := CollectGarbage(ctx, blockStore, reconciler, &GCOptions{MaxOrphansPerShare: 3})

	assert.Equal(t, 3, stats.OrphanFiles, "should stop after max orphans")
}

func TestCollectGarbage_ProgressCallback(t *testing.T) {
	ctx := context.Background()
	blockStore := memory.New()
	defer func() { _ = blockStore.Close() }()

	// Create orphan files
	reconciler := newGCTestReconciler()
	reconciler.addShare("/export")

	for i := 0; i < 5; i++ {
		payloadID := fmt.Sprintf("export/orphan-%d.txt", i)
		require.NoError(t, blockStore.WriteBlock(ctx, payloadID+"/chunk-0/block-0", make([]byte, 1024)))
	}

	// Track progress calls
	var progressCalls []GCStats
	callback := func(stats GCStats) {
		progressCalls = append(progressCalls, stats)
	}

	stats := CollectGarbage(ctx, blockStore, reconciler, &GCOptions{ProgressCallback: callback})

	assert.Equal(t, 5, stats.OrphanFiles)
	assert.Len(t, progressCalls, 5, "should call progress for each orphan")

	// Progress should be incremental
	for i, progress := range progressCalls {
		assert.Equal(t, i+1, progress.OrphanFiles, "progress should be incremental")
	}
}

func TestCollectGarbage_ShareNotFound(t *testing.T) {
	ctx := context.Background()
	blockStore := memory.New()
	defer func() { _ = blockStore.Close() }()

	// Create blocks for a share that doesn't exist in reconciler
	payloadID := "unknownshare/file.txt"
	require.NoError(t, blockStore.WriteBlock(ctx, payloadID+"/chunk-0/block-0", make([]byte, 1024)))

	// Set up reconciler with NO shares
	reconciler := newGCTestReconciler()

	stats := CollectGarbage(ctx, blockStore, reconciler, nil)

	// Should treat as orphan (share not found)
	assert.Equal(t, 1, stats.OrphanFiles)
	assert.Equal(t, 1, stats.OrphanBlocks)
}

func TestCollectGarbage_InvalidBlockKey(t *testing.T) {
	ctx := context.Background()
	blockStore := memory.New()
	defer func() { _ = blockStore.Close() }()

	// Create a block with invalid key format (no chunk marker)
	require.NoError(t, blockStore.WriteBlock(ctx, "invalid-key-no-chunk", make([]byte, 1024)))

	reconciler := newGCTestReconciler()

	stats := CollectGarbage(ctx, blockStore, reconciler, nil)

	assert.Equal(t, 1, stats.Errors, "should count invalid key as error")
	assert.Equal(t, 0, stats.OrphanFiles)
}

// ============================================================================
// Benchmarks
// ============================================================================

func BenchmarkCollectGarbage_Memory(b *testing.B) {
	sizes := []struct {
		name      string
		fileCount int
	}{
		{"10files", 10},
		{"100files", 100},
		{"1000files", 1000},
	}

	for _, size := range sizes {
		b.Run(size.name, func(b *testing.B) {
			ctx := context.Background()
			blockStore := memory.New()
			defer func() { _ = blockStore.Close() }()

			// Set up reconciler with half the files (50% orphans)
			reconciler := newGCTestReconciler()
			store := reconciler.addShare("/export")

			// Create blocks for all files, but only add half to metadata
			for i := 0; i < size.fileCount; i++ {
				payloadID := fmt.Sprintf("export/file-%d.txt", i)
				_ = blockStore.WriteBlock(ctx, payloadID+"/chunk-0/block-0", make([]byte, 1024)) // Error ignored in benchmark setup

				if i%2 == 0 {
					store.addFile(payloadID) // Add every other file to metadata
				}
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				// Use dry run to not modify the store between iterations
				CollectGarbage(ctx, blockStore, reconciler, &GCOptions{DryRun: true})
			}
		})
	}
}

func BenchmarkParsePayloadIDFromBlockKey(b *testing.B) {
	blockKey := "export/deep/nested/path/document.pdf/chunk-100/block-50"

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = parsePayloadIDFromBlockKey(blockKey)
	}
}
