package engine

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"lukechampine.com/blake3"
)

// newPendingSetFixture builds an engine.Store with a memory remote + a
// memory SyncedHashStore so the upload mirror loop is exercised.
func newPendingSetFixture(t *testing.T) (*Store, *fs.FSStore, *metadatamemory.MemoryMetadataStore) {
	t.Helper()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, 16*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes:     128 * 1024 * 1024,
		RollupWorkers:   2,
		StabilizationMS: 5,
		RollupStore:     ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	remote := remotememory.New()
	syncer := NewSyncer(localStore, remote, ms, DefaultConfig())
	bs, err := New(BlockStoreConfig{
		Local:           localStore,
		Remote:          remote,
		Syncer:          syncer,
		FileBlockStore:  ms,
		SyncedHashStore: ms,
		ReadBufferBytes: 64 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs, localStore, ms
}

func (m *Syncer) pendingLen() int {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	return len(m.pendingHashes)
}

// TestSyncer_StoreChunk_PopulatesPendingSet proves a newly-stored CAS
// chunk is registered for upload via the onChunkComplete chokepoint —
// without any directory walk.
func TestSyncer_StoreChunk_PopulatesPendingSet(t *testing.T) {
	bs, localStore, _ := newPendingSetFixture(t)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0x7E}, 4096)
	h := block.ContentHash(blake3.Sum256(data))
	if err := localStore.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}

	bs.syncer.pendingMu.Lock()
	_, ok := bs.syncer.pendingHashes[h]
	bs.syncer.pendingMu.Unlock()
	if !ok {
		t.Fatal("StoreChunk did not register the hash in the syncer pending set")
	}
}

// TestSyncer_MirrorOnce_DrainsPendingSet proves mirrorOnce uploads every
// pending hash, marks it synced, and removes it from the set.
func TestSyncer_MirrorOnce_DrainsPendingSet(t *testing.T) {
	bs, localStore, ms := newPendingSetFixture(t)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0x22}, 4096)
	h := block.ContentHash(blake3.Sum256(data))
	if err := localStore.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}
	if bs.syncer.pendingLen() == 0 {
		t.Fatal("precondition: hash not pending after StoreChunk")
	}

	if err := bs.syncer.mirrorOnce(ctx); err != nil {
		t.Fatalf("mirrorOnce: %v", err)
	}

	if n := bs.syncer.pendingLen(); n != 0 {
		t.Fatalf("pending set not drained after mirrorOnce: %d remain", n)
	}
	synced, err := ms.IsSynced(ctx, h)
	if err != nil {
		t.Fatalf("IsSynced: %v", err)
	}
	if !synced {
		t.Fatal("hash not MarkSynced after mirrorOnce")
	}
}

// TestSyncer_SeedPendingFromDisk_RecoversUnsynced proves the startup
// reconciliation re-seeds the volatile pending set from disk: an unsynced
// chunk written before a (simulated) restart is rediscovered, while an
// already-synced chunk is not.
func TestSyncer_SeedPendingFromDisk_RecoversUnsynced(t *testing.T) {
	bs, localStore, ms := newPendingSetFixture(t)
	ctx := context.Background()

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
	if err := ms.MarkSynced(ctx, sh); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	// Simulate restart: a fresh syncer with an empty pending set over the
	// same local store + synced-hash store.
	fresh := NewSyncer(localStore, bs.syncer.remoteStore, ms, DefaultConfig())
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
	_, hasUnsynced := fresh.pendingHashes[uh]
	_, hasSynced := fresh.pendingHashes[sh]
	fresh.pendingMu.Unlock()
	if !hasUnsynced {
		t.Error("unsynced chunk not re-seeded after restart")
	}
	if hasSynced {
		t.Error("already-synced chunk wrongly re-seeded")
	}
}
