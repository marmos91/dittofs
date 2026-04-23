//go:build integration

package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	remotes3 "github.com/marmos91/dittofs/pkg/blockstore/remote/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Note: Localstack container setup (sharedHelper, localstackHelper,
// startSharedLocalstack, TestMain) lives in syncer_test.go after the
// TD-01 merge of the readbuffer/sync/gc packages into engine.

// ============================================================================
// Memory GC Integration Tests
// ============================================================================

func TestCollectGarbage_Memory(t *testing.T) {
	ctx := context.Background()

	// Create memory remote store
	remoteStore := remotememory.New()
	defer remoteStore.Close()

	// Create blocks for two files
	validPayloadID := "export/valid-file.txt"
	orphanPayloadID := "export/orphan-file.txt"

	// Write blocks
	data := make([]byte, 1024)
	require.NoError(t, remoteStore.WriteBlock(ctx, validPayloadID+"/block-0", data))
	require.NoError(t, remoteStore.WriteBlock(ctx, orphanPayloadID+"/block-0", data))
	require.NoError(t, remoteStore.WriteBlock(ctx, orphanPayloadID+"/block-1", data))

	// Set up reconciler with only valid file
	reconciler := newGCTestReconciler()
	store := reconciler.addShare("/export")
	createFileWithPayloadID(ctx, t, store, "/export", validPayloadID)

	// Run GC
	stats := CollectGarbage(ctx, remoteStore, reconciler, nil)

	assert.Equal(t, 3, stats.BlocksScanned)
	assert.Equal(t, 1, stats.OrphanFiles)
	assert.Equal(t, 2, stats.OrphanBlocks)
	assert.Equal(t, 0, stats.Errors)

	// Verify orphan blocks were deleted
	keys, _ := remoteStore.ListByPrefix(ctx, orphanPayloadID)
	assert.Empty(t, keys, "orphan blocks should be deleted")

	// Verify valid blocks still exist
	keys, _ = remoteStore.ListByPrefix(ctx, validPayloadID)
	assert.Len(t, keys, 1, "valid blocks should remain")
}

func TestCollectGarbage_Memory_LargeScale(t *testing.T) {
	ctx := context.Background()

	// Create memory remote store
	remoteStore := remotememory.New()
	defer remoteStore.Close()

	// Create 100 files, 50% orphans
	reconciler := newGCTestReconciler()
	store := reconciler.addShare("/export")

	data := make([]byte, 1024)
	for i := 0; i < 100; i++ {
		payloadID := fmt.Sprintf("export/file-%d.txt", i)
		require.NoError(t, remoteStore.WriteBlock(ctx, payloadID+"/block-0", data))

		if i%2 == 0 {
			createFileWithPayloadID(ctx, t, store, "/export", payloadID) // 50 valid files
		}
	}

	// Run GC
	stats := CollectGarbage(ctx, remoteStore, reconciler, nil)

	assert.Equal(t, 100, stats.BlocksScanned)
	assert.Equal(t, 50, stats.OrphanFiles)
	assert.Equal(t, 50, stats.OrphanBlocks)
}

// ============================================================================
// S3 GC Integration Tests
// ============================================================================

func TestCollectGarbage_S3(t *testing.T) {
	ctx := context.Background()

	// Create a dedicated bucket for GC testing with unique name to avoid flakiness.
	bucketName := fmt.Sprintf("gc-test-%d", time.Now().UnixNano())
	sharedHelper.createBucket(t, bucketName)

	// Create S3 remote store using the shared helper's client.
	remoteStore := remotes3.New(sharedHelper.client, remotes3.Config{
		Bucket:    bucketName,
		KeyPrefix: "blocks/",
	})

	// Create blocks for two files
	validPayloadID := "export/valid-file.txt"
	orphanPayloadID := "export/orphan-file.txt"

	data := make([]byte, 1024)
	require.NoError(t, remoteStore.WriteBlock(ctx, validPayloadID+"/block-0", data))
	require.NoError(t, remoteStore.WriteBlock(ctx, orphanPayloadID+"/block-0", data))
	require.NoError(t, remoteStore.WriteBlock(ctx, orphanPayloadID+"/block-1", data))

	// Set up reconciler with only valid file
	reconciler := newGCTestReconciler()
	store := reconciler.addShare("/export")
	createFileWithPayloadID(ctx, t, store, "/export", validPayloadID)

	// Run GC
	stats := CollectGarbage(ctx, remoteStore, reconciler, nil)

	assert.Equal(t, 3, stats.BlocksScanned)
	assert.Equal(t, 1, stats.OrphanFiles)
	assert.Equal(t, 2, stats.OrphanBlocks)
	assert.Equal(t, 0, stats.Errors)

	// Verify orphan blocks were deleted from S3
	keys, _ := remoteStore.ListByPrefix(ctx, orphanPayloadID)
	assert.Empty(t, keys, "orphan blocks should be deleted from S3")

	// Verify valid blocks still exist in S3
	keys, _ = remoteStore.ListByPrefix(ctx, validPayloadID)
	assert.Len(t, keys, 1, "valid blocks should remain in S3")
}

// ============================================================================
// Memory Benchmarks
// ============================================================================

func BenchmarkCollectGarbage_Memory(b *testing.B) {
	ctx := context.Background()

	// Create memory remote store
	remoteStore := remotememory.New()
	defer remoteStore.Close()

	// Set up reconciler with 50% orphans
	reconciler := newGCTestReconciler()
	store := reconciler.addShare("/export")

	data := make([]byte, 1024)
	for i := 0; i < 100; i++ {
		payloadID := fmt.Sprintf("export/file-%d.txt", i)
		remoteStore.WriteBlock(ctx, payloadID+"/block-0", data)

		if i%2 == 0 {
			createFileWithPayloadID(ctx, b, store, "/export", payloadID)
		}
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// Use dry run to avoid modifying the store
		CollectGarbage(ctx, remoteStore, reconciler, &Options{DryRun: true})
	}
}
