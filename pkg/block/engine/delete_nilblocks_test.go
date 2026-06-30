package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/memory"
)

// newReapFixture wires a Store with both a FileChunkStore and a refcount
// coordinator so the delete reap path can resolve a payload's manifest.
// buildCascadeFixture deliberately omits the FileChunkStore; the #1433 fix
// depends on it, so this fixture supplies one.
func newReapFixture(t *testing.T, coord MetadataCoordinator, fbs *stubFileChunkStore) *Store {
	t.Helper()
	localStore := memory.New()
	syncer := NewSyncer(localStore, nil, fbs, DefaultConfig())
	bs, err := New(BlockStoreConfig{
		Local:          localStore,
		Remote:         nil,
		Syncer:         syncer,
		Coordinator:    coord,
		FileChunkStore: fbs,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs
}

// TestDelete_NilBlocks_ReapsPayloadManifest reproduces #1433: the file-removal
// path (NFS REMOVE, SMB delete) calls Delete(payloadID, nil). Before the fix
// the nil slice skipped the coordinator entirely, so block refcounts were
// never decremented — the FileChunk rows survived, the hashes stayed in the GC
// live set, and the chunks were never reclaimed on either tier. With the fix
// Delete enumerates the payload's own manifest and reaps each row.
func TestDelete_NilBlocks_ReapsPayloadManifest(t *testing.T) {
	ctx := context.Background()
	coord := newRefcountCoordinator()
	fbs := newStubFileChunkStore()
	bs := newReapFixture(t, coord, fbs)

	const payloadID = "file-1"
	h0 := hashN(0xA0)
	h1 := hashN(0xB1)

	// Seed the payload's manifest rows and matching refcounts (one referrer
	// each — this file).
	for _, r := range []struct {
		off  uint64
		hash block.ContentHash
	}{{0, h0}, {4096, h1}} {
		if err := fbs.Put(ctx, &block.FileChunk{
			ID:       fmt.Sprintf("%s/%d", payloadID, r.off),
			Hash:     r.hash,
			DataSize: 4096,
		}); err != nil {
			t.Fatalf("seed FileChunk: %v", err)
		}
		coord.seedBlock(payloadID, r.off, r.hash, 1)
	}

	// File-removal path: no manifest carried.
	if err := bs.Delete(ctx, payloadID, nil); err != nil {
		t.Fatalf("Delete(nil blocks): %v", err)
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()
	if c := coord.counts[h0]; c != 0 {
		t.Errorf("h0 refcount = %d, want 0 (row not reaped — #1433 regression)", c)
	}
	if c := coord.counts[h1]; c != 0 {
		t.Errorf("h1 refcount = %d, want 0 (row not reaped — #1433 regression)", c)
	}
}
