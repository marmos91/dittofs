package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/memory"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// ---------------------------------------------------------------------------
// Test fixtures: MultiShareReconciler over a memory metadata store.
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
// #832-regression tests that exercise the real reap path.
func putPendingBlock(t *testing.T, st metadata.Store, id string, h block.ContentHash) {
	t.Helper()
	if err := st.Put(t.Context(), &block.FileChunk{
		ID:            id,
		Hash:          h,
		State:         block.BlockStatePending,
		BlockStoreKey: h.String(),
		DataSize:      64,
		RefCount:      0,
		LastAccess:    time.Now(),
		CreatedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("PutFileChunk(%s): %v", id, err)
	}
}

// putBlock seeds a FileChunk with a non-zero hash on the given metadata store.
func putBlock(t *testing.T, st metadata.Store, id string, h block.ContentHash) {
	t.Helper()
	if err := st.Put(t.Context(), &block.FileChunk{
		ID:            id,
		Hash:          h,
		State:         block.BlockStateRemote,
		BlockStoreKey: h.String(),
		DataSize:      64,
		RefCount:      1,
		LastAccess:    time.Now(),
		CreatedAt:     time.Now(),
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
// marker in st — the post-#1493 shape of "this chunk is on remote". Returns
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

// chunkOnRemote reports whether h is still remote-reachable post-#1493: its
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

// collectGarbageBlocks runs the post-#1493 remote sweep over a single-share
// fixture: orphan candidates come from st's synced-hash index and reclamation
// goes through a per-share BlockGCReclaimer bound to rbs. opts may carry any
// other knob (DryRun, GracePeriod, HoldProvider, ...).
func collectGarbageBlocks(t *testing.T, rec MetadataReconciler, st metadata.Store, rbs remote.RemoteBlockStore, opts *Options) *GCStats {
	t.Helper()
	if opts == nil {
		opts = &Options{}
	}
	idx, ok := st.(SyncedHashIndex)
	if !ok {
		t.Fatalf("metadata store %T does not implement SyncedHashIndex", st)
	}
	opts.SyncedHashIndex = idx
	opts.BlockReclaimer = newBlockGCReclaimer(st, rbs)
	return CollectGarbage(t.Context(), rec, opts)
}

// ---------------------------------------------------------------------------
// Tests (behaviors 1..10 from 11-06-PLAN.md Task 3).
// ---------------------------------------------------------------------------

// TestGCMarkSweep_MarkPopulatesLiveSet (behavior 1): given a metadata store
// with N FileChunks (M distinct ContentHashes after dedup), the mark phase
// populates GCState with exactly the M distinct non-zero hashes. Zero-hash
// rows are skipped.
func TestGCMarkSweep_MarkPopulatesLiveSet(t *testing.T) {
	ctx := t.Context()
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-a")

	// 3 distinct hashes referenced by 100 blocks (dedup) + a zero-hash legacy row.
	hashes := []block.ContentHash{
		hashFromString("h1"),
		hashFromString("h2"),
		hashFromString("h3"),
	}
	for i := 0; i < 100; i++ {
		putBlock(t, st, fmt.Sprintf("file-x/%d", i), hashes[i%3])
	}
	// One legacy row with zero hash.
	if err := st.Put(ctx, &block.FileChunk{
		ID:       "legacy/0",
		State:    block.BlockStatePending,
		DataSize: 32,
		RefCount: 1,
	}); err != nil {
		t.Fatalf("PutFileChunk(legacy): %v", err)
	}

	root := t.TempDir()
	stats := collectGarbageBlocks(t, rec, st, rs, &Options{GCStateRoot: root})

	// HashesMarked counts every non-zero hash emission (one per
	// FileChunk); GCState.Add deduplicates internally so the live set
	// holds 3 distinct hashes despite 100 marks. The legacy zero-hash
	// row is skipped (h.IsZero() guard).
	if stats.HashesMarked != 100 {
		t.Errorf("HashesMarked = %d, want 100 (one per non-zero block)", stats.HashesMarked)
	}
	if stats.ErrorCount != 0 {
		t.Errorf("ErrorCount = %d, want 0; FirstErrors=%v", stats.ErrorCount, stats.FirstErrors)
	}

	// Verify dedup: the GCState backing the run only stored 3 distinct keys.
	// We validate via a follow-up sweep where 5 CAS objects (3 referenced
	// by the live set, 2 orphans) get the right disposition.
	for _, h := range hashes {
		seedRemoteChunk(t, st, rs, h)
	}
	orphans := []block.ContentHash{
		hashFromString("orphan-x"),
		hashFromString("orphan-y"),
	}
	for _, h := range orphans {
		seedRemoteChunk(t, st, rs, h)
	}
	stats2 := collectGarbageBlocks(t, rec, st, rs, &Options{GCStateRoot: root, GracePeriod: time.Minute})
	if stats2.ObjectsSwept != int64(len(orphans)) {
		t.Errorf("follow-up sweep deleted %d, want %d (dedup miscount)", stats2.ObjectsSwept, len(orphans))
	}
}

// TestGCMarkSweep_SweepHappyPath (behavior 2): given a remote with 5 synced
// chunks (3 referenced + 2 orphan), sweep reclaims exactly the 2 orphans'
// blocks. GCStats.HashesMarked=3, ObjectsSwept=2, BytesFreed=sum of the
// orphans' block object lengths.
func TestGCMarkSweep_SweepHappyPath(t *testing.T) {
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-a")

	live := []block.ContentHash{
		hashFromString("live-1"),
		hashFromString("live-2"),
		hashFromString("live-3"),
	}
	orphans := []block.ContentHash{
		hashFromString("orphan-1"),
		hashFromString("orphan-2"),
	}

	for i, h := range live {
		putBlock(t, st, fmt.Sprintf("file-live/%d", i), h)
		seedRemoteChunk(t, st, rs, h)
	}
	var wantBytes int64
	for _, h := range orphans {
		wantBytes += seedRemoteChunk(t, st, rs, h)
	}

	root := t.TempDir()
	stats := collectGarbageBlocks(t, rec, st, rs, &Options{
		GCStateRoot: root,
		GracePeriod: time.Minute, // < 2h so the seeded markers are eligible
	})

	if stats.HashesMarked != 3 {
		t.Errorf("HashesMarked = %d, want 3", stats.HashesMarked)
	}
	if stats.ObjectsSwept != 2 {
		t.Errorf("ObjectsSwept = %d, want 2", stats.ObjectsSwept)
	}
	// ObjectsScanned counts every synced marker the sweep inspected: the 3
	// live chunks plus the 2 orphans = 5, regardless of how many were swept.
	if stats.ObjectsScanned != 5 {
		t.Errorf("ObjectsScanned = %d, want 5 (3 live + 2 orphans)", stats.ObjectsScanned)
	}
	if stats.ObjectsScanned < stats.ObjectsSwept {
		t.Errorf("ObjectsScanned (%d) must be >= ObjectsSwept (%d)", stats.ObjectsScanned, stats.ObjectsSwept)
	}
	if stats.BytesFreed != wantBytes {
		t.Errorf("BytesFreed = %d, want %d (sum of the orphans' block lengths)", stats.BytesFreed, wantBytes)
	}
	if stats.ErrorCount != 0 {
		t.Errorf("ErrorCount = %d, want 0; FirstErrors=%v", stats.ErrorCount, stats.FirstErrors)
	}

	// Verify live chunks survive.
	for _, h := range live {
		if !chunkOnRemote(t, st, h) {
			t.Errorf("live chunk %x reclaimed", h[:8])
		}
	}
	// Verify orphans reclaimed.
	for _, h := range orphans {
		if chunkOnRemote(t, st, h) {
			t.Errorf("orphan %x not reclaimed", h[:8])
		}
	}
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

func (c *reapCoordinator) DecrementRefCount(ctx context.Context, hash block.ContentHash) (uint32, error) {
	fb, err := c.store.GetByHash(ctx, hash)
	if err != nil || fb == nil {
		return 0, err
	}
	return c.store.DecrementRefCount(ctx, fb.ID)
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

func (c *reapCoordinator) GetFileObjectID(_ context.Context, _ string) (block.ObjectID, error) {
	return block.ObjectID{}, nil
}

var _ MetadataCoordinator = (*reapCoordinator)(nil)

// newReapEngine builds an engine.Store whose coordinator reaps RefCount-0
// FileChunk rows from the supplied metadata store, so a Truncate/Delete that
// drops a hash's last reference removes it from EnumerateFileChunks and the GC
// sweep can reclaim the remote chunk. The engine's own local store / syncer are
// memory-only (no remote) — the GC sweep runs directly against the test's
// separate remote store via CollectGarbage.
func newReapEngine(t *testing.T, st metadata.Store) *Store {
	t.Helper()
	localStore := memory.New()
	fbs := newStubFileChunkStore()
	syncer := NewSyncer(localStore, nil, fbs, DefaultConfig())
	bs, err := New(BlockStoreConfig{
		Local:          localStore,
		Remote:         nil,
		Syncer:         syncer,
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

// TestGCMarkSweep_TruncateReclaimsRemoteChunk (#832): a Truncate that drops a
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

	stats := collectGarbageBlocks(t, rec, st, rs, &Options{
		GCStateRoot: t.TempDir(),
		GracePeriod: time.Minute,
	})
	if stats.ErrorCount != 0 {
		t.Fatalf("ErrorCount = %d, want 0; FirstErrors=%v", stats.ErrorCount, stats.FirstErrors)
	}

	// H2's chunk MUST be swept (its row was reaped → left the live set).
	if chunkOnRemote(t, st, h2) {
		t.Errorf("dropped chunk H2 still present on remote after Truncate+GC; want swept (#832 leak)")
	}
	// H1's chunk MUST survive (still referenced).
	if !chunkOnRemote(t, st, h1) {
		t.Errorf("retained chunk H1 swept after Truncate+GC; want retained")
	}
	if stats.ObjectsSwept != 1 {
		t.Errorf("ObjectsSwept = %d, want 1 (only the dropped H2)", stats.ObjectsSwept)
	}
}

// TestGCMarkSweep_TruncateDedupSafety (#832 data-loss guard, by-ID model): two
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

	stats := collectGarbageBlocks(t, rec, st, rs, &Options{
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

// TestGCMarkSweep_DeleteDuplicateHashNoOverReap (#832 data-loss guard, by-ID
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

	stats := collectGarbageBlocks(t, rec, st, rs, &Options{GCStateRoot: t.TempDir(), GracePeriod: time.Minute})
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

// TestGCMarkSweep_TruncateStraddleHashNoOverReap (#832 data-loss guard, by-ID
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

	stats := collectGarbageBlocks(t, rec, st, rs, &Options{GCStateRoot: t.TempDir(), GracePeriod: time.Minute})
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

// TestGCMarkSweep_PendingReclaimsRemoteChunk (#832, the real-world gap): the
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
		t.Errorf("dropped Pending hash H2 still in EnumerateFileChunks after reap; want gone (#832 no-op reap)")
	}
	if !hashInLiveSet(t, ctx, st, h1) {
		t.Errorf("retained hash H1 missing from EnumerateFileChunks; want present")
	}

	stats := collectGarbageBlocks(t, rec, st, rs, &Options{GCStateRoot: t.TempDir(), GracePeriod: time.Minute})
	if stats.ErrorCount != 0 {
		t.Fatalf("ErrorCount = %d, want 0; FirstErrors=%v", stats.ErrorCount, stats.FirstErrors)
	}
	if chunkOnRemote(t, st, h2) {
		t.Errorf("dropped chunk H2 still remote-reachable after Truncate+GC; want swept (#832 leak)")
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

	stats := collectGarbageBlocks(t, rec, st, rs, &Options{GCStateRoot: t.TempDir(), GracePeriod: time.Minute})
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
	stats2 := collectGarbageBlocks(t, rec, st, rs, &Options{GCStateRoot: t.TempDir(), GracePeriod: time.Minute})
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

// TestGCMarkSweep_SameHashTwoOffsetsBothReaped (#832 + by-ID regression): one
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
		t.Fatalf("dup hash still in EnumerateFileChunks after deleting both rows; want gone (#832 by-hash leak)")
	}

	stats := collectGarbageBlocks(t, rec, st, rs, &Options{GCStateRoot: t.TempDir(), GracePeriod: time.Minute})
	if stats.ErrorCount != 0 {
		t.Fatalf("ErrorCount = %d; FirstErrors=%v", stats.ErrorCount, stats.FirstErrors)
	}
	if _, err := rs.Get(ctx, dup); err == nil {
		t.Errorf("dup chunk still present after deleting both offsets; want swept")
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

// TestGCMarkSweep_GraceTTLPreserves (behavior 3): an orphan whose synced
// marker is younger than snapshot - GracePeriod is NOT reclaimed (within the
// grace window).
func TestGCMarkSweep_GraceTTLPreserves(t *testing.T) {
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("empty")

	// Seed an orphan, then re-stamp its marker to now (within any grace window).
	orphan := hashFromString("recent-orphan")
	seedRemoteChunk(t, st, rs, orphan)
	st.(*metadatamemory.MemoryMetadataStore).MarkSyncedAtForTest(orphan, time.Now())

	stats := collectGarbageBlocks(t, rec, st, rs, &Options{
		GCStateRoot: t.TempDir(),
		GracePeriod: time.Hour,
	})
	if stats.ObjectsSwept != 0 {
		t.Errorf("ObjectsSwept = %d, want 0 (within grace window)", stats.ObjectsSwept)
	}
	if !chunkOnRemote(t, st, orphan) {
		t.Errorf("recent orphan should be preserved by grace TTL")
	}
}

// TestGCMarkSweep_FailClosed (behavior 4): EnumerateFileChunks returns an
// error mid-iteration. Sweep is NOT executed (no block reclaims). Stats
// reports ErrorCount > 0 and a non-empty FirstErrors slice.
func TestGCMarkSweep_FailClosed(t *testing.T) {
	rs := &blockDeleteCountingRemote{Store: remotememory.New()}
	defer func() { _ = rs.Close() }()

	innerRec := newGCMSReconciler()
	innerStore := innerRec.addShare("share-x")

	// Seed an orphan that, absent the mark failure, the sweep would reclaim.
	orphan := hashFromString("would-be-orphan")
	seedRemoteChunk(t, innerStore, rs, orphan)

	// Wrap the inner store so EnumerateFileChunks always errors.
	putBlock(t, innerStore, "file-x/0", hashFromString("h-1"))
	innerRec.stores["share-x"] = &storeWithFailingEnum{
		Store: innerStore,
		err:   errors.New("synthetic enum failure"),
	}

	stats := collectGarbageBlocks(t, innerRec, innerStore, rs, &Options{GCStateRoot: t.TempDir(), GracePeriod: time.Minute})

	if stats.ErrorCount == 0 {
		t.Errorf("ErrorCount = 0, want > 0")
	}
	if len(stats.FirstErrors) == 0 {
		t.Errorf("FirstErrors empty")
	}
	if stats.ObjectsSwept != 0 {
		t.Errorf("ObjectsSwept = %d, want 0 (sweep must not run)", stats.ObjectsSwept)
	}
	if rs.deletes.Load() != 0 {
		t.Errorf("DeleteBlock invoked %d times, want 0 (sweep must not run)", rs.deletes.Load())
	}
	if !chunkOnRemote(t, innerStore, orphan) {
		t.Errorf("orphan reclaimed despite mark fail-closed")
	}
}

// TestGCMarkSweep_SweepErrorsContinueAndCapture (behavior 5): a remote whose
// DeleteBlock fails for one block but succeeds for others — GC continues
// sweeping the remaining candidates; final ErrorCount > 0 and FirstErrors[0]
// mentions the failing block.
func TestGCMarkSweep_SweepErrorsContinueAndCapture(t *testing.T) {
	inner := remotememory.New()
	defer func() { _ = inner.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-empty")

	// Two orphans in distinct blocks: one whose block delete fails, one ok.
	failHash := mustHashWithPrefix(t, "ab")
	okHash := mustHashWithPrefix(t, "cd")

	seedRemoteChunk(t, st, inner, failHash)
	seedRemoteChunk(t, st, inner, okHash)

	// seedRemoteChunk derives block IDs as "blk-<hash-prefix>", so failing
	// the "blk-ab" prefix poisons exactly failHash's block.
	rs := &blockDeleteFailerRemote{Store: inner, failIDPrefix: "blk-ab"}

	stats := collectGarbageBlocks(t, rec, st, rs, &Options{GCStateRoot: t.TempDir(), GracePeriod: time.Minute})

	if stats.ErrorCount == 0 {
		t.Fatalf("ErrorCount = 0, want > 0 (delete error on failHash's block)")
	}
	if len(stats.FirstErrors) == 0 || !strings.Contains(stats.FirstErrors[0], "blk-ab") {
		t.Errorf("FirstErrors[0] = %v, want one mentioning blk-ab", stats.FirstErrors)
	}
	// The other orphan must still have been swept.
	if chunkOnRemote(t, st, okHash) {
		t.Errorf("orphan in non-failing block not reclaimed")
	}
	// The poisoned orphan is retained fail-closed for the next pass.
	if !chunkOnRemote(t, st, failHash) {
		t.Errorf("poisoned orphan's block record dropped despite delete failure")
	}
}

// TestGCMarkSweep_DryRun (behavior 6): DryRun=true performs no Deletes
// DryRunCandidates contains up to DryRunSampleSize candidates
// ObjectsSwept counts what WOULD be deleted; BytesFreed=0.
func TestGCMarkSweep_DryRun(t *testing.T) {
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-empty")

	for i := 0; i < 5; i++ {
		seedRemoteChunk(t, st, rs, hashFromString(fmt.Sprintf("orphan-%d", i)))
	}

	stats := collectGarbageBlocks(t, rec, st, rs, &Options{
		GCStateRoot:      t.TempDir(),
		GracePeriod:      time.Minute,
		DryRun:           true,
		DryRunSampleSize: 3,
	})

	if stats.ObjectsSwept != 5 {
		t.Errorf("ObjectsSwept = %d, want 5 (would-be-deleted count)", stats.ObjectsSwept)
	}
	if stats.BytesFreed != 0 {
		t.Errorf("BytesFreed = %d, want 0 in dry-run", stats.BytesFreed)
	}
	if len(stats.DryRunCandidates) > 3 {
		t.Errorf("DryRunCandidates len = %d, want <= 3 (sample size)", len(stats.DryRunCandidates))
	}
	// Verify nothing was actually deleted.
	for i := 0; i < 5; i++ {
		h := hashFromString(fmt.Sprintf("orphan-%d", i))
		if !chunkOnRemote(t, st, h) {
			t.Errorf("dry-run reclaimed chunk %s; want everything preserved", h)
		}
	}
}

// stubHoldProvider streams a fixed slice of hashes through the HeldHashes
// callback. Used by the positive snapshot-hold test below.
type stubHoldProvider struct {
	held []block.ContentHash
}

func (s stubHoldProvider) HeldHashes(_ context.Context, _ string, _ []string, fn func(block.ContentHash) error) error {
	for _, h := range s.held {
		if err := fn(h); err != nil {
			return err
		}
	}
	return nil
}

// stubErrHoldProvider always errors from HeldHashes. Used by the
// fail-closed regression test.
type stubErrHoldProvider struct{ err error }

func (s stubErrHoldProvider) HeldHashes(_ context.Context, _ string, _ []string, _ func(block.ContentHash) error) error {
	return s.err
}

// TestGCMarkSweep_NoSnapshotHoldProvider: Options.HoldProvider == nil keeps
// the pre-Phase-22 behavior verbatim — mark + sweep proceed with the live
// set derived solely from EnumerateFileChunks.
func TestGCMarkSweep_NoSnapshotHoldProvider(t *testing.T) {
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	// Force LastModified into the past so the orphan is sweep-eligible.

	rec := newGCMSReconciler()
	st := rec.addShare("share-a")

	liveHash := hashFromString("nil-hold-live")
	orphanHash := hashFromString("nil-hold-orphan")
	putBlock(t, st, "file-live/0", liveHash)
	seedRemoteChunk(t, st, rs, liveHash)
	seedRemoteChunk(t, st, rs, orphanHash)

	stats := collectGarbageBlocks(t, rec, st, rs, &Options{
		GCStateRoot: t.TempDir(),
		GracePeriod: time.Minute,
		// HoldProvider intentionally left nil.
	})

	if stats.ErrorCount != 0 {
		t.Fatalf("ErrorCount=%d want 0; FirstErrors=%v", stats.ErrorCount, stats.FirstErrors)
	}
	if stats.HashesMarked != 1 {
		t.Errorf("HashesMarked=%d want 1 (one FileChunk, no holds)", stats.HashesMarked)
	}
	if stats.ObjectsSwept != 1 {
		t.Errorf("ObjectsSwept=%d want 1", stats.ObjectsSwept)
	}
	if !chunkOnRemote(t, st, liveHash) {
		t.Errorf("live hash deleted; want retained")
	}
	if chunkOnRemote(t, st, orphanHash) {
		t.Errorf("orphan should have been deleted")
	}
}

// TestGCMarkSweep_SnapshotHoldProvider: held hashes streamed by the
// HoldProvider land in the same live set as FileChunk hashes — referenced,
// held, and orphan CAS objects each get the right disposition.
func TestGCMarkSweep_SnapshotHoldProvider(t *testing.T) {
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-a")

	hashA := hashFromString("ref-A")  // referenced by a FileChunk
	hashB := hashFromString("held-B") // held by the provider, no FileChunk
	hashC := hashFromString("orphan-C")

	putBlock(t, st, "file-A/0", hashA)
	seedRemoteChunk(t, st, rs, hashA)
	seedRemoteChunk(t, st, rs, hashB)
	seedRemoteChunk(t, st, rs, hashC)

	provider := stubHoldProvider{held: []block.ContentHash{hashB}}

	stats := collectGarbageBlocks(t, rec, st, rs, &Options{
		GCStateRoot:  t.TempDir(),
		GracePeriod:  time.Minute,
		HoldProvider: provider,
	})

	if stats.ErrorCount != 0 {
		t.Fatalf("ErrorCount=%d want 0; FirstErrors=%v", stats.ErrorCount, stats.FirstErrors)
	}
	// 1 FileChunk-derived + 1 held = 2 hashes marked.
	if stats.HashesMarked != 2 {
		t.Errorf("HashesMarked=%d want 2 (1 file + 1 held)", stats.HashesMarked)
	}
	if stats.ObjectsSwept != 1 {
		t.Errorf("ObjectsSwept=%d want 1 (only C is truly orphan)", stats.ObjectsSwept)
	}
	if !chunkOnRemote(t, st, hashA) {
		t.Errorf("referenced hash A deleted; want retained")
	}
	if !chunkOnRemote(t, st, hashB) {
		t.Errorf("held hash B deleted (HoldProvider live-set leak); want retained")
	}
	if chunkOnRemote(t, st, hashC) {
		t.Errorf("unheld orphan C should have been swept")
	}
}

// TestGCMarkSweep_HoldProvider_ErrorFailsClosed: a HoldProvider that errors
// from HeldHashes aborts the run via the mark fail-closed path — sweep does
// NOT execute, and the orphan that would have been deleted stays put.
func TestGCMarkSweep_HoldProvider_ErrorFailsClosed(t *testing.T) {
	rs := &blockDeleteCountingRemote{Store: remotememory.New()}
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-a")
	putBlock(t, st, "file-live/0", hashFromString("hp-live"))

	orphanHash := hashFromString("hp-orphan")
	seedRemoteChunk(t, st, rs, orphanHash)

	provider := stubErrHoldProvider{err: errors.New("simulated hold failure")}

	stats := collectGarbageBlocks(t, rec, st, rs, &Options{
		GCStateRoot:  t.TempDir(),
		GracePeriod:  time.Minute,
		HoldProvider: provider,
	})

	if stats.ErrorCount == 0 {
		t.Fatalf("ErrorCount=0 want >0 (HoldProvider error must fail-closed)")
	}
	if len(stats.FirstErrors) == 0 || !strings.Contains(stats.FirstErrors[0], "hold provider") {
		t.Errorf("FirstErrors=%v want one mentioning 'hold provider'", stats.FirstErrors)
	}
	if stats.ObjectsSwept != 0 {
		t.Errorf("ObjectsSwept=%d want 0 (sweep must not run)", stats.ObjectsSwept)
	}
	if rs.deletes.Load() != 0 {
		t.Errorf("DeleteBlock invoked %d times, want 0 (sweep must not run)", rs.deletes.Load())
	}
	if !chunkOnRemote(t, st, orphanHash) {
		t.Errorf("orphan reclaimed despite mark fail-closed")
	}
}

// TestGCMarkSweep_LastRunJSON (behavior 8): after a successful run
// <gcStateRoot>/last-run.json exists and parses as GCRunSummary.
func TestGCMarkSweep_LastRunJSON(t *testing.T) {
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-empty")

	root := t.TempDir()
	stats := collectGarbageBlocks(t, rec, st, rs, &Options{GCStateRoot: root})
	if stats.ErrorCount != 0 {
		t.Fatalf("ErrorCount = %d, FirstErrors=%v", stats.ErrorCount, stats.FirstErrors)
	}
	body, err := os.ReadFile(filepath.Join(root, "last-run.json"))
	if err != nil {
		t.Fatalf("read last-run.json: %v", err)
	}
	var summary GCRunSummary
	if err := json.Unmarshal(body, &summary); err != nil {
		t.Fatalf("unmarshal last-run.json: %v", err)
	}
	if summary.RunID == "" {
		t.Errorf("RunID empty in last-run.json")
	}
	if summary.RunID != stats.RunID {
		t.Errorf("RunID mismatch: summary=%q stats=%q", summary.RunID, stats.RunID)
	}
}

// TestGCMarkSweep_StaleDirCleanup (behavior 9): a leftover dir with
// incomplete.flag from a prior run is cleaned at the start of the next
// run.
func TestGCMarkSweep_StaleDirCleanup(t *testing.T) {
	root := t.TempDir()
	// Seed a stale dir (incomplete.flag still present).
	stale, err := NewGCState(root, "stale-prior-run")
	if err != nil {
		t.Fatalf("NewGCState: %v", err)
	}
	_ = stale.Close()
	if _, err := os.Stat(filepath.Join(root, "stale-prior-run", "incomplete.flag")); err != nil {
		t.Fatalf("flag missing pre-run: %v", err)
	}

	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-empty")
	_ = collectGarbageBlocks(t, rec, st, rs, &Options{GCStateRoot: root})

	if _, err := os.Stat(filepath.Join(root, "stale-prior-run")); !os.IsNotExist(err) {
		t.Errorf("stale dir not cleaned at run start: stat err=%v", err)
	}
}

// TestGCMarkSweep_ConcurrencyBound has been removed: the engine GC
// sweep no longer shards work across 256 prefix workers (the
// RemoteStore.Walk-based replacement enumerates every CAS object in
// a single call, with sharding now an internal backend concern).
// Any future per-shard Walk extension can be driven by the backend
// without re-introducing a caller-side concurrency knob.

// ---------------------------------------------------------------------------
// Test wrappers: failing reconciler, prefix-failing remote, concurrency tracker.
// ---------------------------------------------------------------------------

// storeWithFailingEnum wraps a metadata store so EnumerateFileChunks
// always returns the configured error. Used by the fail-closed test.
type storeWithFailingEnum struct {
	metadata.Store
	err error
}

func (s *storeWithFailingEnum) EnumerateFileChunks(_ context.Context, _ func(block.ContentHash) error) error {
	return s.err
}

// blockDeleteFailerRemote wraps the memory remote and fails DeleteBlock for
// block IDs carrying the given prefix. Used by the continue+capture sweep
// test: one poisoned block must not stop the sweep from reclaiming others.
type blockDeleteFailerRemote struct {
	*remotememory.Store
	failIDPrefix string
}

func (p *blockDeleteFailerRemote) DeleteBlock(ctx context.Context, blockID string) error {
	if strings.HasPrefix(blockID, p.failIDPrefix) {
		return fmt.Errorf("synthetic delete failure for block %q", blockID)
	}
	return p.Store.DeleteBlock(ctx, blockID)
}

// blockDeleteCountingRemote wraps the memory remote and counts DeleteBlock
// calls. Used to assert that the sweep does NOT execute on mark failure.
type blockDeleteCountingRemote struct {
	*remotememory.Store
	deletes atomic.Int64
}

func (d *blockDeleteCountingRemote) DeleteBlock(ctx context.Context, blockID string) error {
	d.deletes.Add(1)
	return d.Store.DeleteBlock(ctx, blockID)
}

// TestClassifyGCError_DiversifiesByVerb: the
// classifier strips the high-cardinality path/key tail from the verb
// prefix and the body's tail-after-first-":" so semantically distinct
// errors collapse to distinct class keys but per-key noise does not.
func TestClassifyGCError_DiversifiesByVerb(t *testing.T) {
	cases := []struct {
		name     string
		messages []string
		want     int
	}{
		{
			name: "delete vs list collapse to distinct classes",
			messages: []string{
				"delete cas/aa/bb/cc: 503 SlowDown: retry-after",
				"delete cas/dd/ee/ff: 503 SlowDown: retry-after",
				"list aa: 500 InternalError: try later",
			},
			want: 2, // {delete:503 SlowDown, list:500 InternalError}
		},
		{
			name: "same verb same body are one class",
			messages: []string{
				"delete cas/aa/bb/cc: 403 AccessDenied",
				"delete cas/dd/ee/ff: 403 AccessDenied",
				"delete cas/gg/hh/ii: 403 AccessDenied",
			},
			want: 1,
		},
		{
			name: "different bodies under same verb diverge",
			messages: []string{
				"delete cas/aa/bb/cc: 503 SlowDown",
				"delete cas/dd/ee/ff: 403 AccessDenied",
			},
			want: 2,
		},
		{
			name: "multi-word verb 'gcstate has' preserved",
			messages: []string{
				"gcstate has cas/aa/bb/cc: io error",
				"list aa: io error",
			},
			want: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seen := make(map[string]struct{}, len(tc.messages))
			for _, m := range tc.messages {
				seen[classifyGCError(m)] = struct{}{}
			}
			if len(seen) != tc.want {
				keys := make([]string, 0, len(seen))
				for k := range seen {
					keys = append(keys, k)
				}
				t.Errorf("got %d distinct classes %v, want %d", len(seen), keys, tc.want)
			}
		})
	}
}

// TestGCMarkSweep_FirstErrorsDiversifyAcrossClasses
// when a single sweep produces many identical errors (e.g. 503 SlowDown
// from List) plus a single distinct error from another source, the
// distinct error MUST land in FirstErrors instead of being shadowed by
// the homogeneous burst.
func TestGCMarkSweep_FirstErrorsDiversifyAcrossClasses(t *testing.T) {
	inner := remotememory.New()
	defer func() { _ = inner.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-empty")

	// Seed 20 orphans whose hashes land in the failing "blk-ab" block-ID
	// shard so DeleteBlock fires for each and they all fail identically.
	for i := 0; i < 20; i++ {
		h := hashFromString(fmt.Sprintf("ab-orphan-%d", i))
		h[0] = 0xab // force the "blk-ab..." block-ID prefix
		seedRemoteChunk(t, st, inner, h)
	}
	// With 20 identical "block-reclaim <hash>: ... delete block ..." failures
	// FirstErrors must hold exactly ONE entry (collapsed by class) while
	// ErrorCount reflects the full count.
	rs := &blockDeleteFailerRemote{Store: inner, failIDPrefix: "blk-ab"}

	stats := collectGarbageBlocks(t, rec, st, rs, &Options{
		GCStateRoot: t.TempDir(),
		GracePeriod: time.Minute,
	})
	if stats.ErrorCount < 20 {
		t.Fatalf("ErrorCount=%d want >=20", stats.ErrorCount)
	}
	if len(stats.FirstErrors) != 1 {
		t.Errorf("FirstErrors len=%d want 1 (all delete errors are one class), got %v",
			len(stats.FirstErrors), stats.FirstErrors)
	}
}

// TestGCMarkSweep_ConcurrentRunsAgainstSharedRoot
// N parallel CollectGarbage calls that share a GCStateRoot must serialize
// — no run may delete another run's per-runID directory mid-mark. We fire
// 8 goroutines and assert (a) every run completes without an "open
// badger" or "stale dir cleanup" error, (b) ObjectsSwept matches the
// expected orphan count on every run (the live set was not truncated)
// and (c) at run completion every per-run directory has been cleanly
// torn down (MarkComplete removed each incomplete.flag).
func TestGCMarkSweep_ConcurrentRunsAgainstSharedRoot(t *testing.T) {
	const goroutines = 8
	root := t.TempDir()

	// Each goroutine gets its own remote + reconciler so the assertions
	// are simple per-run. Sharing the GCStateRoot is the contended axis.
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	stats := make([]*GCStats, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rs := remotememory.New()
			defer func() { _ = rs.Close() }()
			rec := newGCMSReconciler()
			st := rec.addShare(fmt.Sprintf("share-%d", idx))

			// Seed one live block + one orphan CAS object. With the live
			// set intact the orphan is swept; if a concurrent run trashes
			// our gcstate directory, gcs.Has would return false-negative
			// for the live hash and we would observe ObjectsSwept=2.
			liveHash := hashFromString(fmt.Sprintf("live-%d", idx))
			orphanHash := hashFromString(fmt.Sprintf("orphan-%d", idx))
			putBlock(t, st, fmt.Sprintf("file-%d/0", idx), liveHash)
			seedRemoteChunk(t, st, rs, liveHash)
			seedRemoteChunk(t, st, rs, orphanHash)

			s := collectGarbageBlocks(t, rec, st, rs, &Options{
				GCStateRoot: root,
				GracePeriod: time.Nanosecond, // make orphan eligible immediately
			})
			stats[idx] = s
			if s.ErrorCount != 0 {
				errs[idx] = fmt.Errorf("run %d errors: %v", idx, s.FirstErrors)
			}
			if s.ObjectsSwept != 1 {
				errs[idx] = fmt.Errorf("run %d: ObjectsSwept=%d want 1 (live truncated by race?)", idx, s.ObjectsSwept)
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	// Every run's directory should have a removed incomplete.flag (MarkComplete).
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir(root): %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		flag := filepath.Join(root, e.Name(), "incomplete.flag")
		if _, err := os.Stat(flag); err == nil {
			t.Errorf("incomplete.flag survived in %s — MarkComplete did not run", e.Name())
		}
	}
}

// mustHashWithPrefix returns a ContentHash whose hex starts with the
// given two-char prefix. We brute-force a counter into the seed string
// until we land in the right shard.
func mustHashWithPrefix(t *testing.T, hexPrefix string) block.ContentHash {
	t.Helper()
	if len(hexPrefix) != 2 {
		t.Fatalf("hexPrefix must be 2 chars, got %q", hexPrefix)
	}
	for i := 0; i < 1_000_000; i++ {
		h := hashFromString(fmt.Sprintf("seed-%s-%d", hexPrefix, i))
		if h.String()[:2] == hexPrefix {
			return h
		}
	}
	t.Fatalf("could not find hash with prefix %q", hexPrefix)
	return block.ContentHash{}
}

// TestGCMarkSweep_ClearsSyncedMarkerForSweptHash proves the remote sweep clears
// the synced marker for every chunk it reclaims, keeping the synced set a strict
// subset of remote contents. Without it, ListUnsynced skips a swept hash forever
// and a later snapshot's durability verify fails on a chunk that never re-uploads
// (#1433). A surviving live chunk keeps its marker. Unlike
// TestGCIndexSweep_DeletesOrphansWithoutWalk (fake index), this runs against the
// real metadata store's marker index.
func TestGCMarkSweep_ClearsSyncedMarkerForSweptHash(t *testing.T) {
	ctx := t.Context()
	rs := remotememory.New()
	defer func() { _ = rs.Close() }()

	rec := newGCMSReconciler()
	st := rec.addShare("share-a")

	live := hashFromString("live-keep")
	orphan := hashFromString("orphan-sweep")
	putBlock(t, st, "file-live/0", live)
	seedRemoteChunk(t, st, rs, live)
	seedRemoteChunk(t, st, rs, orphan)

	stats := collectGarbageBlocks(t, rec, st, rs, &Options{
		GCStateRoot: t.TempDir(),
		GracePeriod: time.Minute,
	})
	if stats.ObjectsSwept != 1 {
		t.Fatalf("ObjectsSwept = %d, want 1 (FirstErrors=%v)", stats.ObjectsSwept, stats.FirstErrors)
	}

	// Swept orphan: marker cleared so it can re-upload if it reappears live.
	if ok, _ := st.IsSynced(ctx, orphan); ok {
		t.Errorf("swept orphan still marked synced; ListUnsynced would skip it forever (#1433)")
	}
	// Live chunk: still on remote AND still synced (marker untouched).
	if ok, _ := st.IsSynced(ctx, live); !ok {
		t.Errorf("live chunk's synced marker wrongly cleared")
	}
	if !chunkOnRemote(t, st, live) {
		t.Errorf("live chunk reclaimed")
	}
}

func TestResolveGracePeriod(t *testing.T) {
	const hour = time.Hour
	tests := []struct {
		name string
		opts Options
		want time.Duration
	}{
		{"unset positive uses caller value", Options{GracePeriod: 30 * time.Minute}, 30 * time.Minute},
		{"unset zero falls back to 1h", Options{GracePeriod: 0}, hour},
		{"unset negative falls back to 1h", Options{GracePeriod: -5 * time.Second}, hour},
		{"set zero is authoritative", Options{GracePeriod: 0, GracePeriodSet: true}, 0},
		{"set positive is authoritative", Options{GracePeriod: 2 * hour, GracePeriodSet: true}, 2 * hour},
		{"set negative clamps to zero", Options{GracePeriod: -1, GracePeriodSet: true}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := tt.opts
			if got := resolveGracePeriod(&opts); got != tt.want {
				t.Fatalf("resolveGracePeriod(%+v) = %v, want %v", tt.opts, got, tt.want)
			}
		})
	}
}
