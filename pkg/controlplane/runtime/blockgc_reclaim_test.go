package runtime

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	blockgc "github.com/marmos91/dittofs/pkg/block/gc"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
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
		{Locators: stA, Records: stA, RemoteBlocks: rbs},
		{Locators: stB, Records: stB, RemoteBlocks: rbs},
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
	var mu sync.Mutex
	inFlight := map[string]int{}
	peak := map[string]int{}
	orig := collectGarbageFn
	collectGarbageFn = func(_ context.Context, _ blockgc.MetadataReconciler, opts *blockgc.Options) *blockgc.GCStats {
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
		return &blockgc.GCStats{}
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
		if _, err := rt.RunBlockGC(context.Background(), false); err != nil {
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

// TestBlockGCForShare_SharedMetadataStoreFreesOwningRemote runs GC for two
// shares on one metadata store, each on its own remote. First, /cubbit's
// reclaimer is called directly: it must leave the dead block on /export's
// remote for /export's pass. That check does not depend on the pass order, so a
// regression fails it on the first run. Then the real per-share GC must free the
// block, with no error recorded, whichever remote's pass runs first. The pass
// order follows map iteration, so that check repeats on fresh runtimes until
// both orders have almost surely run.
func TestBlockGCForShare_SharedMetadataStoreFreesOwningRemote(t *testing.T) {
	ctx := context.Background()
	for run := range 24 {
		exportRemote := remotememory.New()
		cubbitRemote := remotememory.New()
		rt := newRuntimeForGC(t, map[string]remote.RemoteStore{
			"/export": exportRemote,
			"/cubbit": cubbitRemote,
		})
		mds, err := rt.GetMetadataStoreForShare("/export")
		if err != nil {
			t.Fatalf("GetMetadataStoreForShare: %v", err)
		}
		h := testHash("export-dead-chunk")
		seedShareBlock(t, mds, exportRemote, "blk-export", h)

		reclaimer := rt.blockReclaimerForEntry(remoteEntryForShare(t, rt, "/cubbit"))
		if _, _, err := reclaimer.ReclaimDeadChunk(ctx, h); !errors.Is(err, blockgc.ErrBlockOnOtherRemote) {
			t.Fatalf("run %d: /cubbit's reclaimer: err = %v, want ErrBlockOnOtherRemote", run, err)
		}
		if _, err := exportRemote.GetBlock(ctx, "blk-export"); err != nil {
			t.Fatalf("run %d: /cubbit's reclaimer touched /export's block: %v", run, err)
		}
		if _, synced, _ := mds.GetLocator(ctx, h); !synced {
			t.Fatalf("run %d: /cubbit's reclaimer cleared the marker /export's pass needs", run)
		}

		stats, err := rt.RunBlockGCForShare(ctx, "/export", false)
		if err != nil {
			t.Fatalf("run %d: RunBlockGCForShare: %v", run, err)
		}
		if _, err := exportRemote.GetBlock(ctx, "blk-export"); !errors.Is(err, block.ErrChunkNotFound) {
			t.Fatalf("run %d: dead block still on /export's remote after GC (swept %d, errors %d: %v)",
				run, stats.ObjectsSwept, stats.ErrorCount, stats.FirstErrors)
		}
		if stats.ErrorCount != 0 {
			t.Fatalf("run %d: GC recorded %d errors: %v", run, stats.ErrorCount, stats.FirstErrors)
		}
		_ = exportRemote.Close()
		_ = cubbitRemote.Close()
	}
}

// TestCompactRemoteForEntry_LeavesOtherRemotesRecords runs compaction for
// /cubbit's remote while the metadata store it shares with /export holds a
// compaction candidate on /export's remote: one 80-byte live chunk in a 4 KiB
// block. /cubbit's remote does not hold that block, and compaction must not
// take it for the husk of an interrupted compaction and drop its record.
func TestCompactRemoteForEntry_LeavesOtherRemotesRecords(t *testing.T) {
	ctx := context.Background()
	exportRemote := remotememory.New()
	cubbitRemote := remotememory.New()
	defer func() { _ = exportRemote.Close(); _ = cubbitRemote.Close() }()
	rt := newRuntimeForGC(t, map[string]remote.RemoteStore{
		"/export": exportRemote,
		"/cubbit": cubbitRemote,
	})
	mds, err := rt.GetMetadataStoreForShare("/export")
	if err != nil {
		t.Fatalf("GetMetadataStoreForShare: %v", err)
	}
	data := bytes.Repeat([]byte("x"), 4096)
	if err := exportRemote.PutBlock(ctx, "blk-sparse", bytes.NewReader(data)); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	if err := mds.PutBlockRecord(ctx, block.BlockRecord{
		BlockID:        "blk-sparse",
		Length:         int64(len(data)),
		LiveChunkCount: 1,
		SyncState:      block.BlockStateRemote,
	}); err != nil {
		t.Fatalf("PutBlockRecord: %v", err)
	}
	if err := mds.MarkSynced(ctx, testHash("sparse-live-chunk"), block.ChunkLocator{BlockID: "blk-sparse", WireLength: 80}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	var total blockgc.GCStats
	rt.compactRemoteForEntry(ctx, remoteEntryForShare(t, rt, "/cubbit"), false, &GCDefaults{CompactionLiveRatio: 0.5}, &total)

	if _, ok, err := mds.GetBlockRecord(ctx, "blk-sparse"); err != nil || !ok {
		t.Fatalf("/cubbit's compaction dropped the record of /export's block: ok = %v, err = %v", ok, err)
	}
	if _, err := exportRemote.GetBlock(ctx, "blk-sparse"); err != nil {
		t.Fatalf("/export's block gone after /cubbit's compaction: %v", err)
	}
}

// remoteEntryForShare returns the remote-store entry that serves share.
func remoteEntryForShare(t *testing.T, rt *Runtime, share string) shares.RemoteStoreEntry {
	t.Helper()
	for _, entry := range rt.sharesSvc.DistinctRemoteStores() {
		if slices.Contains(entry.Shares, share) {
			return entry
		}
	}
	t.Fatalf("no remote store serves %s", share)
	return shares.RemoteStoreEntry{}
}
