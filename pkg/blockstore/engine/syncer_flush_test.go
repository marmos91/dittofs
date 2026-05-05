package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	"github.com/marmos91/dittofs/pkg/health"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// Phase 13 Plan 12 (BSCAS-04): unit-level coverage for the rewritten
// Syncer.Flush — drains every Pending/Syncing block to Remote and then
// invokes persistFileBlocksAfterFlush exactly once on full quiesce. These
// tests stay in their own file (not the integration-tagged
// pkg/blockstore/engine/syncer_test.go) so they run under
// `go test -short ./pkg/blockstore/engine/...` without Localstack.

// newFlushSyncer constructs a Syncer wired to in-memory backends, attaches
// a fakeCoordinator (so persistFileBlocksAfterFlush is exercised), and
// returns the components tests reach into. The syncer is NOT Started — the
// periodic uploader would race assertions; tests drive Flush directly.
func newFlushSyncer(t *testing.T, rs remote.RemoteStore) (*Syncer, *metadatamemory.MemoryMetadataStore, string, *fakeCoordinator) {
	t.Helper()
	tmp := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := fs.New(tmp, 0, 0, ms)
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}

	cfg := DefaultConfig()
	cfg.ClaimBatchSize = 8
	cfg.UploadConcurrency = 4
	cfg.ClaimTimeout = 100 * time.Millisecond

	m := NewSyncer(bc, rs, ms, cfg)
	fc := newFakeCoordinator()
	m.SetCoordinator(fc)

	t.Cleanup(func() {
		_ = m.Close()
	})
	return m, ms, tmp, fc
}

// seedNumericPendingBlock seeds a FileBlock with a numeric block index in
// its ID — the format parseBlockID + memory.listFileBlocks expect. The
// helper in syncer_unit_test.go uses letter suffixes which would be skipped
// by ListFileBlocks; we need numeric indices for the new flush tests.
func seedNumericPendingBlock(
	t *testing.T,
	ms *metadatamemory.MemoryMetadataStore,
	tmp, payloadID string,
	blockIdx uint64,
	payload []byte,
) *blockstore.FileBlock {
	t.Helper()
	id := fmt.Sprintf("%s/%d", payloadID, blockIdx)
	path := filepath.Join(tmp, fmt.Sprintf("blk-%s-%d", payloadID, blockIdx))
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	b := &blockstore.FileBlock{
		ID:         id,
		LocalPath:  path,
		DataSize:   uint32(len(payload)),
		State:      blockstore.BlockStatePending,
		RefCount:   1,
		LastAccess: time.Now(),
		CreatedAt:  time.Now(),
	}
	if err := ms.Put(context.Background(), b); err != nil {
		t.Fatalf("Put FileBlock: %v", err)
	}
	return b
}

// failingRemote wraps a remote.RemoteStore and induces a
// WriteBlockWithHash failure on the Nth (1-based) call. All other calls
// pass through to the inner store. Used by
// TestSyncer_Flush_PartialQuiesceSkipsHook to leave one block in Syncing
// after Flush so the post-Flush hook MUST NOT fire.
type failingRemote struct {
	inner            remote.RemoteStore
	failOnWriteCount int
	writeCount       atomic.Int64
}

func (f *failingRemote) WriteBlock(ctx context.Context, key string, data []byte) error {
	return f.inner.WriteBlock(ctx, key, data)
}
func (f *failingRemote) WriteBlockWithHash(ctx context.Context, key string, hash blockstore.ContentHash, data []byte) error {
	n := f.writeCount.Add(1)
	if n == int64(f.failOnWriteCount) {
		return errors.New("induced WriteBlockWithHash failure")
	}
	return f.inner.WriteBlockWithHash(ctx, key, hash, data)
}
func (f *failingRemote) ReadBlock(ctx context.Context, key string) ([]byte, error) {
	return f.inner.ReadBlock(ctx, key)
}
func (f *failingRemote) ReadBlockVerified(ctx context.Context, key string, expected blockstore.ContentHash) ([]byte, error) {
	return f.inner.ReadBlockVerified(ctx, key, expected)
}
func (f *failingRemote) ReadBlockRange(ctx context.Context, key string, offset, length int64) ([]byte, error) {
	return f.inner.ReadBlockRange(ctx, key, offset, length)
}
func (f *failingRemote) DeleteBlock(ctx context.Context, key string) error {
	return f.inner.DeleteBlock(ctx, key)
}
func (f *failingRemote) DeleteByPrefix(ctx context.Context, prefix string) error {
	return f.inner.DeleteByPrefix(ctx, prefix)
}
func (f *failingRemote) ListByPrefix(ctx context.Context, prefix string) ([]string, error) {
	return f.inner.ListByPrefix(ctx, prefix)
}
func (f *failingRemote) ListByPrefixWithMeta(ctx context.Context, prefix string) ([]remote.ObjectInfo, error) {
	return f.inner.ListByPrefixWithMeta(ctx, prefix)
}
func (f *failingRemote) HeadObject(ctx context.Context, key string) (remote.HeadResult, error) {
	return f.inner.HeadObject(ctx, key)
}
func (f *failingRemote) CopyBlock(ctx context.Context, src, dst string) error {
	return f.inner.CopyBlock(ctx, src, dst)
}
func (f *failingRemote) Close() error                        { return f.inner.Close() }
func (f *failingRemote) HealthCheck(_ context.Context) error { return nil }
func (f *failingRemote) Healthcheck(_ context.Context) health.Report {
	return health.Report{Status: health.StatusHealthy}
}

// TestSyncer_Flush_InvokesPostFlushHook is the headline assertion for
// Phase 13 Plan 12: after Flush drains every Pending block to Remote, the
// post-Flush coordinator hook fires exactly once with the canonical sorted
// BlockRef list and the correct Merkle-root ObjectID.
func TestSyncer_Flush_InvokesPostFlushHook(t *testing.T) {
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })
	m, ms, tmp, fc := newFlushSyncer(t, rs)

	const payloadID = "pid-1"
	const n = 3
	// Seed 3 Pending blocks with deterministic content. Block 1 stays
	// distinct from blocks 0 and 2 so the dedup short-circuit in
	// uploadOne does not collapse them and we get 3 BlockRefs in the
	// final list.
	for i := uint64(0); i < n; i++ {
		seedNumericPendingBlock(t, ms, tmp, payloadID, i,
			[]byte(fmt.Sprintf("flush-payload-%d", i)))
	}

	res, err := m.Flush(context.Background(), payloadID)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if res == nil {
		t.Fatal("Flush returned nil FlushResult")
	}
	if !res.Finalized {
		t.Errorf("Flush.Finalized = false, want true (full quiesce + hook fired)")
	}

	if len(fc.persistCalls) != 1 {
		t.Fatalf("fakeCoordinator.persistCalls = %d, want 1", len(fc.persistCalls))
	}
	rec := fc.persistCalls[0]
	if rec.payloadID != payloadID {
		t.Errorf("persistCall.payloadID = %q, want %q", rec.payloadID, payloadID)
	}
	if len(rec.blocks) != n {
		t.Fatalf("persistCall.blocks = %d, want %d", len(rec.blocks), n)
	}
	// D-01 invariant: BlockRef list is sorted by Offset.
	if !sort.SliceIsSorted(rec.blocks, func(i, j int) bool {
		return rec.blocks[i].Offset < rec.blocks[j].Offset
	}) {
		t.Errorf("persistCall.blocks not sorted by Offset: %+v", rec.blocks)
	}
	// ObjectID is non-zero AND equals ComputeObjectID(blocks). This is the
	// correctness gate the gap-closure cluster grades against — proves
	// the engine threads the same byte order through the hash function as
	// any future re-derivation would.
	if rec.objectID.IsZero() {
		t.Error("persistCall.objectID is zero — Merkle root not computed")
	}
	if got, want := rec.objectID, blockstore.ComputeObjectID(rec.blocks); got != want {
		t.Errorf("persistCall.objectID = %s, want ComputeObjectID(blocks) = %s",
			got.String(), want.String())
	}
}

// TestSyncer_Flush_PartialQuiesceSkipsHook asserts D-06: when uploadOne
// fails on any block, Flush returns the error AND does NOT invoke the
// post-Flush hook. The next successful Flush will recompute.
func TestSyncer_Flush_PartialQuiesceSkipsHook(t *testing.T) {
	inner := remotememory.New()
	t.Cleanup(func() { _ = inner.Close() })
	rs := &failingRemote{inner: inner, failOnWriteCount: 2} // fail second WriteBlockWithHash
	m, ms, tmp, fc := newFlushSyncer(t, rs)

	const payloadID = "pid-2"
	for i := uint64(0); i < 3; i++ {
		seedNumericPendingBlock(t, ms, tmp, payloadID, i,
			[]byte(fmt.Sprintf("partial-payload-%d", i)))
	}

	res, err := m.Flush(context.Background(), payloadID)
	if err == nil {
		t.Fatalf("Flush: want error from induced upload failure, got nil (res=%+v)", res)
	}
	// Hook MUST NOT have fired (D-06: full quiesce only).
	if len(fc.persistCalls) != 0 {
		t.Errorf("fakeCoordinator.persistCalls = %d, want 0 (partial quiesce must skip hook)",
			len(fc.persistCalls))
	}
}

// TestSyncer_Flush_NilCoordinatorIsNoop asserts the test-ergonomics
// contract: a Syncer with no coordinator (rare in production, common in
// pre-wiring fixtures) returns success from Flush after draining without
// panicking on the nil hook.
func TestSyncer_Flush_NilCoordinatorIsNoop(t *testing.T) {
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })
	tmp := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := fs.New(tmp, 0, 0, ms)
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	cfg := DefaultConfig()
	cfg.ClaimBatchSize = 4
	m := NewSyncer(bc, rs, ms, cfg)
	t.Cleanup(func() { _ = m.Close() })
	// Intentionally do NOT call SetCoordinator.

	const payloadID = "pid-3"
	seedNumericPendingBlock(t, ms, tmp, payloadID, 0, []byte("nil-coord-payload"))

	res, err := m.Flush(context.Background(), payloadID)
	if err != nil {
		t.Fatalf("Flush with nil coordinator: %v", err)
	}
	if res == nil {
		t.Fatal("Flush returned nil result")
	}
}

// TestSyncer_Flush_NoBlocksForPayloadIsNoop asserts the empty-payload
// short-circuit: when ListFileBlocks returns no blocks for payloadID,
// Flush completes successfully and does NOT invoke the coordinator hook.
// The runtime coordinator's PersistFileBlocks would error on an unknown
// payloadID, so silently skipping preserves the no-op semantics for any
// pre-write Flush call (e.g., a CLOSE on an opened-but-untouched file).
func TestSyncer_Flush_NoBlocksForPayloadIsNoop(t *testing.T) {
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })
	m, _, _, fc := newFlushSyncer(t, rs)

	res, err := m.Flush(context.Background(), "pid-empty")
	if err != nil {
		t.Fatalf("Flush(empty payload): %v", err)
	}
	if res == nil {
		t.Fatal("Flush returned nil result")
	}
	if res.Finalized {
		t.Error("Flush.Finalized = true on empty payload, want false (no quiesce performed)")
	}
	if len(fc.persistCalls) != 0 {
		t.Errorf("fakeCoordinator.persistCalls = %d, want 0 (empty payload must skip hook)",
			len(fc.persistCalls))
	}
}

// ============================================================================
// Phase 13 Plan 13 (BSCAS-05): file-level dedup short-circuit before drain
// ============================================================================

// countingRemote wraps a remote.RemoteStore and counts WriteBlockWithHash
// calls. The Phase 13 Plan 13 file-level dedup tests use this counter as
// the headline assertion — a hit MUST produce zero CAS PUTs (the per-block
// upload pump is bypassed entirely), and a miss MUST produce one PUT per
// distinct block (the pump runs as before).
type countingRemote struct {
	inner      remote.RemoteStore
	writeCount atomic.Int64
}

func (c *countingRemote) WriteBlock(ctx context.Context, key string, data []byte) error {
	return c.inner.WriteBlock(ctx, key, data)
}
func (c *countingRemote) WriteBlockWithHash(ctx context.Context, key string, hash blockstore.ContentHash, data []byte) error {
	c.writeCount.Add(1)
	return c.inner.WriteBlockWithHash(ctx, key, hash, data)
}
func (c *countingRemote) ReadBlock(ctx context.Context, key string) ([]byte, error) {
	return c.inner.ReadBlock(ctx, key)
}
func (c *countingRemote) ReadBlockVerified(ctx context.Context, key string, expected blockstore.ContentHash) ([]byte, error) {
	return c.inner.ReadBlockVerified(ctx, key, expected)
}
func (c *countingRemote) ReadBlockRange(ctx context.Context, key string, offset, length int64) ([]byte, error) {
	return c.inner.ReadBlockRange(ctx, key, offset, length)
}
func (c *countingRemote) DeleteBlock(ctx context.Context, key string) error {
	return c.inner.DeleteBlock(ctx, key)
}
func (c *countingRemote) DeleteByPrefix(ctx context.Context, prefix string) error {
	return c.inner.DeleteByPrefix(ctx, prefix)
}
func (c *countingRemote) ListByPrefix(ctx context.Context, prefix string) ([]string, error) {
	return c.inner.ListByPrefix(ctx, prefix)
}
func (c *countingRemote) ListByPrefixWithMeta(ctx context.Context, prefix string) ([]remote.ObjectInfo, error) {
	return c.inner.ListByPrefixWithMeta(ctx, prefix)
}
func (c *countingRemote) HeadObject(ctx context.Context, key string) (remote.HeadResult, error) {
	return c.inner.HeadObject(ctx, key)
}
func (c *countingRemote) CopyBlock(ctx context.Context, src, dst string) error {
	return c.inner.CopyBlock(ctx, src, dst)
}
func (c *countingRemote) Close() error                        { return c.inner.Close() }
func (c *countingRemote) HealthCheck(_ context.Context) error { return nil }
func (c *countingRemote) Healthcheck(_ context.Context) health.Report {
	return health.Report{Status: health.StatusHealthy}
}

// seedHashedPendingBlock seeds a Pending FileBlock with a pre-set Hash
// field so the file-level dedup short-circuit (which builds its
// speculativeBlocks list from the FileBlock projection) sees real
// hashes. Mirrors seedNumericPendingBlock but with a caller-provided
// Hash. Phase 13 Plan 13: in production, the FastCDC chunker has
// already populated FileBlock.Hash before Syncer.Flush runs (per
// pkg/blockstore/local/fs/rollup.go); in unit tests we pre-seed the
// hash to model that invariant deterministically.
func seedHashedPendingBlock(
	t *testing.T,
	ms *metadatamemory.MemoryMetadataStore,
	tmp, payloadID string,
	blockIdx uint64,
	hash blockstore.ContentHash,
	payload []byte,
) *blockstore.FileBlock {
	t.Helper()
	id := fmt.Sprintf("%s/%d", payloadID, blockIdx)
	path := filepath.Join(tmp, fmt.Sprintf("blk-%s-%d", payloadID, blockIdx))
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	b := &blockstore.FileBlock{
		ID:         id,
		Hash:       hash,
		LocalPath:  path,
		DataSize:   uint32(len(payload)),
		State:      blockstore.BlockStatePending,
		RefCount:   1,
		LastAccess: time.Now(),
		CreatedAt:  time.Now(),
	}
	if err := ms.Put(context.Background(), b); err != nil {
		t.Fatalf("Put FileBlock: %v", err)
	}
	return b
}

// makeBlockRef constructs a BlockRef for the given block index, using the
// engine.BlockSize stride that snapshotPendingBlockRefs expects.
func makeBlockRef(hash blockstore.ContentHash, blockIdx uint64, size uint32) blockstore.BlockRef {
	return blockstore.BlockRef{
		Hash:   hash,
		Offset: blockIdx * uint64(BlockSize),
		Size:   size,
	}
}

// TestSyncer_Flush_FileLevelDedupHitSkipsUploadPump is the headline
// assertion for Phase 13 Plan 13 (BSCAS-05): when the D-09 trigger
// condition holds AND the provisional ObjectID matches a previously-
// quiesced file, Syncer.Flush must invoke TrySpeculativeFileLevelDedup,
// the metadata-side swap fires (RefCount++ on target hashes,
// FileAttr.Blocks/ObjectID written), and the per-block upload pump is
// BYPASSED entirely. The headline gate is countingRemote.writeCount==0.
func TestSyncer_Flush_FileLevelDedupHitSkipsUploadPump(t *testing.T) {
	inner := remotememory.New()
	t.Cleanup(func() { _ = inner.Close() })
	rs := &countingRemote{inner: inner}
	m, ms, tmp, fc := newFlushSyncer(t, rs)

	const payloadID = "pid-clone"
	h1 := blockstore.ContentHash{0x11}
	h2 := blockstore.ContentHash{0x22}
	h3 := blockstore.ContentHash{0x33}
	const dataSize = uint32(64)

	// Seed 3 Pending blocks WITH pre-set hashes — models the post-rollup
	// state where FastCDC has populated FileBlock.Hash.
	seedHashedPendingBlock(t, ms, tmp, payloadID, 0, h1, make([]byte, dataSize))
	seedHashedPendingBlock(t, ms, tmp, payloadID, 1, h2, make([]byte, dataSize))
	seedHashedPendingBlock(t, ms, tmp, payloadID, 2, h3, make([]byte, dataSize))

	// Seed: file has never quiesced (zero ObjectID).
	fc.fileObjectIDs[payloadID] = blockstore.ObjectID{}

	// Seed: target file already exists with the same chunk sequence.
	target := []blockstore.BlockRef{
		makeBlockRef(h1, 0, dataSize),
		makeBlockRef(h2, 1, dataSize),
		makeBlockRef(h3, 2, dataSize),
	}
	provisional := blockstore.ComputeObjectID(target)
	fc.objectIDHits[provisional] = target

	res, err := m.Flush(context.Background(), payloadID)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if res == nil || !res.Finalized {
		t.Fatalf("Flush.Finalized = false, want true (file-level dedup hit)")
	}

	// Headline assertion: zero CAS PUTs (the upload pump was bypassed).
	if rs.writeCount.Load() != 0 {
		t.Errorf("countingRemote.writeCount = %d, want 0 (file-level dedup hit must bypass upload pump)",
			rs.writeCount.Load())
	}

	// Coordinator: IncrementRefCount once per distinct target hash.
	if len(fc.incHashes) != 3 {
		t.Errorf("fakeCoordinator.incHashes = %d, want 3 (one per distinct target hash)",
			len(fc.incHashes))
	}

	// Coordinator: PersistFileBlocks called once with the target's
	// blocks and the provisional ObjectID (the dedup-hit path commits
	// the target's BlockRef list, not the speculative one).
	if len(fc.persistCalls) != 1 {
		t.Fatalf("fakeCoordinator.persistCalls = %d, want 1", len(fc.persistCalls))
	}
	rec := fc.persistCalls[0]
	if rec.payloadID != payloadID {
		t.Errorf("persistCall.payloadID = %q, want %q", rec.payloadID, payloadID)
	}
	if rec.objectID != provisional {
		t.Errorf("persistCall.objectID = %s, want provisional %s",
			rec.objectID.String(), provisional.String())
	}
	if len(rec.blocks) != 3 {
		t.Errorf("persistCall.blocks = %d, want 3 (target's BlockRefs)", len(rec.blocks))
	}

	// FindByObjectID must have been consulted with the provisional
	// ObjectID — proves the trigger evaluation reached the dedup path.
	foundCall := false
	for _, oid := range fc.findCalls {
		if oid == provisional {
			foundCall = true
			break
		}
	}
	if !foundCall {
		t.Errorf("FindByObjectID(provisional=%s) not invoked; findCalls=%v",
			provisional.String(), fc.findCalls)
	}
}

// TestSyncer_Flush_FileLevelDedupMissProceedsToUpload asserts the miss
// path: when the provisional ObjectID has no match in the metadata
// store, Syncer.Flush falls through to the existing per-block upload
// pump (Plan 13-12) and the post-Flush hook fires once with the real
// post-drain ObjectID.
func TestSyncer_Flush_FileLevelDedupMissProceedsToUpload(t *testing.T) {
	inner := remotememory.New()
	t.Cleanup(func() { _ = inner.Close() })
	rs := &countingRemote{inner: inner}
	m, ms, tmp, fc := newFlushSyncer(t, rs)

	const payloadID = "pid-novel"
	const n = 3
	// Distinct payloads → distinct hashes once uploadOne computes them.
	for i := uint64(0); i < n; i++ {
		seedNumericPendingBlock(t, ms, tmp, payloadID, i,
			[]byte(fmt.Sprintf("novel-payload-%d", i)))
	}

	// Trigger condition holds (zero ObjectID, all-Pending) but
	// objectIDHits is empty — FindByObjectID returns miss.
	fc.fileObjectIDs[payloadID] = blockstore.ObjectID{}

	res, err := m.Flush(context.Background(), payloadID)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if res == nil || !res.Finalized {
		t.Fatalf("Flush.Finalized = false, want true (full quiesce via per-block path)")
	}

	// Headline assertion: per-block path uploaded each block once.
	if rs.writeCount.Load() != n {
		t.Errorf("countingRemote.writeCount = %d, want %d (per-block path must run on miss)",
			rs.writeCount.Load(), n)
	}

	// File-level dedup path was NOT entered: zero IncrementRefCount calls.
	// (The post-Flush hook only invokes PersistFileBlocks, which does
	// not touch IncrementRefCount.)
	if len(fc.incHashes) != 0 {
		t.Errorf("fakeCoordinator.incHashes = %d, want 0 (miss must skip applyFileLevelDedupHit)",
			len(fc.incHashes))
	}

	// Post-Flush hook fired once with the real (non-zero) ObjectID
	// computed over the now-Remote BlockRef list (Plan 13-12 path).
	if len(fc.persistCalls) != 1 {
		t.Fatalf("fakeCoordinator.persistCalls = %d, want 1 (post-Flush hook)", len(fc.persistCalls))
	}
	rec := fc.persistCalls[0]
	if rec.objectID.IsZero() {
		t.Error("persistCall.objectID is zero — post-Flush hook should compute Merkle root")
	}
}

// TestSyncer_Flush_FileLevelDedupSkippedWhenObjectIDNonZero asserts the
// D-09 trigger guard: when the file already has a non-zero prior
// ObjectID (the file was previously quiesced — a re-flush / incremental
// write), the file-level dedup short-circuit is BYPASSED even if a hit
// would otherwise fire. The per-block path runs and the post-Flush
// hook recomputes the new ObjectID.
func TestSyncer_Flush_FileLevelDedupSkippedWhenObjectIDNonZero(t *testing.T) {
	inner := remotememory.New()
	t.Cleanup(func() { _ = inner.Close() })
	rs := &countingRemote{inner: inner}
	m, ms, tmp, fc := newFlushSyncer(t, rs)

	const payloadID = "pid-requiesce"
	h1 := blockstore.ContentHash{0xA1}
	h2 := blockstore.ContentHash{0xA2}
	h3 := blockstore.ContentHash{0xA3}

	// Distinct payloads → distinct content hashes once uploadOne computes
	// them (avoids the uploadOne pre-PUT dedup short-circuit collapsing
	// zero-filled blocks). The pre-set Hash on the FileBlock is what the
	// pre-drain projection sees; the real hash computed during upload is
	// what countingRemote observes.
	payloads := [][]byte{
		[]byte("requiesce-payload-A-distinct"),
		[]byte("requiesce-payload-B-distinct"),
		[]byte("requiesce-payload-C-distinct"),
	}
	const dataSize = uint32(28)
	for i, h := range []blockstore.ContentHash{h1, h2, h3} {
		seedHashedPendingBlock(t, ms, tmp, payloadID, uint64(i), h, payloads[i])
	}

	// Prior ObjectID is non-zero — file already quiesced once before.
	priorObjectID := blockstore.ObjectID{0xCA, 0xFE}
	fc.fileObjectIDs[payloadID] = priorObjectID

	// Seed objectIDHits so a hit WOULD fire if the trigger guard ignored
	// the non-zero prior ObjectID. The guard MUST veto.
	target := []blockstore.BlockRef{
		makeBlockRef(h1, 0, dataSize),
		makeBlockRef(h2, 1, dataSize),
		makeBlockRef(h3, 2, dataSize),
	}
	fc.objectIDHits[blockstore.ComputeObjectID(target)] = target

	res, err := m.Flush(context.Background(), payloadID)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if res == nil || !res.Finalized {
		t.Fatalf("Flush.Finalized = false, want true (full quiesce via per-block path)")
	}

	// Per-block path ran (D-09 trigger vetoed file-level path).
	if rs.writeCount.Load() != 3 {
		t.Errorf("countingRemote.writeCount = %d, want 3 (D-09 must veto file-level path on non-zero prior ObjectID)",
			rs.writeCount.Load())
	}

	// applyFileLevelDedupHit not entered.
	if len(fc.incHashes) != 0 {
		t.Errorf("fakeCoordinator.incHashes = %d, want 0 (file-level path must be vetoed)",
			len(fc.incHashes))
	}
}

// TestSyncer_Flush_FileLevelDedupSkippedWhenSomeBlocksRemote asserts the
// "all-Pending" arm of the D-09 trigger condition: when the FileBlock
// projection contains any non-Pending block (e.g., a partial drain
// from a previous failed Flush, or an in-progress periodic uploader
// tick), the file-level dedup short-circuit is BYPASSED. The per-block
// path drains the remaining Pending blocks and the post-Flush hook
// fires.
func TestSyncer_Flush_FileLevelDedupSkippedWhenSomeBlocksRemote(t *testing.T) {
	inner := remotememory.New()
	t.Cleanup(func() { _ = inner.Close() })
	rs := &countingRemote{inner: inner}
	m, ms, tmp, fc := newFlushSyncer(t, rs)

	const payloadID = "pid-mixed"
	// Seed 2 Pending blocks (will be uploaded by the per-block path).
	seedNumericPendingBlock(t, ms, tmp, payloadID, 0, []byte("mixed-pending-0"))
	seedNumericPendingBlock(t, ms, tmp, payloadID, 1, []byte("mixed-pending-1"))

	// Seed 1 already-Remote block — the trigger condition's
	// "every block.State == Pending" guard MUST veto.
	id2 := fmt.Sprintf("%s/2", payloadID)
	path2 := filepath.Join(tmp, "blk-mixed-2")
	if err := os.WriteFile(path2, []byte("mixed-remote-2"), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	remoteBlock := &blockstore.FileBlock{
		ID:            id2,
		Hash:          blockstore.ContentHash{0xBB},
		LocalPath:     path2,
		BlockStoreKey: "cas/bb/00/already-uploaded",
		DataSize:      uint32(len("mixed-remote-2")),
		State:         blockstore.BlockStateRemote,
		RefCount:      1,
		LastAccess:    time.Now(),
		CreatedAt:     time.Now(),
	}
	if err := ms.Put(context.Background(), remoteBlock); err != nil {
		t.Fatalf("Put remote FileBlock: %v", err)
	}

	fc.fileObjectIDs[payloadID] = blockstore.ObjectID{}

	res, err := m.Flush(context.Background(), payloadID)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if res == nil || !res.Finalized {
		t.Fatalf("Flush.Finalized = false, want true")
	}

	// Per-block path uploaded the 2 Pending blocks; the already-Remote
	// block was skipped by drainPayloadToRemote.
	if rs.writeCount.Load() != 2 {
		t.Errorf("countingRemote.writeCount = %d, want 2 (only Pending blocks should upload)",
			rs.writeCount.Load())
	}

	// applyFileLevelDedupHit not entered.
	if len(fc.incHashes) != 0 {
		t.Errorf("fakeCoordinator.incHashes = %d, want 0 (file-level path must be vetoed by mixed states)",
			len(fc.incHashes))
	}
}
