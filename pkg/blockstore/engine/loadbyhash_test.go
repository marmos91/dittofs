package engine

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestLoadByHash_DelegatesToLocalGet pins Phase 16 D-02: loadByHash
// must read content-addressed bytes via local.Get(ctx, hash) without
// consulting the FileBlock row. We stage a chunk directly via
// FSStore.StoreChunk (which writes to the CAS layout) and call
// loadByHash with that hash; no FileBlock exists for it.
//
// Pre-rewire (Phase 12 mmap path), loadByHash short-circuits with
// "block not local" because GetByHash returns nil. Post-rewire it
// returns the chunk bytes via local.Get → ReadChunk.
func TestLoadByHash_DelegatesToLocalGet(t *testing.T) {
	tmpDir := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	localStore, err := fs.New(tmpDir, 100*1024*1024, 16*1024*1024, ms)
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}

	syncer := NewSyncer(localStore, nil, ms, DefaultConfig())
	bs, err := New(Config{
		Local:           localStore,
		Remote:          nil,
		Syncer:          syncer,
		FileBlockStore:  ms,
		ReadBufferBytes: 0,
		PrefetchWorkers: 0,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	// Stage a chunk directly in the CAS layer; no FileBlock row exists.
	var h blockstore.ContentHash
	for i := range h {
		h[i] = byte(0x10 ^ i)
	}
	payload := bytes.Repeat([]byte{0x5A}, 8192)
	if err := localStore.StoreChunk(context.Background(), h, payload); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}

	got, err := bs.loadByHash(context.Background(), h)
	if err != nil {
		t.Fatalf("loadByHash: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("loadByHash returned %d bytes; want %d byte-identical payload", len(got), len(payload))
	}
}

// TestLoadByHash_MissingChunkReturnsErrChunkNotFound pins Phase 16 D-02
// error contract: when the chunk is absent from the CAS layer the
// caller sees blockstore.ErrChunkNotFound, surfaced verbatim from
// local.Get. No FileBlock translation layer between caller and store.
func TestLoadByHash_MissingChunkReturnsErrChunkNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	localStore, err := fs.New(tmpDir, 100*1024*1024, 16*1024*1024, ms)
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	syncer := NewSyncer(localStore, nil, ms, DefaultConfig())
	bs, err := New(Config{
		Local:          localStore,
		Remote:         nil,
		Syncer:         syncer,
		FileBlockStore: ms,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	var missing blockstore.ContentHash
	for i := range missing {
		missing[i] = byte(0xEE)
	}
	_, err = bs.loadByHash(context.Background(), missing)
	if err == nil {
		t.Fatal("loadByHash on missing chunk: expected error, got nil")
	}
	// Phase 16 D-02: the sentinel surfaces from local.Get verbatim.
	if err != blockstore.ErrChunkNotFound {
		t.Fatalf("loadByHash on missing chunk: got %v, want blockstore.ErrChunkNotFound", err)
	}
}
