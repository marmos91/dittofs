package engine

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	blocksync "github.com/marmos91/dittofs/pkg/blockstore/sync"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// waitForUnhealthy polls until the syncer reports unhealthy or timeout.
func waitForUnhealthy(t *testing.T, bs *BlockStore, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !bs.syncer.IsRemoteHealthy() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for remote to become unhealthy")
}

// TestOfflineReadCachedBlockSucceeds proves RESIL-01:
// When remote is unhealthy, reading a locally-cached block still works.
func TestOfflineReadCachedBlockSucceeds(t *testing.T) {
	bs, fakeRemote := newHealthTestEngine(t)
	ctx := context.Background()

	payloadID := "export/offline-cached-read.bin"
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i % 256)
	}

	// Write data (goes to local cache).
	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Flush to persist locally.
	if _, err := bs.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	// Mark remote unhealthy.
	fakeRemote.SetHealthy(false)
	waitForUnhealthy(t, bs, 500*time.Millisecond)

	// ReadAt should succeed (data is in local cache).
	readBuf := make([]byte, 4096)
	n, err := bs.ReadAt(ctx, payloadID, readBuf, 0)
	if err != nil {
		t.Fatalf("ReadAt failed during outage: %v", err)
	}
	if n != len(data) {
		t.Fatalf("expected %d bytes read, got %d", len(data), n)
	}

	if !bytes.Equal(readBuf, data) {
		t.Fatal("data mismatch: read buffer does not match written data")
	}
}

// TestOfflineReadRemoteOnlyBlockFails proves RESIL-02:
// When remote is unhealthy, reading a block only in S3 returns ErrRemoteUnavailable.
func TestOfflineReadRemoteOnlyBlockFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows: fsync fails on temp dirs, preventing remote sync")
	}
	bs, fakeRemote := newHealthTestEngine(t)
	ctx := context.Background()

	payloadID := "export/offline-remote-only.bin"
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i % 256)
	}

	// Write data (goes to local cache).
	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Flush and sync to remote.
	if _, err := bs.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	// SyncNow holds the uploading gate end-to-end and uploads synchronously,
	// so by the time it returns every block is in the remote store.
	bs.syncer.SyncNow(ctx)

	// Verify block is in remote.
	memStore := fakeRemote.RemoteStore.(*remotememory.Store)
	if memStore.BlockCount() == 0 {
		t.Fatal("expected block to be uploaded to remote")
	}

	// Evict from local cache.
	if err := bs.EvictLocal(ctx, payloadID); err != nil {
		t.Fatalf("EvictLocal failed: %v", err)
	}

	// Mark remote unhealthy.
	fakeRemote.SetHealthy(false)
	waitForUnhealthy(t, bs, 500*time.Millisecond)

	// ReadAt should fail with ErrRemoteUnavailable.
	readBuf := make([]byte, 4096)
	_, err := bs.ReadAt(ctx, payloadID, readBuf, 0)
	if err == nil {
		t.Fatal("expected error for remote-only block during outage, got nil")
	}
	if !errors.Is(err, blockstore.ErrRemoteUnavailable) {
		t.Fatalf("expected ErrRemoteUnavailable, got: %v", err)
	}
}

// TestOfflineWriteSucceeds proves RESIL-03:
// When remote is unhealthy, writes succeed (go to local store).
func TestOfflineWriteSucceeds(t *testing.T) {
	bs, fakeRemote := newHealthTestEngine(t)
	ctx := context.Background()

	// Mark remote unhealthy first.
	fakeRemote.SetHealthy(false)
	waitForUnhealthy(t, bs, 500*time.Millisecond)

	payloadID := "export/offline-write.bin"
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i % 256)
	}

	// WriteAt should succeed (goes to local cache).
	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed during outage: %v", err)
	}

	// Flush should succeed (persists to local disk).
	if _, err := bs.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush failed during outage: %v", err)
	}

	// ReadAt should succeed (data is in local cache).
	readBuf := make([]byte, 4096)
	n, err := bs.ReadAt(ctx, payloadID, readBuf, 0)
	if err != nil {
		t.Fatalf("ReadAt of locally-written data failed: %v", err)
	}
	if n != len(data) {
		t.Fatalf("expected %d bytes read, got %d", len(data), n)
	}
	if !bytes.Equal(readBuf, data) {
		t.Fatal("data mismatch: read buffer does not match written data")
	}
}

// TestOfflineReadsBlockedCounter verifies BlockStoreStats tracks blocked reads.
func TestOfflineReadsBlockedCounter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows: fsync fails on temp dirs, preventing remote sync")
	}
	bs, fakeRemote := newHealthTestEngine(t)
	ctx := context.Background()

	payloadID := "export/offline-counter.bin"
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i % 256)
	}

	// Write, flush, sync, then evict from local.
	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if _, err := bs.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	bs.syncer.SyncNow(ctx)
	if err := bs.EvictLocal(ctx, payloadID); err != nil {
		t.Fatalf("EvictLocal failed: %v", err)
	}

	// Verify initial counter is 0.
	stats := bs.GetStats()
	if stats.OfflineReadsBlocked != 0 {
		t.Fatalf("expected OfflineReadsBlocked == 0 initially, got %d", stats.OfflineReadsBlocked)
	}

	// Mark remote unhealthy.
	fakeRemote.SetHealthy(false)
	waitForUnhealthy(t, bs, 500*time.Millisecond)

	// Attempt reads (should fail and increment counter).
	readBuf := make([]byte, 4096)
	for range 3 {
		_, _ = bs.ReadAt(ctx, payloadID, readBuf, 0)
	}

	stats = bs.GetStats()
	if stats.OfflineReadsBlocked < 3 {
		t.Fatalf("expected OfflineReadsBlocked >= 3 after 3 failed reads, got %d", stats.OfflineReadsBlocked)
	}
}

// TestPrefetchSuppressedWhenUnhealthy verifies prefetch is skipped during outage.
func TestPrefetchSuppressedWhenUnhealthy(t *testing.T) {
	// Create engine with prefetch enabled.
	tmpDir := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	localStore, err := fs.New(tmpDir, 100*1024*1024, 16*1024*1024, ms)
	if err != nil {
		t.Fatalf("fs.New() error = %v", err)
	}

	fakeRemote := newFakeRemoteStore()

	syncCfg := blocksync.Config{
		ParallelUploads:             4,
		ParallelDownloads:           4,
		PrefetchBlocks:              2, // Enable prefetch
		UploadInterval:              50 * time.Millisecond,
		UploadDelay:                 0,
		HealthCheckInterval:         20 * time.Millisecond,
		HealthCheckFailureThreshold: 2,
		UnhealthyCheckInterval:      10 * time.Millisecond,
	}

	syncer := blocksync.New(localStore, fakeRemote, ms, syncCfg)

	bsEngine, err := New(Config{
		Local:  localStore,
		Remote: fakeRemote,
		Syncer: syncer,
	})
	if err != nil {
		t.Fatalf("engine.New() error = %v", err)
	}
	if err := bsEngine.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start() error = %v", err)
	}
	t.Cleanup(func() { _ = bsEngine.Close() })

	ctx := context.Background()
	payloadID := "export/prefetch-test.bin"
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i % 256)
	}

	// Write and flush a block.
	if err := bsEngine.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if _, err := bsEngine.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	// Mark remote unhealthy.
	fakeRemote.SetHealthy(false)
	waitForUnhealthy(t, bsEngine, 500*time.Millisecond)

	// Get queue stats before reading.
	_, _, prefetchesBefore := syncer.Queue().PendingByType()

	// Read cached block -- should succeed, but prefetch should be suppressed.
	readBuf := make([]byte, 4096)
	_, err = bsEngine.ReadAt(ctx, payloadID, readBuf, 0)
	if err != nil {
		t.Fatalf("ReadAt of cached block failed during outage: %v", err)
	}

	// Short wait for any async prefetch that might be enqueued.
	time.Sleep(50 * time.Millisecond)

	// Get queue stats after reading.
	_, _, prefetchesAfter := syncer.Queue().PendingByType()

	// Prefetch count should not increase (prefetch was suppressed).
	if prefetchesAfter > prefetchesBefore {
		t.Fatalf("expected no new prefetch requests during outage, before=%d after=%d",
			prefetchesBefore, prefetchesAfter)
	}
}
