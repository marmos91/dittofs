//go:build integration

package transfer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/payload/store/fs"
	s3store "github.com/marmos91/dittofs/pkg/payload/store/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Filesystem GC Integration Tests
// ============================================================================

func TestCollectGarbage_Filesystem(t *testing.T) {
	ctx := context.Background()

	// Create temp directory for filesystem store
	tmpDir, err := os.MkdirTemp("", "gc-fs-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Create filesystem block store
	blockStore, err := fs.New(fs.Config{
		BasePath:  tmpDir,
		CreateDir: true,
	})
	require.NoError(t, err)
	defer blockStore.Close()

	// Create blocks for two files
	validPayloadID := "export/valid-file.txt"
	orphanPayloadID := "export/orphan-file.txt"

	// Write blocks
	data := make([]byte, 1024)
	require.NoError(t, blockStore.WriteBlock(ctx, validPayloadID+"/chunk-0/block-0", data))
	require.NoError(t, blockStore.WriteBlock(ctx, orphanPayloadID+"/chunk-0/block-0", data))
	require.NoError(t, blockStore.WriteBlock(ctx, orphanPayloadID+"/chunk-0/block-1", data))

	// Set up reconciler with only valid file
	reconciler := newGCTestReconciler()
	store := reconciler.addShare("/export")
	createFileWithPayloadID(ctx, t, store, "/export", validPayloadID)

	// Run GC
	stats := CollectGarbage(ctx, blockStore, reconciler, nil)

	assert.Equal(t, 3, stats.BlocksScanned)
	assert.Equal(t, 1, stats.OrphanFiles)
	assert.Equal(t, 2, stats.OrphanBlocks)
	assert.Equal(t, 0, stats.Errors)

	// Verify orphan blocks were deleted
	keys, _ := blockStore.ListByPrefix(ctx, orphanPayloadID)
	assert.Empty(t, keys, "orphan blocks should be deleted from filesystem")

	// Verify valid blocks still exist
	keys, _ = blockStore.ListByPrefix(ctx, validPayloadID)
	assert.Len(t, keys, 1, "valid blocks should remain on filesystem")

	// Verify directory was cleaned up
	orphanDir := filepath.Join(tmpDir, orphanPayloadID)
	_, err = os.Stat(orphanDir)
	assert.True(t, os.IsNotExist(err), "orphan directory should be removed")
}

func TestCollectGarbage_Filesystem_LargeScale(t *testing.T) {
	ctx := context.Background()

	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "gc-fs-large-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Create filesystem block store
	blockStore, err := fs.New(fs.Config{
		BasePath:  tmpDir,
		CreateDir: true,
	})
	require.NoError(t, err)
	defer blockStore.Close()

	// Create 100 files, 50% orphans
	reconciler := newGCTestReconciler()
	store := reconciler.addShare("/export")

	data := make([]byte, 1024)
	for i := 0; i < 100; i++ {
		payloadID := fmt.Sprintf("export/file-%d.txt", i)
		require.NoError(t, blockStore.WriteBlock(ctx, payloadID+"/chunk-0/block-0", data))

		if i%2 == 0 {
			createFileWithPayloadID(ctx, t, store, "/export", payloadID) // 50 valid files
		}
	}

	// Run GC
	stats := CollectGarbage(ctx, blockStore, reconciler, nil)

	assert.Equal(t, 100, stats.BlocksScanned)
	assert.Equal(t, 50, stats.OrphanFiles)
	assert.Equal(t, 50, stats.OrphanBlocks)
}

// ============================================================================
// S3 GC Integration Tests
// ============================================================================

func TestCollectGarbage_S3(t *testing.T) {
	ctx := context.Background()

	// Reuse the shared localstackHelper (same as manager_test.go).
	helper := newLocalstackHelper(t)
	defer helper.close(t)

	// Create a dedicated bucket for GC testing.
	helper.createBucket(t, "gc-test-bucket")

	// Create S3 block store using the helper's client.
	blockStore := s3store.New(helper.client, s3store.Config{
		Bucket:    "gc-test-bucket",
		KeyPrefix: "blocks/",
	})

	// Create blocks for two files
	validPayloadID := "export/valid-file.txt"
	orphanPayloadID := "export/orphan-file.txt"

	data := make([]byte, 1024)
	require.NoError(t, blockStore.WriteBlock(ctx, validPayloadID+"/chunk-0/block-0", data))
	require.NoError(t, blockStore.WriteBlock(ctx, orphanPayloadID+"/chunk-0/block-0", data))
	require.NoError(t, blockStore.WriteBlock(ctx, orphanPayloadID+"/chunk-0/block-1", data))

	// Set up reconciler with only valid file
	reconciler := newGCTestReconciler()
	store := reconciler.addShare("/export")
	createFileWithPayloadID(ctx, t, store, "/export", validPayloadID)

	// Run GC
	stats := CollectGarbage(ctx, blockStore, reconciler, nil)

	assert.Equal(t, 3, stats.BlocksScanned)
	assert.Equal(t, 1, stats.OrphanFiles)
	assert.Equal(t, 2, stats.OrphanBlocks)
	assert.Equal(t, 0, stats.Errors)

	// Verify orphan blocks were deleted from S3
	keys, _ := blockStore.ListByPrefix(ctx, orphanPayloadID)
	assert.Empty(t, keys, "orphan blocks should be deleted from S3")

	// Verify valid blocks still exist in S3
	keys, _ = blockStore.ListByPrefix(ctx, validPayloadID)
	assert.Len(t, keys, 1, "valid blocks should remain in S3")
}

// ============================================================================
// S3 Benchmarks
// ============================================================================

func BenchmarkCollectGarbage_Filesystem(b *testing.B) {
	ctx := context.Background()

	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "gc-bench-*")
	require.NoError(b, err)
	defer os.RemoveAll(tmpDir)

	// Create filesystem block store
	blockStore, err := fs.New(fs.Config{
		BasePath:  tmpDir,
		CreateDir: true,
	})
	require.NoError(b, err)
	defer blockStore.Close()

	// Set up reconciler with 50% orphans
	reconciler := newGCTestReconciler()
	store := reconciler.addShare("/export")

	data := make([]byte, 1024)
	for i := 0; i < 100; i++ {
		payloadID := fmt.Sprintf("export/file-%d.txt", i)
		blockStore.WriteBlock(ctx, payloadID+"/chunk-0/block-0", data)

		if i%2 == 0 {
			createFileWithPayloadID(ctx, b, store, "/export", payloadID)
		}
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// Use dry run to avoid modifying the store
		CollectGarbage(ctx, blockStore, reconciler, &GCOptions{DryRun: true})
	}
}
