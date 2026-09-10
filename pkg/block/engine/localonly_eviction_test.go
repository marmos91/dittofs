package engine_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// localOnlyPair is newLocalOnlyEngine but hands back the local store too, so the
// test can drive the journal directly the way a replayed journal would arrive.
func localOnlyPair(t *testing.T, ms metadata.Store) (*engine.Store, *fs.FSStore) {
	t.Helper()
	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes: 128 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	syncedHashStore, ok := ms.(metadata.SyncedHashStore)
	if !ok {
		t.Fatalf("metadata store %T is not a SyncedHashStore", ms)
	}
	syncer := engine.NewRemoteSync(localStore, nil, ms, engine.DefaultConfig())
	bs, err := engine.New(engine.BlockStoreConfig{
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

// TestLocalOnlyShareNeverEvictsItsOnlyCopy pins #2314. A journal replayed from
// a share's earlier remote-backed life carries records still flagged synced. If
// that journal is reopened under a share with no remote, those records satisfy
// the eviction gate — and there is nothing to fetch them back from, so eviction
// destroys the only copy.
//
// The gate used to be IsRemoteHealthy(), which reports true for a nil health
// monitor, so a share with no remote at all read as healthy. It is now
// CanEvict(), which also requires the carve path to be wired to a remote.
func TestLocalOnlyShareNeverEvictsItsOnlyCopy(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs, localStore := localOnlyPair(t, ms)

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
		t.Errorf("evicted %d segment(s) / %d bytes from a share with no remote — "+
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
