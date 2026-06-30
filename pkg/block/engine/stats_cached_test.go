package engine

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	memorylocal "github.com/marmos91/dittofs/pkg/block/local/memory"
	"lukechampine.com/blake3"
)

// TestPopulateBlockCounts_ClassifiesByPhysicalPresence pins the #1362 stats
// fix: a block whose FileChunk row still reads BlockStateRemote but whose CAS
// chunk is physically on local disk (the read-through-cache case) must count as
// local — and additionally as cached — rather than remote. Before the fix,
// classification went purely by sync state, so a fully read-cached share
// reported "Blocks Local: 0" while du showed the cache was full.
func TestPopulateBlockCounts_ClassifiesByPhysicalPresence(t *testing.T) {
	ctx := context.Background()
	loc := memorylocal.New()
	fbs := newStubFileChunkStore()
	bs := &Store{local: loc, fileChunkStore: fbs}

	const payloadID = "p1"

	// Block A: row says remote, but the chunk is physically present locally
	// (fetched and cached). Expect local + cached.
	dataA := []byte("aaaa-read-cached-block-present-on-disk")
	hA := block.ContentHash(blake3.Sum256(dataA))
	if err := loc.Put(ctx, hA, dataA); err != nil {
		t.Fatalf("local Put A: %v", err)
	}
	mustPutFB(t, fbs, &block.FileChunk{ID: payloadID + "/0", Hash: hA, DataSize: uint32(len(dataA)), State: block.BlockStateRemote})

	// Block B: row says remote and the chunk is absent locally. Expect remote.
	dataB := []byte("bbbb-remote-only-not-on-disk")
	hB := block.ContentHash(blake3.Sum256(dataB))
	mustPutFB(t, fbs, &block.FileChunk{ID: payloadID + "/1", Hash: hB, DataSize: uint32(len(dataB)), State: block.BlockStateRemote})

	// Block C: dirty/in-flight (zero hash, rollup incomplete). Expect dirty.
	mustPutFB(t, fbs, &block.FileChunk{ID: payloadID + "/2", Hash: block.ContentHash{}, DataSize: 4, State: block.BlockStatePending})

	var stats BlockStoreStats
	bs.populateBlockCounts(&stats)

	if stats.BlocksTotal != 3 {
		t.Errorf("BlocksTotal=%d, want 3", stats.BlocksTotal)
	}
	if stats.BlocksLocal != 1 {
		t.Errorf("BlocksLocal=%d, want 1 (read-cached chunk is physically local)", stats.BlocksLocal)
	}
	if stats.BlocksCached != 1 {
		t.Errorf("BlocksCached=%d, want 1 (locally present but row says remote)", stats.BlocksCached)
	}
	if stats.BlocksRemote != 1 {
		t.Errorf("BlocksRemote=%d, want 1 (remote-only block)", stats.BlocksRemote)
	}
	if stats.BlocksDirty != 1 {
		t.Errorf("BlocksDirty=%d, want 1 (zero-hash in-flight block)", stats.BlocksDirty)
	}
}

func mustPutFB(t *testing.T, fbs *stubFileChunkStore, fb *block.FileChunk) {
	t.Helper()
	if err := fbs.Put(context.Background(), fb); err != nil {
		t.Fatalf("stub FileChunk Put: %v", err)
	}
}
