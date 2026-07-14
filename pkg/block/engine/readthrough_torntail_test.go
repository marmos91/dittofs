package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestReadThrough_TornTail_RecoversFromRemote is the ITEM-1 durability
// regression: a chunk staged into the log-blob by the read-through cache
// (FSStore.Put) is remote-durable, but its local index entry is committed
// WITHOUT fsyncing the blob. A crash in that window leaves a durable index
// entry pointing at un-fsynced blob bytes.
//
// Modelled by staging via Put, marking the chunk synced + remote-durable, then
// truncating the blob file (index survives, bytes lost). A subsequent read must
// RECOVER byte-identical from the authoritative remote copy — not hard-error
// and not silently return zeros.
func TestReadThrough_TornTail_RecoversFromRemote(t *testing.T) {
	ctx := context.Background()

	// One memory metadata store backs the FileChunk manifest, the local chunk
	// index, and the synced-hash store, so the read path and the recovery path
	// agree on a single source of truth (as they do in production).
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()

	localStore, err := fs.NewWithOptions(t.TempDir(), 100*1024*1024, ms, fs.FSStoreOptions{})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}

	remoteRS := &countingRemoteStore{Store: remotememory.New()}
	syncer := NewSyncer(localStore, remoteRS, ms, DefaultConfig())
	bs, err := New(BlockStoreConfig{
		Local:           localStore,
		Remote:          remoteRS,
		Syncer:          syncer,
		FileChunkStore:  ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(ctx); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	data := bytes.Repeat([]byte("read-through-recovery!"), 512) // ~11 KiB
	hash := block.ContentHash(blake3.Sum256(data))
	payloadID := "p-recover"

	// Seed the remote with the authoritative copy as a packed block and mark
	// the chunk synced with the block locator — exactly the remote-durable
	// state the carver commits (post-#1493 every synced chunk is
	// block-resident).
	loc := putChunkBlock(t, ctx, remoteRS, "blk-recover", data)
	if err := ms.MarkSynced(ctx, hash, loc); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	// Stage locally via Put (the read-through cache's write entry): appends to
	// the log-blob and commits a durable index entry.
	if err := localStore.Put(ctx, hash, data); err != nil {
		t.Fatalf("local Put (stage): %v", err)
	}

	// Manifest row so the read path can resolve (payloadID, offset 0) -> hash.
	fb := &block.FileChunk{
		ID:       payloadID + "/0",
		Hash:     hash,
		DataSize: uint32(len(data)),
		State:    block.BlockStatePending,
	}
	if err := ms.Put(ctx, fb); err != nil {
		t.Fatalf("FileChunk Put: %v", err)
	}

	// Sanity: reads correctly before the tear.
	pre := make([]byte, len(data))
	if n, err := bs.readAtInternal(ctx, payloadID, pre, 0); err != nil || n != len(data) || !bytes.Equal(pre, data) {
		t.Fatalf("pre-tear read: n=%d err=%v equal=%v", n, err, bytes.Equal(pre, data))
	}

	// Simulate the crash: truncate the active blob so the staged bytes are
	// gone, while the durable index entry survives.
	truncateAllBlobs(t, localStore)

	// The read must recover byte-identical from the remote copy.
	dest := make([]byte, len(data))
	n, err := bs.readAtInternal(ctx, payloadID, dest, 0)
	if err != nil {
		t.Fatalf("post-tear read hard-errored (availability bug): %v", err)
	}
	if n != len(data) {
		t.Fatalf("post-tear read n=%d, want %d", n, len(data))
	}
	if !bytes.Equal(dest, data) {
		t.Fatalf("post-tear read not byte-identical to remote copy (zeros/garbage?): recovered %d bytes", len(dest))
	}
	if got := remoteRS.readChunkCount.Load(); got == 0 {
		t.Fatal("recovery did not consult the remote store — read did not self-heal from remote")
	}
}

// truncateAllBlobs zeroes every .blob file under the fs store's blobs directory,
// modelling a torn tail where appended-but-un-fsynced bytes are lost on crash.
func truncateAllBlobs(t *testing.T, localStore *fs.FSStore) {
	t.Helper()
	blobsDir := filepath.Join(localStore.BaseDirForTest(), "blobs")
	entries, err := os.ReadDir(blobsDir)
	if err != nil {
		t.Fatalf("read blobs dir: %v", err)
	}
	truncated := false
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".blob" {
			if err := os.Truncate(filepath.Join(blobsDir, e.Name()), 0); err != nil {
				t.Fatalf("truncate blob %s: %v", e.Name(), err)
			}
			truncated = true
		}
	}
	if !truncated {
		t.Fatal("no .blob file found to truncate")
	}
}
