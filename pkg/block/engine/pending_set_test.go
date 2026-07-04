package engine

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"lukechampine.com/blake3"
)

// newPendingSetFixture builds an engine.Store with a memory remote and the
// full carve substrate wired (block-keyed remote + synced-hash store +
// local-chunk index) so the pending-carve set is exercised end to end.
// startUploader controls whether the fixture launches the background carve
// dispatcher. Tests that observe an intermediate pending-set state must pass
// false: the dispatcher wakes the instant a chunk is registered, so a running
// dispatcher would drain the pending set out from under the assertion (a
// race). Tests that exercise the live pipeline pass true. carveBytes sets the
// target block size (0 = default).
func newPendingSetFixture(t *testing.T, startUploader bool, carveBytes int64) (*Store, *fs.FSStore, *metadatamemory.MemoryMetadataStore) {
	t.Helper()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes:     128 * 1024 * 1024,
		RollupWorkers:   2,
		StabilizationMS: 5,
		RollupStore:     ms,
		SyncedHashStore: ms,
		LocalChunkIndex: ms,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	remote := remotememory.New()
	cfg := DefaultConfig()
	if carveBytes > 0 {
		cfg.BlockCarveBytes = carveBytes
	}
	syncer := NewSyncer(localStore, remote, ms, cfg)
	syncer.SetRemoteBlockStore(remote)
	bs, err := New(BlockStoreConfig{
		Local:           localStore,
		Remote:          remote,
		Syncer:          syncer,
		FileChunkStore:  ms,
		SyncedHashStore: ms,
		ReadBufferBytes: 64 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if !syncer.carveActive.Load() {
		t.Fatal("carve substrate should be active after wiring")
	}
	if startUploader {
		if err := bs.Start(context.Background()); err != nil {
			t.Fatalf("engine.Start: %v", err)
		}
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs, localStore, ms
}

func (m *Syncer) pendingLen() int {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	return len(m.pendingCarveHashes)
}

// TestSyncer_StoreChunk_PopulatesPendingSet proves a newly-stored CAS
// chunk is registered for carve via the onChunkComplete chokepoint —
// without any directory walk.
func TestSyncer_StoreChunk_PopulatesPendingSet(t *testing.T) {
	// No background dispatcher: this test observes the pending set immediately
	// after StoreChunk, which the eager carve dispatcher would otherwise drain.
	bs, localStore, _ := newPendingSetFixture(t, false, 0)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0x7E}, 4096)
	h := block.ContentHash(blake3.Sum256(data))
	if err := localStore.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}

	bs.syncer.pendingMu.Lock()
	_, ok := bs.syncer.pendingCarveHashes[h]
	bs.syncer.pendingMu.Unlock()
	if !ok {
		t.Fatal("StoreChunk did not register the hash in the syncer pending-carve set")
	}
}

// TestSyncer_SyncNow_DrainsPendingSet proves an explicit drain packs every
// pending chunk into a block, marks it synced with a block locator, and
// removes it from the set.
func TestSyncer_SyncNow_DrainsPendingSet(t *testing.T) {
	// No background dispatcher: the drain is driven explicitly here, and a live
	// dispatcher would race the manual drain.
	bs, localStore, ms := newPendingSetFixture(t, false, 0)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0x22}, 4096)
	h := block.ContentHash(blake3.Sum256(data))
	if err := localStore.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}
	if bs.syncer.pendingLen() == 0 {
		t.Fatal("precondition: hash not pending after StoreChunk")
	}

	if err := bs.syncer.SyncNow(ctx); err != nil {
		t.Fatalf("SyncNow: %v", err)
	}

	if n := bs.syncer.pendingLen(); n != 0 {
		t.Fatalf("pending set not drained after SyncNow: %d remain", n)
	}
	synced, err := ms.IsSynced(ctx, h)
	if err != nil {
		t.Fatalf("IsSynced: %v", err)
	}
	if !synced {
		t.Fatal("hash not MarkSynced after SyncNow")
	}
	loc, ok, err := ms.GetLocator(ctx, h)
	if err != nil || !ok || loc.BlockID == "" {
		t.Fatalf("drained chunk has no block locator: ok=%v err=%v loc=%+v", ok, err, loc)
	}
}

// TestSyncer_SeedPendingFromDisk_RecoversUnsynced proves the startup
// reconciliation re-seeds the volatile pending set from disk: an unsynced
// chunk written before a (simulated) restart is rediscovered, while an
// already-synced chunk is not.
func TestSyncer_SeedPendingFromDisk_RecoversUnsynced(t *testing.T) {
	ctx := context.Background()

	// Build the "crashed before upload" disk state directly on a bare local
	// store with NO running engine dispatcher. The engine Store wires
	// StoreChunk's onChunkComplete to syncer.addPendingHash, which immediately
	// wakes the carve dispatcher and packs the chunk — that eager pipelining
	// would MarkSynced the chunk we deliberately want to leave unsynced,
	// defeating the restart simulation. A bare FSStore has no such wiring, so
	// the unsynced chunk stays unsynced until seedPendingFromDisk rediscovers
	// it.
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = ms.Close() })
	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes:     128 * 1024 * 1024,
		RollupWorkers:   2,
		StabilizationMS: 5,
		RollupStore:     ms,
		SyncedHashStore: ms,
		LocalChunkIndex: ms,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = localStore.Close() })

	unsynced := bytes.Repeat([]byte{0x01}, 4096)
	uh := block.ContentHash(blake3.Sum256(unsynced))
	if err := localStore.StoreChunk(ctx, uh, unsynced); err != nil {
		t.Fatalf("StoreChunk unsynced: %v", err)
	}
	syncedData := bytes.Repeat([]byte{0x02}, 4096)
	sh := block.ContentHash(blake3.Sum256(syncedData))
	if err := localStore.StoreChunk(ctx, sh, syncedData); err != nil {
		t.Fatalf("StoreChunk synced: %v", err)
	}
	if err := ms.MarkSynced(ctx, sh, block.ChunkLocator{BlockID: "blk-restart-seed"}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	// Simulate restart: a fresh syncer with an empty pending set over the
	// same local store + synced-hash store.
	fresh := NewSyncer(localStore, remotememory.New(), ms, DefaultConfig())
	fresh.SetSyncedHashStore(ms)
	if fresh.pendingLen() != 0 {
		t.Fatalf("fresh syncer pending set not empty: %d", fresh.pendingLen())
	}

	n, err := fresh.seedPendingFromDisk(ctx)
	if err != nil {
		t.Fatalf("seedPendingFromDisk: %v", err)
	}
	if n != 1 {
		t.Fatalf("seeded %d hashes; want 1 (only the unsynced chunk)", n)
	}
	fresh.pendingMu.Lock()
	_, hasUnsynced := fresh.pendingCarveHashes[uh]
	_, hasSynced := fresh.pendingCarveHashes[sh]
	fresh.pendingMu.Unlock()
	if !hasUnsynced {
		t.Error("unsynced chunk not re-seeded after restart")
	}
	if hasSynced {
		t.Error("already-synced chunk wrongly re-seeded")
	}
}

// TestSyncer_WakePipelinesUploadBeforeTick is the eager-pipelining guard. A
// stored chunk registers in the pending-carve set via onChunkComplete, which
// wakes the carve dispatcher immediately instead of letting it idle until the
// next idle-flush window. With a carve target smaller than the chunk, the
// wake triggers a size-based flush: the chunk must reach the remote well
// inside one upload interval — proving the wake, not the idle timer, drove
// the upload.
func TestSyncer_WakePipelinesUploadBeforeTick(t *testing.T) {
	interval := DefaultConfig().UploadDelay
	if interval < time.Second {
		t.Skipf("UploadDelay %v too short to distinguish wake from idle flush", interval)
	}
	// carveBytes 1024 < the 4096-byte chunk, so the wake-driven pass emits a
	// full block immediately.
	bs, localStore, _ := newPendingSetFixture(t, true, 1024)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0x5A}, 4096)
	h := block.ContentHash(blake3.Sum256(data))
	if err := localStore.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}

	// Generous margin below the idle-flush interval: the wake-driven pass
	// packs to the in-memory remote within milliseconds, so anything well
	// under one interval proves pipelining.
	budget := interval - 500*time.Millisecond
	deadline := time.Now().Add(budget)
	for {
		if c, _ := bs.syncer.SyncCounts(); c >= 1 {
			return // packed before the idle flush — wake pipelining fired.
		}
		if time.Now().After(deadline) {
			t.Fatalf("chunk not packed within %v of the write — wake pipelining did not fire", budget)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
