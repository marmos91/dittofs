package engine

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Post-#1493 seeding helpers: fixtures that model remote-durable chunks must
// seed them the packed-block way — a blocks/<id> object on the remote plus a
// synced marker carrying the block locator. The legacy standalone form
// (hash-keyed cas/ object + zero locator) is refused by dispatchRemoteFetch as
// post-migration drift.

// putChunkBlock uploads data as a single-chunk packed block object keyed
// blockID on rbs and returns the chunk's block locator. The wire bytes are the
// raw plaintext (no codec framing, no sealing) — exactly what the plain memory
// remote's ReadChunk serves back and what the engine's readChunkVerified
// BLAKE3-verifies against.
func putChunkBlock(t testing.TB, ctx context.Context, rbs remote.RemoteBlockStore, blockID string, data []byte) block.ChunkLocator {
	t.Helper()
	if err := rbs.PutBlock(ctx, blockID, bytes.NewReader(data)); err != nil {
		t.Fatalf("seed PutBlock(%s): %v", blockID, err)
	}
	return block.ChunkLocator{BlockID: blockID, WireOffset: 0, WireLength: int64(len(data))}
}

// seedSyncedRemoteChunk seeds the post-#1493 remote-durable state for one
// chunk: a packed block object holding data, a synced marker carrying the
// block locator, and a FileChunk manifest row at (payloadID, offset). The
// fetch path then resolves the row, resolves the locator, and reads the chunk
// through the remote's ChunkReader. Returns the chunk's content hash.
func seedSyncedRemoteChunk(
	t testing.TB,
	fbs *stubFileChunkStore,
	rbs remote.RemoteBlockStore,
	shs metadata.SyncedHashStore,
	payloadID string,
	offset uint64,
	data []byte,
) block.ContentHash {
	t.Helper()
	ctx := context.Background()
	hash := block.ContentHash(blake3.Sum256(data))
	loc := putChunkBlock(t, ctx, rbs, fmt.Sprintf("blk-%s-%d", payloadID, offset), data)
	if err := shs.MarkSynced(ctx, hash, loc); err != nil {
		t.Fatalf("seed MarkSynced: %v", err)
	}
	fb := &block.FileChunk{
		ID:       fmt.Sprintf("%s/%d", payloadID, offset),
		Hash:     hash,
		DataSize: uint32(len(data)),
		State:    block.BlockStateRemote,
	}
	if err := fbs.Put(ctx, fb); err != nil {
		t.Fatalf("seed FileChunk Put: %v", err)
	}
	return hash
}

// remoteBlockObjectCount returns the number of packed blocks/ objects on the
// memory remote.
func remoteBlockObjectCount(t testing.TB, ctx context.Context, s *remotememory.Store) int {
	t.Helper()
	n := 0
	if err := s.WalkBlocks(ctx, func(string, block.Meta) error { n++; return nil }); err != nil {
		t.Fatalf("WalkBlocks: %v", err)
	}
	return n
}
