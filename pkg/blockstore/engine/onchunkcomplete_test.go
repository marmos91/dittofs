package engine

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newOnChunkCompleteFixture builds an FS-backed engine.BlockStore with a
// configurable cache budget so the Cache materializes in Start (when
// readBufferBytes > 0) or remains the Null Object (when 0). The
// wire-in is expected to install OnChunkComplete from engine.New via a
// structural-interface assertion on cfg.Local; the fixture is the canonical
// integration site because only the FSStore exposes SetOnChunkComplete.
func newOnChunkCompleteFixture(t *testing.T, readBufferBytes int64) (*BlockStore, *fs.FSStore) {
	t.Helper()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, 16*1024*1024, ms, fs.FSStoreOptions{})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	syncer := NewSyncer(localStore, nil, ms, DefaultConfig())
	bs, err := New(Config{
		Local:           localStore,
		Syncer:          syncer,
		FileBlockStore:  ms,
		ReadBufferBytes: readBufferBytes,
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

// TestEngine_OnChunkComplete_WiredToCache pins the consumer-side
// contract: when the engine's BlockStore is constructed with a non-zero
// cache budget, every successful StoreChunk on the underlying FSStore
// populates bs.cache via the wired OnChunkComplete callback. The
// subsequent bs.cache.Get(hash) hit proves the chunk is in RAM —
// satisfying the "wrote then read" no-disk-hop contract.
func TestEngine_OnChunkComplete_WiredToCache(t *testing.T) {
	bs, localStore := newOnChunkCompleteFixture(t, 64*1024*1024)
	ctx := context.Background()

	var h blockstore.ContentHash
	for i := range h {
		h[i] = byte(0xA0 ^ i)
	}
	payload := bytes.Repeat([]byte{0x5A}, 8192)
	if err := localStore.StoreChunk(ctx, h, payload); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}

	got, ok := bs.cache.Get(h)
	if !ok {
		t.Fatal("bs.cache.Get(h) miss after StoreChunk: OnChunkComplete did not populate the engine Cache")
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("cache returned %d bytes; want %d byte-identical payload", len(got), len(payload))
	}
}

// TestEngine_OnChunkComplete_LargeChunkRespectsCacheCap asserts that
// when a chunk's size exceeds the Cache's maxBytes, Cache.Put silently
// skips it (existing guard at cache.go:233). StoreChunk itself still
// succeeds — the size guard lives in the Cache, not at the chunkstore
// firing site (bounded by Cache LRU; no extra cap upstream).
func TestEngine_OnChunkComplete_LargeChunkRespectsCacheCap(t *testing.T) {
	// 4 KiB cache budget; 8 KiB chunk → Cache.Put guard fires.
	bs, localStore := newOnChunkCompleteFixture(t, 4*1024)
	ctx := context.Background()

	var h blockstore.ContentHash
	for i := range h {
		h[i] = byte(0xB1 ^ i)
	}
	payload := bytes.Repeat([]byte{0xCC}, 8192)
	if err := localStore.StoreChunk(ctx, h, payload); err != nil {
		t.Fatalf("StoreChunk (large): %v", err)
	}

	if _, ok := bs.cache.Get(h); ok {
		t.Fatal("bs.cache.Get(h) HIT for chunk larger than maxBytes; Cache.Put should have skipped")
	}
}

// TestEngine_OnChunkComplete_NilCache_NoPanic asserts the engine
// constructs and serves writes even when the Cache budget is zero
// (nullCache substituted by the constructor). The wired closure must not
// panic when bs.cache is the Null Object — nullCache.Put is a no-op by
// design (cache.go:138), so the binding is safe.
func TestEngine_OnChunkComplete_NilCache_NoPanic(t *testing.T) {
	bs, localStore := newOnChunkCompleteFixture(t, 0)
	ctx := context.Background()

	var h blockstore.ContentHash
	for i := range h {
		h[i] = byte(0xC2 ^ i)
	}
	payload := bytes.Repeat([]byte{0xDD}, 512)
	// Must not panic even though bs.cache is nullCache{}.
	if err := localStore.StoreChunk(ctx, h, payload); err != nil {
		t.Fatalf("StoreChunk with nullCache: %v", err)
	}
	// nullCache.Get always misses.
	if _, ok := bs.cache.Get(h); ok {
		t.Fatal("nullCache.Get must always miss; got hit")
	}
}
