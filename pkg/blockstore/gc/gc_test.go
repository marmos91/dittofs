package gc

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
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
			blockKey: "export/file.txt/block-0",
			expected: "export/file.txt",
		},
		{
			name:     "nested path",
			blockKey: "export/deep/nested/path/document.pdf/block-5",
			expected: "export/deep/nested/path/document.pdf",
		},
		{
			name:     "file at root of share",
			blockKey: "myshare/readme.txt/block-0",
			expected: "myshare/readme.txt",
		},
		{
			name:     "high block index",
			blockKey: "export/large-file.bin/block-3",
			expected: "export/large-file.bin",
		},
		{
			name:     "empty string",
			blockKey: "",
			expected: "",
		},
		{
			name:     "no block marker",
			blockKey: "export/file.txt",
			expected: "",
		},
		{
			name:     "block at start",
			blockKey: "/block-0",
			expected: "",
		},
		{
			name:     "only block marker",
			blockKey: "block-0",
			expected: "",
		},
		{
			name:     "path with hyphen",
			blockKey: "export/my-file/block-0",
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
	remoteStore := remotememory.New()
	defer func() { _ = remoteStore.Close() }()

	reconciler := newGCTestReconciler()

	stats := CollectGarbage(ctx, remoteStore, reconciler, nil)

	assert.NotNil(t, stats)
	assert.Equal(t, 0, stats.BlocksScanned)
	assert.Equal(t, 0, stats.OrphanBlocks)
	assert.Equal(t, int64(0), stats.BytesReclaimed)
}

func TestCollectGarbage_NoOrphans(t *testing.T) {
	ctx := context.Background()
	remoteStore := remotememory.New()
	defer func() { _ = remoteStore.Close() }()

	// Add a block to the store
	blockKey := "export/test-file.txt/block-0"
	require.NoError(t, remoteStore.WriteBlock(ctx, blockKey, []byte("test data")))

	// Create reconciler with a share and file that owns this block
	reconciler := newGCTestReconciler()
	store := reconciler.addShare("/export")
	createFileWithPayloadID(ctx, t, store, "/export", "export/test-file.txt")

	stats := CollectGarbage(ctx, remoteStore, reconciler, nil)

	assert.Equal(t, 1, stats.BlocksScanned)
	assert.Equal(t, 0, stats.OrphanBlocks)
	assert.Equal(t, int64(0), stats.BytesReclaimed)

	// Verify block still exists
	data, err := remoteStore.ReadBlock(ctx, blockKey)
	assert.NoError(t, err)
	assert.Equal(t, []byte("test data"), data)
}

func TestCollectGarbage_DeletesOrphans(t *testing.T) {
	ctx := context.Background()
	remoteStore := remotememory.New()
	defer func() { _ = remoteStore.Close() }()

	// Add an orphan block (no file references it)
	blockKey := "export/deleted-file.txt/block-0"
	require.NoError(t, remoteStore.WriteBlock(ctx, blockKey, []byte("orphan data")))

	// Create reconciler with share but no file for this payload
	reconciler := newGCTestReconciler()
	reconciler.addShare("/export")

	stats := CollectGarbage(ctx, remoteStore, reconciler, nil)

	assert.Equal(t, 1, stats.BlocksScanned)
	assert.Equal(t, 1, stats.OrphanBlocks)
	assert.Equal(t, int64(BlockSize), stats.BytesReclaimed) // GC estimates by block size

	// Verify block was deleted
	_, err := remoteStore.ReadBlock(ctx, blockKey)
	assert.Error(t, err)
}

func TestCollectGarbage_MixedOrphansAndValid(t *testing.T) {
	ctx := context.Background()
	remoteStore := remotememory.New()
	defer func() { _ = remoteStore.Close() }()

	// Add a valid block
	validKey := "export/valid-file.txt/block-0"
	require.NoError(t, remoteStore.WriteBlock(ctx, validKey, []byte("valid")))

	// Add an orphan block
	orphanKey := "export/orphan-file.txt/block-0"
	require.NoError(t, remoteStore.WriteBlock(ctx, orphanKey, []byte("orphan")))

	// Create reconciler with share and only the valid file
	reconciler := newGCTestReconciler()
	store := reconciler.addShare("/export")
	createFileWithPayloadID(ctx, t, store, "/export", "export/valid-file.txt")

	stats := CollectGarbage(ctx, remoteStore, reconciler, nil)

	assert.Equal(t, 2, stats.BlocksScanned)
	assert.Equal(t, 1, stats.OrphanBlocks)
	assert.Equal(t, int64(BlockSize), stats.BytesReclaimed)

	// Valid block should still exist
	data, err := remoteStore.ReadBlock(ctx, validKey)
	assert.NoError(t, err)
	assert.Equal(t, []byte("valid"), data)

	// Orphan should be deleted
	_, err = remoteStore.ReadBlock(ctx, orphanKey)
	assert.Error(t, err)
}

func TestCollectGarbage_UnknownShare(t *testing.T) {
	ctx := context.Background()
	remoteStore := remotememory.New()
	defer func() { _ = remoteStore.Close() }()

	// Add a block for an unknown share
	blockKey := "unknown-share/file.txt/block-0"
	require.NoError(t, remoteStore.WriteBlock(ctx, blockKey, []byte("data")))

	// Create reconciler with a different share
	reconciler := newGCTestReconciler()
	reconciler.addShare("/export")

	stats := CollectGarbage(ctx, remoteStore, reconciler, nil)

	// Block should be deleted (unknown share = orphan)
	assert.Equal(t, 1, stats.BlocksScanned)
	assert.Equal(t, 1, stats.OrphanBlocks)
}

func TestCollectGarbage_ProgressCallback(t *testing.T) {
	ctx := context.Background()
	remoteStore := remotememory.New()
	defer func() { _ = remoteStore.Close() }()

	// Add some orphan blocks (one per "file" to trigger callback per file)
	for i := 0; i < 5; i++ {
		blockKey := fmt.Sprintf("export/file%d.txt/block-0", i)
		require.NoError(t, remoteStore.WriteBlock(ctx, blockKey, []byte("data")))
	}

	reconciler := newGCTestReconciler()
	reconciler.addShare("/export")

	var progressCalls int
	progress := func(stats Stats) {
		progressCalls++
		assert.GreaterOrEqual(t, stats.OrphanFiles, 0)
		assert.LessOrEqual(t, stats.OrphanBlocks, stats.BlocksScanned)
	}

	CollectGarbage(ctx, remoteStore, reconciler, &Options{ProgressCallback: progress})

	// Progress should have been called once per orphan file
	assert.Equal(t, 5, progressCalls)
}

// ============================================================================
// BackupHoldProvider Tests (Phase 5 D-11)
// ============================================================================

// fakeBackupHold is a testing fake for gc.BackupHoldProvider. If err != nil it
// is returned from HeldPayloadIDs; otherwise the preconfigured held set is
// returned (nil held map is allowed — equivalent to empty set).
type fakeBackupHold struct {
	held map[metadata.PayloadID]struct{}
	err  error
}

func (f *fakeBackupHold) HeldPayloadIDs(_ context.Context) (map[metadata.PayloadID]struct{}, error) {
	return f.held, f.err
}

// TestGC_BackupHold_PreservesHeldPayload verifies that a payloadID which has
// no live metadata reference but IS in the backup hold set is retained (not
// treated as an orphan, not deleted from the remote store).
func TestGC_BackupHold_PreservesHeldPayload(t *testing.T) {
	ctx := context.Background()
	remoteStore := remotememory.New()
	defer func() { _ = remoteStore.Close() }()

	// Add a block that no metadata references (would normally be orphan).
	heldPayloadID := "export/held-file.txt"
	blockKey := heldPayloadID + "/block-0"
	require.NoError(t, remoteStore.WriteBlock(ctx, blockKey, []byte("held data")))

	// Share exists but no file references this payloadID.
	reconciler := newGCTestReconciler()
	reconciler.addShare("/export")

	hold := &fakeBackupHold{
		held: map[metadata.PayloadID]struct{}{
			metadata.PayloadID(heldPayloadID): {},
		},
	}

	stats := CollectGarbage(ctx, remoteStore, reconciler, &Options{BackupHold: hold})

	assert.Equal(t, 1, stats.BlocksScanned)
	assert.Equal(t, 0, stats.OrphanFiles, "held payload should not be accounted as orphan")
	assert.Equal(t, 0, stats.OrphanBlocks)
	assert.Equal(t, int64(0), stats.BytesReclaimed)

	// Block should still exist — hold preserved it.
	data, err := remoteStore.ReadBlock(ctx, blockKey)
	assert.NoError(t, err, "held block must NOT be deleted")
	assert.Equal(t, []byte("held data"), data)
}

// TestGC_BackupHold_OrphanStillDeletedWhenNotHeld verifies that payloads
// absent from the hold set AND the metadata store are still reclaimed.
func TestGC_BackupHold_OrphanStillDeletedWhenNotHeld(t *testing.T) {
	ctx := context.Background()
	remoteStore := remotememory.New()
	defer func() { _ = remoteStore.Close() }()

	// Orphan: no metadata, not in hold set.
	orphanPayloadID := "export/orphan-file.txt"
	orphanKey := orphanPayloadID + "/block-0"
	require.NoError(t, remoteStore.WriteBlock(ctx, orphanKey, []byte("orphan")))

	// Hold set covers a DIFFERENT payloadID — does not protect the orphan.
	reconciler := newGCTestReconciler()
	reconciler.addShare("/export")
	hold := &fakeBackupHold{
		held: map[metadata.PayloadID]struct{}{
			metadata.PayloadID("export/some-other-file.txt"): {},
		},
	}

	stats := CollectGarbage(ctx, remoteStore, reconciler, &Options{BackupHold: hold})

	assert.Equal(t, 1, stats.BlocksScanned)
	assert.Equal(t, 1, stats.OrphanFiles)
	assert.Equal(t, 1, stats.OrphanBlocks)

	// Orphan should be deleted.
	_, err := remoteStore.ReadBlock(ctx, orphanKey)
	assert.Error(t, err, "unheld orphan should be deleted")
}

// TestGC_BackupHold_ProviderError_FailsOpen verifies that when the hold
// provider returns an error, GC proceeds WITHOUT a hold (logs WARN) and
// continues to delete orphans. Under-hold is preferred over aborting GC.
func TestGC_BackupHold_ProviderError_FailsOpen(t *testing.T) {
	ctx := context.Background()
	remoteStore := remotememory.New()
	defer func() { _ = remoteStore.Close() }()

	// Orphan block that would be protected IF the hold set were consulted.
	payloadID := "export/would-be-held.txt"
	blockKey := payloadID + "/block-0"
	require.NoError(t, remoteStore.WriteBlock(ctx, blockKey, []byte("data")))

	reconciler := newGCTestReconciler()
	reconciler.addShare("/export")

	// Provider fails — fail-open means the orphan is still reclaimed.
	hold := &fakeBackupHold{
		err: errors.New("destination unavailable"),
	}

	stats := CollectGarbage(ctx, remoteStore, reconciler, &Options{BackupHold: hold})

	assert.Equal(t, 1, stats.BlocksScanned)
	assert.Equal(t, 1, stats.OrphanFiles, "fail-open: orphan still accounted")
	assert.Equal(t, 1, stats.OrphanBlocks)

	// Orphan deleted — GC did not abort on provider error.
	_, err := remoteStore.ReadBlock(ctx, blockKey)
	assert.Error(t, err, "fail-open: orphan should still be deleted")
}

// TestGC_NilBackupHold_PreservesLegacyBehavior is a regression test verifying
// that Options.BackupHold == nil keeps pre-Phase-5 behavior intact.
func TestGC_NilBackupHold_PreservesLegacyBehavior(t *testing.T) {
	ctx := context.Background()
	remoteStore := remotememory.New()
	defer func() { _ = remoteStore.Close() }()

	// Plain orphan.
	blockKey := "export/orphan.txt/block-0"
	require.NoError(t, remoteStore.WriteBlock(ctx, blockKey, []byte("data")))

	reconciler := newGCTestReconciler()
	reconciler.addShare("/export")

	// Options with BackupHold explicitly nil.
	stats := CollectGarbage(ctx, remoteStore, reconciler, &Options{BackupHold: nil})

	assert.Equal(t, 1, stats.BlocksScanned)
	assert.Equal(t, 1, stats.OrphanFiles)
	assert.Equal(t, 1, stats.OrphanBlocks)

	// Orphan deleted as in pre-Phase-5 behavior.
	_, err := remoteStore.ReadBlock(ctx, blockKey)
	assert.Error(t, err)
}

func TestCollectGarbage_DryRun(t *testing.T) {
	ctx := context.Background()
	remoteStore := remotememory.New()
	defer func() { _ = remoteStore.Close() }()

	// Add an orphan block
	blockKey := "export/orphan.txt/block-0"
	require.NoError(t, remoteStore.WriteBlock(ctx, blockKey, []byte("orphan data")))

	reconciler := newGCTestReconciler()
	reconciler.addShare("/export")

	// Run with dry run - should NOT delete
	stats := CollectGarbage(ctx, remoteStore, reconciler, &Options{DryRun: true})

	assert.Equal(t, 1, stats.OrphanBlocks)

	// Block should still exist
	data, err := remoteStore.ReadBlock(ctx, blockKey)
	assert.NoError(t, err)
	assert.Equal(t, []byte("orphan data"), data)
}
