package engine

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// forceRollupOnEngineLocal drives a synchronous rollup pass on the
// engine's underlying FSStore so AppendWrite-staged bytes are chunked
// into CAS objects + FileBlock rows before the test reads them back.
// No-op if the local store is not an *fs.FSStore (memory backend
// already rolls up inline). The helper exists because the post-Phase-18
// engine.Flush deliberately does not synchronously drive rollup — the
// production code path relies on the periodic rollup workers and the
// stabilization window. Tests bypass that timing with this hook.
func forceRollupOnEngineLocal(t *testing.T, bs *BlockStore, payloadID string) {
	t.Helper()
	if fsLocal, ok := bs.local.(*fs.FSStore); ok {
		// Wait past the test config's 50ms stabilization window so
		// EarliestStable considers the freshly-AppendWritten interval
		// rollable. Conservative 80ms margin.
		time.Sleep(80 * time.Millisecond)
		// Drive rollup until the dirty interval drains. ForceRollupForTest
		// commits at most one rollup pass per call, so loop until the
		// next call is a no-op (no new chunks committed). Cap at a
		// generous bound to keep CI deterministic.
		for i := 0; i < 16; i++ {
			if err := fsLocal.ForceRollupForTest(context.Background(), payloadID); err != nil {
				t.Fatalf("ForceRollupForTest: %v", err)
			}
			if fsLocal.IntervalsLenForTest(payloadID) == 0 {
				break
			}
		}
		// Flush queued FileBlock metadata so the engine read path sees
		// the just-published rows immediately.
		fsLocal.SyncFileBlocksForFile(context.Background(), payloadID)
	}
}

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

// TestOfflineReadCachedBlockSucceeds proves RESIL-01
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
	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Flush to persist locally.
	if _, err := bs.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	forceRollupOnEngineLocal(t, bs, payloadID)

	// Mark remote unhealthy.
	fakeRemote.SetHealthy(false)
	waitForUnhealthy(t, bs, 500*time.Millisecond)

	// ReadAt should succeed (data is in local cache).
	readBuf := make([]byte, 4096)
	n, err := bs.ReadAt(ctx, payloadID, nil, readBuf, 0)
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

// No "offline read remote-only block" unit test lives here. Under the
// unified CAS surface, engine.EvictLocal does not eagerly delete
// content-addressed chunks (they may be shared across files via
// file-level dedup); chunk eviction flows through the refcount → GC
// path. The legacy "remote-only block" state would require seeding
// metadata in a way that bypasses the production write path entirely
// so reproducing it at the unit level is infeasible.
// End-to-end "remote unhealthy + only-on-remote" coverage
// moves to -09's integration suite where a real GC pass can
// drain orphan chunks.

// TestOfflineWriteSucceeds proves RESIL-03
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
	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt failed during outage: %v", err)
	}

	// Flush should succeed (persists to local disk).
	if _, err := bs.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush failed during outage: %v", err)
	}
	forceRollupOnEngineLocal(t, bs, payloadID)

	// ReadAt should succeed (data is in local cache).
	readBuf := make([]byte, 4096)
	n, err := bs.ReadAt(ctx, payloadID, nil, readBuf, 0)
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

// No unit-level assertion for the OfflineReadsBlocked counter lives
// here: it required engine.EvictLocal to drive a local CAS chunk gone
// which is no longer the contract — chunks live until refcount → GC
// reaps them. Equivalent coverage lives in the integration suite.

// TestPrefetchSuppressedWhenUnhealthy verifies prefetch is skipped during outage.
func TestPrefetchSuppressedWhenUnhealthy(t *testing.T) {
	// Create engine with prefetch enabled.
	tmpDir := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	localStore, err := fs.NewWithOptions(tmpDir, 100*1024*1024, 16*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes:     128 * 1024 * 1024,
		RollupWorkers:   2,
		StabilizationMS: 50,
		RollupStore:     ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions() error = %v", err)
	}
	if err := localStore.StartRollup(context.Background()); err != nil {
		t.Fatalf("StartRollup: %v", err)
	}

	fakeRemote := newFakeRemoteStore()

	syncCfg := SyncerConfig{
		ParallelUploads:             4,
		ParallelDownloads:           4,
		PrefetchBlocks:              2, // Enable prefetch
		UploadInterval:              50 * time.Millisecond,
		UploadDelay:                 0,
		HealthCheckInterval:         20 * time.Millisecond,
		HealthCheckFailureThreshold: 2,
		UnhealthyCheckInterval:      10 * time.Millisecond,
	}

	syncer := NewSyncer(localStore, fakeRemote, ms, syncCfg)

	bsEngine, err := New(BlockStoreConfig{
		Local:           localStore,
		Remote:          fakeRemote,
		Syncer:          syncer,
		FileBlockStore:  ms,
		SyncedHashStore: ms,
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
	if _, err := bsEngine.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if _, err := bsEngine.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	forceRollupOnEngineLocal(t, bsEngine, payloadID)

	// Mark remote unhealthy.
	fakeRemote.SetHealthy(false)
	waitForUnhealthy(t, bsEngine, 500*time.Millisecond)

	// Get queue stats before reading.
	_, _, prefetchesBefore := syncer.Queue().PendingByType()

	// Read cached block -- should succeed, but prefetch should be suppressed.
	readBuf := make([]byte, 4096)
	_, err = bsEngine.ReadAt(ctx, payloadID, nil, readBuf, 0)
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
