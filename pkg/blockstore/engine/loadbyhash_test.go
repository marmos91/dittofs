package engine

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newLoadByHashFixture builds an FS-backed engine.BlockStore plus the
// underlying *fs.FSStore for tests that stage CAS chunks directly via
// StoreChunk. The memory local store used by newTestEngine has no CAS
// layer, so the loadByHash pin tests need this FS-backed variant.
func newLoadByHashFixture(t *testing.T) (*BlockStore, *fs.FSStore) {
	t.Helper()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, 16*1024*1024, ms, fs.FSStoreOptions{})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	syncer := NewSyncer(localStore, nil, ms, DefaultConfig())
	bs, err := New(Config{
		Local:          localStore,
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
	return bs, localStore
}

// TestLoadByHash_DelegatesToLocalGet pins the contract that loadByHash
// reads content-addressed bytes via local.Get(ctx, hash) without
// consulting the FileBlock row. We stage a chunk directly via
// FSStore.StoreChunk (which writes to the CAS layout) and call
// loadByHash with that hash; no FileBlock exists for it. loadByHash
// must return the chunk bytes via local.Get → ReadChunk.
func TestLoadByHash_DelegatesToLocalGet(t *testing.T) {
	bs, localStore := newLoadByHashFixture(t)
	ctx := context.Background()

	// Stage a chunk directly in the CAS layer; no FileBlock row exists.
	var h blockstore.ContentHash
	for i := range h {
		h[i] = byte(0x10 ^ i)
	}
	payload := bytes.Repeat([]byte{0x5A}, 8192)
	if err := localStore.StoreChunk(ctx, h, payload); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}

	got, err := bs.loadByHash(ctx, h)
	if err != nil {
		t.Fatalf("loadByHash: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("loadByHash returned %d bytes; want %d byte-identical payload", len(got), len(payload))
	}
}

// TestLoadByHash_MissingChunkReturnsErrChunkNotFound pins the
// error contract: when the chunk is absent from the CAS layer the
// caller sees blockstore.ErrChunkNotFound, surfaced verbatim from
// local.Get. No FileBlock translation layer between caller and store.
func TestLoadByHash_MissingChunkReturnsErrChunkNotFound(t *testing.T) {
	bs, _ := newLoadByHashFixture(t)

	var missing blockstore.ContentHash
	for i := range missing {
		missing[i] = byte(0xEE)
	}
	_, err := bs.loadByHash(context.Background(), missing)
	// The sentinel surfaces from local.Get verbatim.
	if !errors.Is(err, blockstore.ErrChunkNotFound) {
		t.Fatalf("loadByHash on missing chunk: got %v, want blockstore.ErrChunkNotFound", err)
	}
}
