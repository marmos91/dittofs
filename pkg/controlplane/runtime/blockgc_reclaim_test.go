package runtime

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// testHash derives a deterministic block.ContentHash from a seed string so the
// reclaim tests can reference the same chunk across two shares.
func testHash(seed string) block.ContentHash {
	var h block.ContentHash
	const fnvPrime = uint64(0x100000001b3)
	state := uint64(0xcbf29ce484222325)
	for _, b := range []byte(seed) {
		state ^= uint64(b)
		state *= fnvPrime
	}
	for i := 0; i < block.HashSize; i++ {
		h[i] = byte(state >> (uint(i%8) * 8))
	}
	return h
}

// seedShareBlock records, in one share's metadata store, a single-chunk packed
// block: the block record (LiveChunkCount=1), the chunk's local location, and
// its synced block-locator backdated past grace. The block bytes are written to
// the shared remote under blockID. Mirrors what a share's carver produces for a
// block-resident chunk. Returns the block length recorded.
func seedShareBlock(t *testing.T, st metadata.Store, rbs remote.RemoteBlockStore, blockID string, h block.ContentHash) int64 {
	t.Helper()
	ctx := context.Background()
	data := []byte("block-bytes-" + blockID)
	if err := rbs.PutBlock(ctx, blockID, bytes.NewReader(data)); err != nil {
		t.Fatalf("PutBlock(%s): %v", blockID, err)
	}
	if err := st.PutBlockRecord(ctx, block.BlockRecord{
		BlockID:        blockID,
		Length:         int64(len(data)),
		LiveChunkCount: 1,
		SyncState:      block.BlockStateRemote,
	}); err != nil {
		t.Fatalf("PutBlockRecord(%s): %v", blockID, err)
	}
	if err := st.PutLocalLocation(ctx, h, block.LocalChunkLocation{LogBlobID: "0000000000000000", RawLength: 64}); err != nil {
		t.Fatalf("PutLocalLocation: %v", err)
	}
	if err := st.MarkSynced(ctx, h, block.ChunkLocator{BlockID: blockID, WireLength: 80}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}
	st.(*metadatamemory.MemoryMetadataStore).MarkSyncedAtForTest(h, time.Now().Add(-2*time.Hour))
	return int64(len(data))
}

// TestUnionBlockReclaimer_ReclaimsEveryShare proves the union reclaimer frees
// the enclosing block in EVERY share that packed a now-dead hash, not just the
// first. Identical content carved by two shares on a shared remote packs into
// two distinct blocks (random per-share block IDs); stopping at the first owner
// would leak the second share's block forever.
func TestUnionBlockReclaimer_ReclaimsEveryShare(t *testing.T) {
	ctx := context.Background()
	stA := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	stB := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	rbs := remotememory.New()
	defer func() { _ = rbs.Close() }()

	h := testHash("shared-dead-chunk")
	lenA := seedShareBlock(t, stA, rbs, "blk-a", h)
	lenB := seedShareBlock(t, stB, rbs, "blk-b", h)

	u := unionBlockReclaimer{
		{Locators: stA, Records: stA, LocalIndex: stA, RemoteBlocks: rbs},
		{Locators: stB, Records: stB, LocalIndex: stB, RemoteBlocks: rbs},
	}

	handled, freed, err := u.ReclaimDeadChunk(ctx, h)
	if err != nil {
		t.Fatalf("ReclaimDeadChunk: %v", err)
	}
	if !handled {
		t.Fatal("handled = false, want true (block-resident in both shares)")
	}
	if want := lenA + lenB; freed != want {
		t.Errorf("bytesFreed = %d, want %d (both blocks freed)", freed, want)
	}

	// Both share block records must be gone.
	if _, ok, _ := stA.GetBlockRecord(ctx, "blk-a"); ok {
		t.Error("share A block record survived — its block leaked")
	}
	if _, ok, _ := stB.GetBlockRecord(ctx, "blk-b"); ok {
		t.Error("share B block record survived — its block leaked (I2 early-return bug)")
	}
	// Both block objects must be gone from the shared remote.
	if _, err := rbs.GetBlock(ctx, "blk-a"); err == nil {
		t.Error("share A block object survived on remote")
	}
	if _, err := rbs.GetBlock(ctx, "blk-b"); err == nil {
		t.Error("share B block object survived on remote — leaked block (I2 early-return bug)")
	}
}

// TestBlockGC_SerializesPerRemote proves the server-wide sweep and the
// per-share sweep never run concurrently against the SAME remote. They use
// different engine gc-state roots, so only the runtime per-remote lock
// serializes them; without it a packed-block reclaim decrement double-counts a
// dead chunk and frees a block a live sibling still needs. The spy sleeps while
// "inside" a remote's sweep and records the peak concurrency observed per
// remote config; the fix caps it at 1.
func TestBlockGC_SerializesPerRemote(t *testing.T) {
	installCollectGarbageLocalSpy(t)

	var mu sync.Mutex
	inFlight := map[string]int{}
	peak := map[string]int{}
	orig := collectGarbageFn
	collectGarbageFn = func(_ context.Context, _ engine.MetadataReconciler, opts *engine.Options) *engine.GCStats {
		cid := opts.RemoteEndpointID
		mu.Lock()
		inFlight[cid]++
		if inFlight[cid] > peak[cid] {
			peak[cid] = inFlight[cid]
		}
		mu.Unlock()
		// Widen the window so any missing serialization deterministically
		// overlaps rather than racing by luck.
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		inFlight[cid]--
		mu.Unlock()
		return &engine.GCStats{}
	}
	t.Cleanup(func() { collectGarbageFn = orig })

	rs := &fakeRemoteStore{name: "s3-shared"}
	rt := newRuntimeForGC(t, map[string]remote.RemoteStore{"/share-a": rs})

	// The server-wide sweep (gc-state root "") and the per-share sweep
	// (gc-state root "<share>/gc-state") both touch the one shared remote.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := rt.RunBlockGC(context.Background(), "", false); err != nil {
			t.Errorf("RunBlockGC: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := rt.RunBlockGCForShare(context.Background(), "/share-a", false); err != nil {
			t.Errorf("RunBlockGCForShare: %v", err)
		}
	}()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for cid, p := range peak {
		if p > 1 {
			t.Errorf("remote %s swept by %d concurrent GC passes; per-remote lock must serialize to 1", cid, p)
		}
	}
}
