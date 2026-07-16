package engine

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newDrainResetFixture builds a full engine.Store over an fs local
// store + memory metadata, with a LARGE stabilization window and the
// rollup worker pool DELIBERATELY NOT started, so the only way dirty
// append-log bytes reach CAS + the FileChunk manifest is via an explicit
// DrainRollups. This reproduces the snapshot race where a snapshot is
// taken before the async rollup catches up.
func newDrainResetFixture(t *testing.T) (*Store, *fs.FSStore) {
	t.Helper()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, ms, fs.FSStoreOptions{
		MaxLogBytes: 128 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	// Chunking + manifest population now happen at CARVE, which needs a wired
	// remote block sink (there is no local-only rollup). ManualSync keeps
	// DrainRollups the deterministic carve driver.
	rem := remotememory.New()
	cfg := DefaultConfig()
	cfg.ManualSync = true
	syncer := NewSyncer(localStore, rem, ms, cfg)
	bs, err := New(BlockStoreConfig{
		Local:           localStore,
		Remote:          rem,
		Syncer:          syncer,
		FileChunkStore:  ms,
		SyncedHashStore: ms,
		ReadBufferBytes: 64 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	syncer.SetRemoteBlockStore(rem)
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs, localStore
}

// TestEngine_DrainRollups_PopulatesManifest reproduces C1 at the engine
// layer through the REAL write path (bs.WriteAt — NOT pre-populated
// metadata). Before DrainRollups the FileChunk manifest is empty (the
// async rollup never ran), so a metadata Backup taken now would yield an
// empty snapshot manifest. After DrainRollups the manifest is non-empty.
func TestEngine_DrainRollups_PopulatesManifest(t *testing.T) {
	ctx := context.Background()
	bs, _ := newDrainResetFixture(t)

	payloadID := "c1-file"
	data := bytes.Repeat([]byte{0x7E}, 4*1024*1024)
	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	// Pre-drain: manifest must be empty — proves the snapshot race would
	// capture an empty manifest.
	pre, err := bs.fileChunkStore.ListFileChunks(ctx, payloadID)
	if err != nil {
		t.Fatalf("ListFileChunks (pre): %v", err)
	}
	if len(pre) != 0 {
		t.Fatalf("manifest already populated before DrainRollups (%d rows); cannot prove C1", len(pre))
	}

	if err := bs.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups: %v", err)
	}

	post, err := bs.fileChunkStore.ListFileChunks(ctx, payloadID)
	if err != nil {
		t.Fatalf("ListFileChunks (post): %v", err)
	}
	if len(post) == 0 {
		t.Fatal("DrainRollups did not populate the FileChunk manifest (C1: empty snapshot manifest)")
	}
}
