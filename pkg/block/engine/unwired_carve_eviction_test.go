package engine_test

import (
	"bytes"
	"context"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// unwiredCarvePair builds an engine whose carve path is never wired to the
// remote, and hands back the local store too so the test can drive the journal
// directly the way a replayed journal would arrive.
func unwiredCarvePair(t *testing.T, ms metadata.Store) (*engine.Store, *journal.Store) {
	t.Helper()
	localStore, err := journal.Open(t.TempDir(), journal.Config{MaxLocalBytes: 100 * 1024 * 1024,
		MaxLogBytes: 128 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	syncedHashStore, ok := ms.(metadata.SyncedHashStore)
	if !ok {
		t.Fatalf("metadata store %T is not a SyncedHashStore", ms)
	}
	testRemote := remotememory.New()
	syncer := engine.NewRemoteSync(localStore, testRemote, ms, engine.DefaultConfig())
	bs, err := engine.New(engine.BlockStoreConfig{
		Remote:          testRemote,
		Local:           localStore,
		RemoteSync:      syncer,
		FileChunkStore:  ms,
		Coordinator:     &testCoordinator{store: ms},
		SyncedHashStore: syncedHashStore,
		ReadBufferBytes: 64 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs, localStore
}

// TestUnwiredCarveNeverEvictsItsOnlyCopy pins the eviction gate. A replayed
// journal carries records still flagged synced from an earlier life. While the
// carve path is not wired to the remote those records would satisfy a
// health-only gate — and nothing could fetch them back, so eviction would
// destroy the only copy.
//
// The gate used to be IsRemoteHealthy(), which reports true before the health
// monitor exists. It is now CanEvict(), which also requires the carve path to be
// wired to the remote.
func TestUnwiredCarveNeverEvictsItsOnlyCopy(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs, localStore := unwiredCarvePair(t, ms)

	const payloadID = "p2314"
	want := bytes.Repeat([]byte("A"), 1<<20)

	// Hydrate appends records already marked synced — the state a replay of a
	// formerly remote-backed journal restores.
	if err := localStore.Hydrate(ctx, payloadID, 0, want, 0); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	// Start is what decides the eviction gate for the share.
	if err := bs.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Force-evict. On a share whose bytes cannot be fetched back this must
	// reclaim nothing, however much pressure it is under.
	res, err := localStore.Evict(ctx, 0)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if res.SegmentsEvicted != 0 {
		t.Errorf("evicted %d segment(s) / %d bytes while the carve path was unwired — "+
			"those bytes existed nowhere else", res.SegmentsEvicted, res.BytesFreed)
	}

	// And the bytes must still read back. A read that fails closed is better
	// than zeros, but the data should never have been dropped in the first place.
	got := make([]byte, len(want))
	if _, err := bs.ReadAt(ctx, payloadID, got, 0); err != nil {
		t.Fatalf("read after eviction attempt failed: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("read returned %d zero bytes of %d", bytes.Count(got, []byte{0}), len(got))
	}
}
