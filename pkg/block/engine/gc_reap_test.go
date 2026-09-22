package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	blockgc "github.com/marmos91/dittofs/pkg/block/gc"
	"github.com/marmos91/dittofs/pkg/block/local/memory"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// ---------------------------------------------------------------------------
// GC fixtures. These mirror pkg/block/gc's own test fixtures: the tests below
// drive a real engine.Store into the manifest state the GC mark phase reads,
// which neither package can set up alone.
// ---------------------------------------------------------------------------

type gcMSReconciler struct {
	stores map[string]metadata.Store
	order  []string
}

func newGCMSReconciler() *gcMSReconciler {
	return &gcMSReconciler{stores: make(map[string]metadata.Store)}
}

func (r *gcMSReconciler) addShare(name string) metadata.Store {
	st := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	r.stores[name] = st
	r.order = append(r.order, name)
	return st
}

func (r *gcMSReconciler) GetMetadataStoreForShare(name string) (metadata.Store, error) {
	s, ok := r.stores[name]
	if !ok {
		return nil, fmt.Errorf("share %q not found", name)
	}
	return s, nil
}

func (r *gcMSReconciler) SharesForGC() []string { return append([]string(nil), r.order...) }

// putPendingBlock seeds a FileChunk in BlockStatePending — the exact shape the
// engine rollup creates and never transitions to Remote. RefCount 0 (the rollup
// never bumps it; cross-file keep-alive comes from sibling rows in the GC live
// set, not RefCount). The Remote-gated GetByHash returns nil for these, which is
// why the reap path resolves rows by EXACT ID, never by hash. Used by the
// regression tests that exercise the real reap path.
func putPendingBlock(t *testing.T, st metadata.Store, id string, h block.ContentHash) {
	t.Helper()
	if err := st.Put(t.Context(), &block.FileChunk{
		ID:         id,
		Hash:       h,
		State:      block.BlockStatePending,
		DataSize:   64,
		RefCount:   0,
		LastAccess: time.Now(),
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("PutFileChunk(%s): %v", id, err)
	}
}

// putBlock seeds a FileChunk with a non-zero hash on the given metadata store.
func putBlock(t *testing.T, st metadata.Store, id string, h block.ContentHash) {
	t.Helper()
	if err := st.Put(t.Context(), &block.FileChunk{
		ID:         id,
		Hash:       h,
		State:      block.BlockStateRemote,
		DataSize:   64,
		RefCount:   1,
		LastAccess: time.Now(),
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("PutFileChunk(%s): %v", id, err)
	}
}

// hashFromString fans the seed into a 32-byte ContentHash via a simple
// FNV-style mix so similar seeds produce dispersed hashes (otherwise
// "seed-N" all share the same first byte).
func hashFromString(seed string) block.ContentHash {
	var h block.ContentHash
	src := []byte(seed)
	const fnvPrime = uint64(0x100000001b3)
	state := uint64(0xcbf29ce484222325)
	for _, b := range src {
		state ^= uint64(b)
		state *= fnvPrime
	}
	for i := 0; i < block.HashSize; i++ {
		h[i] = byte(state >> (i % 8 * 8))
		state ^= uint64(i+1) * fnvPrime
		state = state*fnvPrime ^ uint64(i)
	}
	return h
}

// seedRemoteChunk packs h into its own single-chunk packed block on rbs and
// records the block record + local location + backdated (past-grace) synced
// marker in st — the block-locator shape of "this chunk is on remote". Returns
// the block object's length: the bytes a sweep frees when it reclaims the
// chunk.
func seedRemoteChunk(t *testing.T, st metadata.Store, rbs remote.RemoteBlockStore, h block.ContentHash) int64 {
	t.Helper()
	blockID := "blk-" + h.String()[:16]
	seedPackedBlock(t, st, rbs, blockID, []block.ContentHash{h})
	rec, ok, err := st.GetBlockRecord(t.Context(), blockID)
	if err != nil || !ok {
		t.Fatalf("GetBlockRecord(%s): ok=%v err=%v", blockID, ok, err)
	}
	return rec.Length
}

// chunkOnRemote reports whether h is still remote-reachable: its
// synced marker resolves to a block locator whose block record still exists.
func chunkOnRemote(t *testing.T, st metadata.Store, h block.ContentHash) bool {
	t.Helper()
	ctx := t.Context()
	loc, ok, err := st.GetLocator(ctx, h)
	if err != nil {
		t.Fatalf("GetLocator(%s): %v", h, err)
	}
	if !ok || loc.BlockID == "" {
		return false
	}
	_, ok, err = st.GetBlockRecord(ctx, loc.BlockID)
	if err != nil {
		t.Fatalf("GetBlockRecord(%s): %v", loc.BlockID, err)
	}
	return ok
}

// collectGarbageBlocks runs the block-keyed remote sweep over a single-share
// fixture: orphan candidates come from st's synced-hash index and reclamation
// goes through a per-share gc.BlockGCReclaimer bound to rbs. opts may carry any
// other knob (DryRun, GracePeriod, HoldProvider, ...).
func collectGarbageBlocks(t *testing.T, rec blockgc.MetadataReconciler, st metadata.Store, rbs remote.RemoteBlockStore, opts *blockgc.Options) *blockgc.GCStats {
	t.Helper()
	if opts == nil {
		opts = &blockgc.Options{}
	}
	idx, ok := st.(blockgc.SyncedHashIndex)
	if !ok {
		t.Fatalf("metadata store %T does not implement blockgc.SyncedHashIndex", st)
	}
	opts.SyncedHashIndex = idx
	opts.BlockReclaimer = newBlockGCReclaimer(st, rbs)
	return blockgc.CollectGarbage(t.Context(), rec, opts)
}

// seedPackedBlock writes a block object to rbs and records, in st, the block
// record (LiveChunkCount = len(chunks)) and each chunk's synced block-locator
// backdated past the grace window. It mirrors what the carver's
// DefaultCommitBlock produces, minus the real codec framing the GC reclaim path
// never inspects.
func seedPackedBlock(t *testing.T, st metadata.Store, rbs remote.RemoteBlockStore, blockID string, chunks []block.ContentHash) {
	t.Helper()
	ctx := t.Context()
	data := []byte("block-bytes-" + blockID)
	if err := rbs.PutBlock(ctx, blockID, bytes.NewReader(data)); err != nil {
		t.Fatalf("PutBlock(%s): %v", blockID, err)
	}
	if err := st.PutBlockRecord(ctx, block.BlockRecord{
		BlockID:        blockID,
		Length:         int64(len(data)),
		LiveChunkCount: uint32(len(chunks)),
		SyncState:      block.BlockStateRemote,
	}); err != nil {
		t.Fatalf("PutBlockRecord(%s): %v", blockID, err)
	}
	for i, h := range chunks {
		if err := st.MarkSynced(ctx, h, block.ChunkLocator{BlockID: blockID, WireOffset: int64(i) * 80, WireLength: 80}); err != nil {
			t.Fatalf("MarkSynced(%s): %v", h, err)
		}
		// Backdate past grace so the steady-state index sweep treats it as
		// eligible (the live-set check is then the only thing that can save it).
		st.(*metadatamemory.MemoryMetadataStore).MarkSyncedAtForTest(h, time.Now().Add(-2*time.Hour))
	}
}

func newBlockGCReclaimer(st metadata.Store, rbs remote.RemoteBlockStore) *blockgc.BlockGCReclaimer {
	return &blockgc.BlockGCReclaimer{Locators: st, Records: st, RemoteBlocks: rbs}
}

// reapCoordinator is the GC-reclaim test's MetadataCoordinator: it binds the
// engine's refcount surface to a real metadata.Store exactly like the
// production runtime coordinator. Increment/Decrement are hash-keyed (CopyPayload
// bookkeeping); the reap path is BY EXACT ID "{payloadID}/{offset}" — never
// hash-resolved — so it removes THIS file's own row unambiguously. Only the
// refcount methods are exercised by Truncate/Delete; the rest are no-ops.
type reapCoordinator struct {
	store metadata.Store
}

func (c *reapCoordinator) IncrementRefCount(ctx context.Context, hash block.ContentHash) error {
	fb, err := c.store.GetByHash(ctx, hash)
	if err != nil || fb == nil {
		return err
	}
	return c.store.IncrementRefCount(ctx, fb.ID)
}

func (c *reapCoordinator) DecrementRefCountAndReap(ctx context.Context, payloadID string, offset uint64) (uint32, error) {
	// Mirrors the production coordinator: reap by EXACT ID — no hash resolution.
	// The engine rollup creates per-chunk rows keyed "{payloadID}/{offset}" in
	// Pending and never finalizes them, so this works whatever the row's state,
	// and removing this file's own row by ID can never touch another file's row.
	id := fmt.Sprintf("%s/%d", payloadID, offset)
	count, err := c.store.DecrementRefCountAndReap(ctx, id)
	if err != nil {
		if errors.Is(err, metadata.ErrFileChunkNotFound) {
			return 0, nil
		}
		return 0, err
	}
	return count, nil
}

func (c *reapCoordinator) PersistFileChunks(_ context.Context, _ string, _ []block.ChunkRef, _ block.ObjectID) error {
	return nil
}

func (c *reapCoordinator) GetPersistedBlocks(_ context.Context, _ string) ([]block.ChunkRef, error) {
	return nil, nil
}

func (c *reapCoordinator) FindByObjectID(_ context.Context, _ block.ObjectID) ([]block.ChunkRef, error) {
	return nil, nil
}

// ReprojectBlocks is a no-op: this fake does not model the Blocks
// projection.
func (c *reapCoordinator) ReprojectBlocks(_ context.Context, _ string) error { return nil }

func (c *reapCoordinator) GetFileObjectID(_ context.Context, _ string) (block.ObjectID, error) {
	return block.ObjectID{}, nil
}

var _ MetadataCoordinator = (*reapCoordinator)(nil)

// newReapEngine builds an engine.Store whose coordinator reaps RefCount-0
// FileChunk rows from the supplied metadata store, so a Truncate/Delete that
// drops a hash's last reference removes it from EnumerateFileChunks and the GC
// sweep can reclaim the remote chunk. The engine's own local store / syncer are
// memory-only (no remote) — the GC sweep runs directly against the test's
// separate remote store via gc.CollectGarbage.
func newReapEngine(t *testing.T, st metadata.Store) *Store {
	t.Helper()
	localStore := memory.New()
	fbs := newStubFileChunkStore()
	syncer := NewRemoteSync(localStore, nil, fbs, DefaultConfig())
	bs, err := New(BlockStoreConfig{
		Local:          localStore,
		Remote:         nil,
		RemoteSync:     syncer,
		FileChunkStore: fbs,
		Coordinator:    &reapCoordinator{store: st},
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs
}

// TestGCMarkSweep_TruncateReclaimsRemoteChunk: a Truncate that drops a
// tail block's LAST reference must reap its FileChunk index row so the hash
// leaves the GC live set and the sweep reclaims the remote chunk. The retained
// block's chunk survives. This test FAILS on develop — where Truncate only
// decremented RefCount (leaving the row at RefCount 0 but still emitted by
// EnumerateFileChunks), so the dropped chunk stayed in the live set forever.
func TestGCMarkSweep_TruncateReclaimsRemoteChunk(t *testing.T) {
	ctx := t.Context()
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-a")
	bs := newReapEngine(t, st)

	const mib = uint64(1 << 20)
	h1 := hashFromString("trunc-keep-h1")
	h2 := hashFromString("trunc-drop-h2")

	// Two CAS objects: H1 @ offset 0 (kept), H2 @ offset 4MiB (dropped). Both
	// have FileChunk index rows keyed by EXACT "{payloadID}/{offset}" (the shape
	// the engine rollup produces) and live CAS objects on remote.
	putBlock(t, st, "file-trunc/0", h1)
	putBlock(t, st, fmt.Sprintf("file-trunc/%d", 4*mib), h2)
	seedRemoteChunk(t, st, rs, h1)
	seedRemoteChunk(t, st, rs, h2)

	// Truncate to 4MiB: H2 (offset 4MiB) is dropped, H1 (offset 0) kept.
	blocks := []block.ChunkRef{
		{Hash: h1, Offset: 0, Size: uint32(mib)},
		{Hash: h2, Offset: 4 * mib, Size: uint32(mib)},
	}
	if _, err := bs.Truncate(ctx, "file-trunc", blocks, 4*mib); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	stats := collectGarbageBlocks(t, rec, st, rs, &blockgc.Options{
		GCStateRoot: t.TempDir(),
		GracePeriod: time.Minute,
	})
	if stats.ErrorCount != 0 {
		t.Fatalf("ErrorCount = %d, want 0; FirstErrors=%v", stats.ErrorCount, stats.FirstErrors)
	}

	// H2's chunk MUST be swept (its row was reaped → left the live set).
	if chunkOnRemote(t, st, h2) {
		t.Errorf("dropped chunk H2 still present on remote after Truncate+GC; want swept (reap leaked the row)")
	}
	// H1's chunk MUST survive (still referenced).
	if !chunkOnRemote(t, st, h1) {
		t.Errorf("retained chunk H1 swept after Truncate+GC; want retained")
	}
	if stats.ObjectsSwept != 1 {
		t.Errorf("ObjectsSwept = %d, want 1 (only the dropped H2)", stats.ObjectsSwept)
	}
}

// TestGCMarkSweep_TruncateDedupSafety (data-loss guard, by-ID model): two
// files reference the same content hash, each via its OWN per-offset row
// (file-A/<off> and file-B/<off>). Truncating file-A reaps file-A's own row by
// EXACT ID; file-B's SIBLING row keeps the hash in EnumerateFileChunks (the GC
// live set), so the sweep must NOT reclaim the chunk. Keep-alive is provided by
// the sibling row, not by RefCount.
func TestGCMarkSweep_TruncateDedupSafety(t *testing.T) {
	ctx := t.Context()
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-a")
	bs := newReapEngine(t, st)

	const mib = uint64(1 << 20)
	shared := hashFromString("dedup-shared-hash")

	// Two independent rows for the shared hash: one per file. One CAS object.
	putBlock(t, st, fmt.Sprintf("file-A/%d", 4*mib), shared)
	putBlock(t, st, "file-B/0", shared)
	seedRemoteChunk(t, st, rs, shared)

	// Truncate file-A dropping its block: file-A's own row (file-A/4MiB) is
	// reaped by ID; file-B's sibling row remains.
	blocks := []block.ChunkRef{{Hash: shared, Offset: 4 * mib, Size: uint32(mib)}}
	if _, err := bs.Truncate(ctx, "file-A", blocks, 0); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	// file-B's sibling row must keep the hash in the live set.
	if !hashInLiveSet(t, ctx, st, shared) {
		t.Fatalf("shared hash left EnumerateFileChunks after truncating ONE of two files; data-loss — sibling row not keeping it alive")
	}

	stats := collectGarbageBlocks(t, rec, st, rs, &blockgc.Options{
		GCStateRoot: t.TempDir(),
		GracePeriod: time.Minute,
	})
	if stats.ErrorCount != 0 {
		t.Fatalf("ErrorCount = %d, want 0; FirstErrors=%v", stats.ErrorCount, stats.FirstErrors)
	}

	// The shared chunk MUST survive: file-B's sibling row still references it.
	if !chunkOnRemote(t, st, shared) {
		t.Errorf("shared chunk swept after truncating ONE of two referencing files; want retained (sibling row keeps it live)")
	}
	if stats.ObjectsSwept != 0 {
		t.Errorf("ObjectsSwept = %d, want 0 (shared chunk still referenced)", stats.ObjectsSwept)
	}
}

// TestGCMarkSweep_DeleteDuplicateHashNoOverReap (data-loss guard, by-ID
// model): file-A references the SAME hash at TWO offsets (two rows), and file-B
// references it via its own sibling row. Deleting file-A reaps BOTH of file-A's
// rows by exact ID; file-B's sibling row keeps the hash in the GC live set, so
// the chunk must survive. The two file-A rows are independent — each must be
// reaped, but neither can touch file-B's row.
func TestGCMarkSweep_DeleteDuplicateHashNoOverReap(t *testing.T) {
	ctx := t.Context()
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-a")
	bs := newReapEngine(t, st)

	const mib = uint64(1 << 20)
	shared := hashFromString("dup-and-shared-hash")

	// file-A holds the hash at two offsets (two rows); file-B holds it once.
	putBlock(t, st, "file-A/0", shared)
	putBlock(t, st, fmt.Sprintf("file-A/%d", 4*mib), shared)
	putBlock(t, st, "file-B/0", shared)
	seedRemoteChunk(t, st, rs, shared)

	// Delete file-A: both its rows (offsets 0 and 4MiB) are reaped by ID.
	dupBlocks := []block.ChunkRef{
		{Hash: shared, Offset: 0, Size: uint32(mib)},
		{Hash: shared, Offset: 4 * mib, Size: uint32(mib)},
	}
	if err := bs.Delete(ctx, "file-A", dupBlocks); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Both file-A rows gone, but file-B's sibling row keeps the hash live.
	if !hashInLiveSet(t, ctx, st, shared) {
		t.Fatalf("shared hash left EnumerateFileChunks after deleting file-A; data-loss — file-B sibling row not keeping it alive")
	}

	stats := collectGarbageBlocks(t, rec, st, rs, &blockgc.Options{GCStateRoot: t.TempDir(), GracePeriod: time.Minute})
	if stats.ErrorCount != 0 {
		t.Fatalf("ErrorCount = %d; FirstErrors=%v", stats.ErrorCount, stats.FirstErrors)
	}
	if !chunkOnRemote(t, st, shared) {
		t.Errorf("shared chunk swept after deleting file-A; want retained (file-B sibling row still refs it)")
	}
	if stats.ObjectsSwept != 0 {
		t.Errorf("ObjectsSwept = %d, want 0 (shared chunk still referenced by file-B)", stats.ObjectsSwept)
	}
}

// TestGCMarkSweep_TruncateStraddleHashNoOverReap (data-loss guard, by-ID
// model): the same hash sits on BOTH sides of newSize within ONE file, each at
// its own offset (its own row). Truncate reaps only the DROPPED row (file-S/4MiB)
// by exact ID; the KEPT row (file-S/0) is a different ID and survives, keeping
// the hash in the GC live set, so the chunk must NOT be swept. Reaping by ID
// cannot touch the kept row because their IDs differ.
func TestGCMarkSweep_TruncateStraddleHashNoOverReap(t *testing.T) {
	ctx := t.Context()
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-a")
	bs := newReapEngine(t, st)

	const mib = uint64(1 << 20)
	shared := hashFromString("straddle-hash")

	// Two rows in one file for the same hash: offset 0 (kept) and 4MiB (dropped).
	putBlock(t, st, "file-S/0", shared)
	putBlock(t, st, fmt.Sprintf("file-S/%d", 4*mib), shared)
	seedRemoteChunk(t, st, rs, shared)

	// Same hash kept (offset 0) and dropped (offset 4 MiB). Truncate to 1 MiB.
	blocks := []block.ChunkRef{
		{Hash: shared, Offset: 0, Size: uint32(mib)},
		{Hash: shared, Offset: 4 * mib, Size: uint32(mib)},
	}
	if _, err := bs.Truncate(ctx, "file-S", blocks, mib); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	// The kept row (file-S/0) keeps the hash live.
	if !hashInLiveSet(t, ctx, st, shared) {
		t.Fatalf("straddling hash left EnumerateFileChunks after truncate; data-loss — kept row was over-reaped")
	}

	stats := collectGarbageBlocks(t, rec, st, rs, &blockgc.Options{GCStateRoot: t.TempDir(), GracePeriod: time.Minute})
	if stats.ErrorCount != 0 {
		t.Fatalf("ErrorCount = %d; FirstErrors=%v", stats.ErrorCount, stats.FirstErrors)
	}
	if !chunkOnRemote(t, st, shared) {
		t.Errorf("straddling chunk swept after truncate; want retained (still referenced below newSize)")
	}
	if stats.ObjectsSwept != 0 {
		t.Errorf("ObjectsSwept = %d, want 0 (hash still kept below newSize)", stats.ObjectsSwept)
	}
}

// TestGCMarkSweep_PendingReclaimsRemoteChunk (the real-world gap): the
// engine rollup creates per-chunk FileChunk rows in BlockStatePending and never
// transitions them to Remote. On develop, Delete/Truncate routed the reap
// through the Remote-gated GetByHash, which returns nil for a Pending row — so
// the reap was a no-op: the row stayed in EnumerateFileChunks and the remote
// chunk leaked forever. The fix reaps by EXACT ID "{payloadID}/{offset}", which
// resolves the row whatever its state.
//
// This test seeds Pending rows (NOT pre-finalized Remote rows like the other
// GC tests, which is why they could not catch this) and asserts that, after
// dropping the block, its row leaves the GC live set AND the sweep reclaims the
// remote chunk.
func TestGCMarkSweep_PendingReclaimsRemoteChunk(t *testing.T) {
	ctx := t.Context()
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-a")
	bs := newReapEngine(t, st)

	const mib = uint64(1 << 20)
	h1 := hashFromString("pending-keep-h1")
	h2 := hashFromString("pending-drop-h2")

	// Pending rows (the rollup never finalizes them). GetByHash returns nil for
	// these; the reap path resolves them by EXACT ID "{payloadID}/{offset}".
	putPendingBlock(t, st, "file-pend/0", h1)
	putPendingBlock(t, st, "file-pend/1048576", h2)
	seedRemoteChunk(t, st, rs, h1)
	seedRemoteChunk(t, st, rs, h2)

	// Sanity: GetByHash (Remote-gated) cannot see the Pending rows.
	if fb, _ := st.GetByHash(ctx, h2); fb != nil {
		t.Fatalf("GetByHash resolved a Pending row; the leak this test guards cannot occur — fixture wrong")
	}

	// Truncate to 1MiB: H2 (offset 1MiB) dropped, H1 (offset 0) kept.
	blocks := []block.ChunkRef{
		{Hash: h1, Offset: 0, Size: uint32(mib)},
		{Hash: h2, Offset: mib, Size: uint32(mib)},
	}
	if _, err := bs.Truncate(ctx, "file-pend", blocks, mib); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	// H2's Pending row (file-pend/1048576) must have been reaped by ID → its
	// hash leaves EnumerateFileChunks (no sibling row references it).
	if hashInLiveSet(t, ctx, st, h2) {
		t.Errorf("dropped Pending hash H2 still in EnumerateFileChunks after reap; want gone (the reap was a no-op)")
	}
	if !hashInLiveSet(t, ctx, st, h1) {
		t.Errorf("retained hash H1 missing from EnumerateFileChunks; want present")
	}

	stats := collectGarbageBlocks(t, rec, st, rs, &blockgc.Options{GCStateRoot: t.TempDir(), GracePeriod: time.Minute})
	if stats.ErrorCount != 0 {
		t.Fatalf("ErrorCount = %d, want 0; FirstErrors=%v", stats.ErrorCount, stats.FirstErrors)
	}
	if chunkOnRemote(t, st, h2) {
		t.Errorf("dropped chunk H2 still remote-reachable after Truncate+GC; want swept (reap leaked the row)")
	}
	if !chunkOnRemote(t, st, h1) {
		t.Errorf("retained chunk H1 swept; want retained")
	}
	if stats.ObjectsSwept != 1 {
		t.Errorf("ObjectsSwept = %d, want 1 (only the dropped H2)", stats.ObjectsSwept)
	}
}

// TestGCMarkSweep_CrossFileDedupKeepAlive is the mandated characterization
// test for the by-ID model: it proves keep-alive is provided by a SIBLING ROW,
// NOT by RefCount. File A and file B each own an independent FileChunk row for
// the same content hash H (file-A/0 and file-B/0). Deleting file A reaps file
// A's OWN row by exact ID; file B's sibling row keeps H in EnumerateFileChunks
// (the GC live set), so the chunk must NOT be swept. This is the data-loss
// safety proof: removing one file's row by ID cannot strand a chunk another
// file still references, because GC sweeps only when NO row anywhere carries H.
func TestGCMarkSweep_CrossFileDedupKeepAlive(t *testing.T) {
	ctx := t.Context()
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-a")
	bs := newReapEngine(t, st)

	const mib = uint64(1 << 20)
	shared := hashFromString("xfile-dedup-keepalive")

	// File A and file B each hold their OWN row for the shared hash. Cross-file
	// keep-alive is the sibling row, not a shared RefCount on one row.
	putBlock(t, st, "file-A/0", shared)
	putBlock(t, st, "file-B/0", shared)
	seedRemoteChunk(t, st, rs, shared)

	// Delete file A: the reap removes file-A's OWN row by ID (file-A/0); file-B's
	// sibling row keeps the hash in the GC live set, so the chunk is NOT swept.
	if err := bs.Delete(ctx, "file-A", []block.ChunkRef{{Hash: shared, Offset: 0, Size: uint32(mib)}}); err != nil {
		t.Fatalf("Delete file-A: %v", err)
	}

	// The shared hash must survive in the live set via file-B's sibling row.
	if !hashInLiveSet(t, ctx, st, shared) {
		t.Fatalf("shared hash left EnumerateFileChunks after deleting one of two referencing files; data-loss — reap removed a hash a sibling file still references")
	}

	stats := collectGarbageBlocks(t, rec, st, rs, &blockgc.Options{GCStateRoot: t.TempDir(), GracePeriod: time.Minute})
	if stats.ErrorCount != 0 {
		t.Fatalf("ErrorCount = %d; FirstErrors=%v", stats.ErrorCount, stats.FirstErrors)
	}
	if !chunkOnRemote(t, st, shared) {
		t.Errorf("shared chunk swept after deleting ONE of two referencing files; want retained (file-B still refs it)")
	}
	if stats.ObjectsSwept != 0 {
		t.Errorf("ObjectsSwept = %d, want 0 (shared chunk still referenced by file-B)", stats.ObjectsSwept)
	}

	// Second phase: delete file B too. Now NO row anywhere carries H, so the
	// hash leaves the live set and the next sweep reclaims the chunk. This
	// completes the keep-alive proof — the chunk dies only when the LAST
	// referencing row is reaped.
	if err := bs.Delete(ctx, "file-B", []block.ChunkRef{{Hash: shared, Offset: 0, Size: uint32(mib)}}); err != nil {
		t.Fatalf("Delete file-B: %v", err)
	}
	if hashInLiveSet(t, ctx, st, shared) {
		t.Fatalf("shared hash still in EnumerateFileChunks after deleting BOTH files; want gone (last row reaped)")
	}
	stats2 := collectGarbageBlocks(t, rec, st, rs, &blockgc.Options{GCStateRoot: t.TempDir(), GracePeriod: time.Minute})
	if stats2.ErrorCount != 0 {
		t.Fatalf("phase-2 ErrorCount = %d; FirstErrors=%v", stats2.ErrorCount, stats2.FirstErrors)
	}
	if chunkOnRemote(t, st, shared) {
		t.Errorf("shared chunk still remote-reachable after deleting BOTH referencing files; want swept (no row references it)")
	}
	if stats2.ObjectsSwept != 1 {
		t.Errorf("phase-2 ObjectsSwept = %d, want 1 (the now-unreferenced chunk)", stats2.ObjectsSwept)
	}
}

// TestGCMarkSweep_SameHashTwoOffsetsBothReaped (by-ID regression): one
// file holds IDENTICAL content at TWO offsets — TWO independent FileChunk rows
// keyed file-X/0 and file-X/<off>, both carrying the same hash H. Deleting the
// file must reap BOTH rows so H leaves EnumerateFileChunks and the chunk is
// swept. This is the exact edge the prior by-hash reap leaked: resolving by hash
// reaped only ONE row (an indeterminate one), leaving the other row stranded —
// the hash stayed live forever and the chunk never reclaimed. By-ID reap removes
// each offset's row independently, so both go. Rows are Pending (the rollup
// shape) to exercise the realistic path.
func TestGCMarkSweep_SameHashTwoOffsetsBothReaped(t *testing.T) {
	ctx := t.Context()
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-a")
	bs := newReapEngine(t, st)

	const mib = uint64(1 << 20)
	dup := hashFromString("same-hash-two-offsets")

	// One file, same hash at offset 0 and offset 1MiB: two distinct rows.
	id0 := "file-dup/0"
	id1 := fmt.Sprintf("file-dup/%d", mib)
	putPendingBlock(t, st, id0, dup)
	putPendingBlock(t, st, id1, dup)
	seedRemoteChunk(t, st, rs, dup)

	// Both rows exist before the delete.
	if fb, _ := st.GetFileChunk(ctx, id0); fb == nil {
		t.Fatalf("fixture: row %s missing before delete", id0)
	}
	if fb, _ := st.GetFileChunk(ctx, id1); fb == nil {
		t.Fatalf("fixture: row %s missing before delete", id1)
	}

	// Delete the file: its block list carries the SAME hash at both offsets.
	if err := bs.Delete(ctx, "file-dup", []block.ChunkRef{
		{Hash: dup, Offset: 0, Size: uint32(mib)},
		{Hash: dup, Offset: mib, Size: uint32(mib)},
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// BOTH rows must be gone (the by-hash approach left one stranded).
	if fb, _ := st.GetFileChunk(ctx, id0); fb != nil {
		t.Errorf("row %s survived delete; want reaped", id0)
	}
	if fb, _ := st.GetFileChunk(ctx, id1); fb != nil {
		t.Errorf("row %s survived delete (the by-hash leak); want reaped", id1)
	}
	if hashInLiveSet(t, ctx, st, dup) {
		t.Fatalf("dup hash still in EnumerateFileChunks after deleting both rows; want gone (the by-hash reap stranded a row)")
	}

	// Resolve the packed block the chunk lives in BEFORE the sweep: reclaiming
	// the chunk drops its locator, so afterwards the blockID is unrecoverable.
	loc, ok, err := st.GetLocator(ctx, dup)
	if err != nil || !ok {
		t.Fatalf("GetLocator(dup) before sweep: ok=%v err=%v", ok, err)
	}

	stats := collectGarbageBlocks(t, rec, st, rs, &blockgc.Options{GCStateRoot: t.TempDir(), GracePeriod: time.Minute})
	if stats.ErrorCount != 0 {
		t.Fatalf("ErrorCount = %d; FirstErrors=%v", stats.ErrorCount, stats.FirstErrors)
	}
	if _, err := rs.GetBlock(ctx, loc.BlockID); err == nil {
		t.Errorf("packed block %s holding the dup chunk still present after deleting both offsets; want swept", loc.BlockID)
	}
	if stats.ObjectsSwept != 1 {
		t.Errorf("ObjectsSwept = %d, want 1 (the now-unreferenced chunk)", stats.ObjectsSwept)
	}
}

// hashInLiveSet reports whether h appears in the store's EnumerateFileChunks
// (the GC mark live set).
func hashInLiveSet(t *testing.T, ctx context.Context, st metadata.Store, h block.ContentHash) bool {
	t.Helper()
	found := false
	if err := st.EnumerateFileChunks(ctx, func(got block.ContentHash) error {
		if got == h {
			found = true
		}
		return nil
	}); err != nil {
		t.Fatalf("EnumerateFileChunks: %v", err)
	}
	return found
}

func (c *reapCoordinator) DecrementRefCountAndReapMany(ctx context.Context, payloadID string, offsets []uint64) error {
	return reapEach(ctx, payloadID, offsets, c.DecrementRefCountAndReap)
}
