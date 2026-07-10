package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	gosync "sync"
	"sync/atomic"
	"testing"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/health"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// spyingRemoteStore wraps a remote.RemoteStore and counts how many times the
// ranged block-chunk read entry point (ChunkReader.ReadChunk) is invoked.
// Post-#1493 that is the ONLY remote read path — the legacy per-hash
// ReadBlockVerified surface is gone from the engine read path.
type spyingRemoteStore struct {
	remote.RemoteStore
	readChunkCalls atomic.Int64
	mu             gosync.Mutex
	readChunkKeys  []string // blocks/<id> store keys, in call order
	// onReadChunk, if set, runs at the start of each ReadChunk with the target
	// block ID — lets a test inject a concurrent relocation mid-fetch.
	onReadChunk func(blockID string)
}

func newSpyingRemoteStore(inner remote.RemoteStore) *spyingRemoteStore {
	return &spyingRemoteStore{RemoteStore: inner}
}

func (s *spyingRemoteStore) ReadChunk(ctx context.Context, blockID string, offset, length int64, expected block.ContentHash) ([]byte, error) {
	s.readChunkCalls.Add(1)
	s.mu.Lock()
	s.readChunkKeys = append(s.readChunkKeys, block.FormatBlockKey(blockID))
	hook := s.onReadChunk
	s.mu.Unlock()
	if hook != nil {
		hook(blockID)
	}
	return s.RemoteStore.(remote.ChunkReader).ReadChunk(ctx, blockID, offset, length, expected)
}

// Healthcheck delegates so the syncer's HealthMonitor sees a healthy
// status; without this the wrapper would shadow the interface method
// with the default zero-value Report (fail-closed unhealthy).
func (s *spyingRemoteStore) Healthcheck(ctx context.Context) health.Report {
	return s.RemoteStore.Healthcheck(ctx)
}

// dualReadEnv is a self-contained syncer fixture using in-memory
// metadata + spying remote store. The syncer is NOT Started — tests
// drive fetchBlock directly so the periodic uploader does not race.
type dualReadEnv struct {
	tmp     string
	ms      *metadatamemory.MemoryMetadataStore
	rs      *spyingRemoteStore
	innerRS *remotememory.Store
	syncer  *Syncer
}

func newDualReadEnv(t *testing.T) *dualReadEnv {
	t.Helper()
	tmp := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := fs.NewWithOptions(tmp, 0, ms, fs.FSStoreOptions{
		LocalChunkIndex: ms})
	if err != nil {
		t.Fatalf("fs.NewWithOptions: %v", err)
	}
	inner := remotememory.New()
	rs := newSpyingRemoteStore(inner)

	cfg := DefaultConfig()
	cfg.ClaimTimeout = 100 * time.Millisecond

	m := NewSyncer(bc, rs, ms, cfg)
	// Wire the synced-hash store so dispatchRemoteFetch can resolve block
	// locators recorded by MarkSynced (post-#1493 the ONLY remote read route).
	m.SetSyncedHashStore(ms)
	t.Cleanup(func() {
		_ = m.Close()
		// Close the fs store to release the logBlob fd BEFORE t.TempDir's
		// RemoveAll runs (LIFO: this cleanup is registered after TempDir, so
		// it runs first). Windows cannot unlink a blob file that is still open.
		_ = bc.Close()
		_ = inner.Close()
	})
	return &dualReadEnv{tmp: tmp, ms: ms, rs: rs, innerRS: inner, syncer: m}
}

func dualReadHash(data []byte) block.ContentHash {
	sum := blake3.Sum256(data)
	var h block.ContentHash
	copy(h[:], sum[:])
	return h
}

// seedBlockChunk registers data as a single-chunk packed block: the block
// object lands on the remote, MarkSynced records its block locator, and a
// FileChunk row makes the chunk reachable from (payloadID, blockIdx 0) — the
// same durable state a carve commit leaves behind.
func (env *dualReadEnv) seedBlockChunk(t *testing.T, ctx context.Context, payloadID, blockID string, blockBytes []byte, hash block.ContentHash, dataSize uint32) {
	t.Helper()
	if err := env.innerRS.PutBlock(ctx, blockID, bytes.NewReader(blockBytes)); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	loc := block.ChunkLocator{BlockID: blockID, WireOffset: 0, WireLength: int64(len(blockBytes))}
	if err := env.ms.MarkSynced(ctx, hash, loc); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}
	fb := &block.FileChunk{
		ID:            fmt.Sprintf("%s/0", payloadID),
		Hash:          hash,
		DataSize:      dataSize,
		BlockStoreKey: block.FormatBlockKey(blockID),
		State:         block.BlockStateRemote,
		RefCount:      1,
		LastAccess:    time.Now(),
		CreatedAt:     time.Now(),
	}
	if err := env.ms.Put(ctx, fb); err != nil {
		t.Fatalf("PutFileChunk: %v", err)
	}
}

// TestDualRead_BlockRowRoutesToVerifiedChunkRead asserts a FileChunk with a
// non-zero Hash and a recorded block locator routes through the ranged
// ChunkReader.ReadChunk path (BLAKE3-verified in readChunkVerified) and
// round-trips byte-identical.
func TestDualRead_BlockRowRoutesToVerifiedChunkRead(t *testing.T) {
	env := newDualReadEnv(t)
	ctx := context.Background()

	const payloadID = "share/block-file"
	const blockID = "blk-dualread-route"
	data := []byte("packed-block bytes — verified ranged read on fetch")
	hash := dualReadHash(data)
	env.seedBlockChunk(t, ctx, payloadID, blockID, data, hash, uint32(len(data)))

	got, err := env.syncer.fetchBlock(ctx, payloadID, 0)
	if err != nil {
		t.Fatalf("fetchBlock: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("fetchBlock data mismatch: got %q, want %q", got, data)
	}

	if env.rs.readChunkCalls.Load() != 1 {
		t.Errorf("ReadChunk calls = %d, want 1", env.rs.readChunkCalls.Load())
	}
	if got := env.rs.readChunkKeys[0]; got != block.FormatBlockKey(blockID) {
		t.Errorf("ReadChunk key = %q, want %q", got, block.FormatBlockKey(blockID))
	}
}

// TestDualRead_MissingLocatorRowRefused pins the fetchBlock-level fail-closed
// contract: a FileChunk row with a non-zero Hash but NO recorded locator (the
// startup migration repacked every synced hash, so this is post-migration
// drift) must be refused without touching the remote — never silent zeros.
// TestDualRead_SyncedStandaloneLocatorMissingFailsClosed: a row whose hash IS
// synced but carries an empty-BlockID (standalone) locator with no bytes
// anywhere (no local copy, no legacy cas/ object) must fail closed with
// ErrChunkNotFound — never silent zeros. (When the legacy object exists, the
// read-path fallback serves it; that path is covered in read_path_test.go and
// locator_fetch_test.go. A hash with NO marker is the benign "not uploaded yet"
// case, covered by TestDispatchRemoteFetch_UnsyncedChunkFallsBackToLocal.)
func TestDualRead_SyncedStandaloneLocatorMissingFailsClosed(t *testing.T) {
	env := newDualReadEnv(t)
	ctx := context.Background()

	const payloadID = "share/drift-row"
	data := []byte("drifted bytes with a standalone locator")
	hash := dualReadHash(data)

	fb := &block.FileChunk{
		ID:         fmt.Sprintf("%s/0", payloadID),
		Hash:       hash,
		DataSize:   uint32(len(data)),
		State:      block.BlockStateRemote,
		RefCount:   1,
		LastAccess: time.Now(),
		CreatedAt:  time.Now(),
	}
	if err := env.ms.Put(ctx, fb); err != nil {
		t.Fatalf("PutFileChunk: %v", err)
	}
	// Synced marker with an empty-BlockID (standalone) locator = drift.
	if err := env.ms.MarkSynced(ctx, hash, block.ChunkLocator{}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	got, err := env.syncer.fetchBlock(ctx, payloadID, 0)
	if !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("fetchBlock err = %v, want ErrChunkNotFound (standalone bytes resident nowhere)", err)
	}
	if got != nil {
		t.Errorf("fetchBlock data = %v, want nil on miss", got)
	}
	if env.rs.readChunkCalls.Load() != 0 {
		t.Errorf("ReadChunk calls = %d, want 0 (block-range read never runs for a standalone locator)", env.rs.readChunkCalls.Load())
	}
}

// TestDualRead_NoFileChunkReturnsNil asserts that a missing metadata row
// (sparse / never uploaded) yields no remote call and a nil result.
func TestDualRead_NoFileChunkReturnsNil(t *testing.T) {
	env := newDualReadEnv(t)
	ctx := context.Background()

	got, err := env.syncer.fetchBlock(ctx, "share/missing", 0)
	if err != nil {
		t.Fatalf("fetchBlock: %v", err)
	}
	if got != nil {
		t.Fatalf("fetchBlock data = %v, want nil for sparse block", got)
	}
	if env.rs.readChunkCalls.Load() != 0 {
		t.Errorf("expected zero remote chunk reads for sparse block, got %d",
			env.rs.readChunkCalls.Load())
	}
}

// TestDualRead_BlockRowMismatchSurfacesError asserts that corrupted remote
// block bytes that fail the BLAKE3 recompute are surfaced as
// ErrChunkContentMismatch through the engine read path (plumbed end-to-end),
// with no data returned.
func TestDualRead_BlockRowMismatchSurfacesError(t *testing.T) {
	env := newDualReadEnv(t)
	ctx := context.Background()

	const payloadID = "share/block-mismatch"
	const blockID = "blk-dualread-corrupt"
	expected := []byte("expected payload — caller asks for THIS hash")
	wrongBytes := []byte("WRONG bytes — must fail body recompute!!!!!!")
	hash := dualReadHash(expected)

	// Seed the block-resident chunk with CORRUPT remote block bytes: the
	// locator and FileChunk row claim `hash`, but the block object holds
	// wrongBytes. readChunkVerified re-hashes the ranged read and must
	// surface ErrChunkContentMismatch.
	env.seedBlockChunk(t, ctx, payloadID, blockID, wrongBytes, hash, uint32(len(expected)))

	got, err := env.syncer.fetchBlock(ctx, payloadID, 0)
	if err == nil {
		t.Fatal("fetchBlock: expected ErrChunkContentMismatch, got nil")
	}
	if !errors.Is(err, block.ErrChunkContentMismatch) {
		t.Fatalf("fetchBlock err = %v, want wrapped ErrChunkContentMismatch", err)
	}
	if got != nil {
		t.Errorf("fetchBlock data = %v, want nil on content mismatch", got)
	}

	// The verified ranged-read path must have been chosen.
	if env.rs.readChunkCalls.Load() != 1 {
		t.Errorf("ReadChunk calls = %d, want 1", env.rs.readChunkCalls.Load())
	}
}

// TestDualRead_MissingBlockObjectFailsClosed: a row with a non-zero hash
// whose recorded locator points at a block object that has been DELETED from
// the remote MUST surface as ErrChunkNotFound, NOT silently return zeros.
// Fail-closed GC makes this state structurally impossible, but if a bug ever
// lets a live block get reaped, the read path should fail loudly rather than
// corrupt the caller's data.
func TestDualRead_MissingBlockObjectFailsClosed(t *testing.T) {
	env := newDualReadEnv(t)
	ctx := context.Background()

	const payloadID = "share/block-missing"
	const blockID = "blk-dualread-reaped"
	data := []byte("expected payload")
	hash := dualReadHash(data)

	// Seed the full block-resident state, then delete the block object out
	// from under the locator — the "live data loss" drift this test pins.
	env.seedBlockChunk(t, ctx, payloadID, blockID, data, hash, uint32(len(data)))
	if err := env.innerRS.DeleteBlock(ctx, blockID); err != nil {
		t.Fatalf("DeleteBlock: %v", err)
	}

	got, err := env.syncer.fetchBlock(ctx, payloadID, 0)
	if err == nil {
		t.Fatalf("fetchBlock: expected ErrChunkNotFound, got nil with data=%v", got)
	}
	if !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("fetchBlock err = %v, want wrapped ErrChunkNotFound", err)
	}
	if got != nil {
		t.Errorf("fetchBlock data = %v, want nil on fail-closed block miss", got)
	}
}

// TestDualRead_RelocatedChunkReResolvesOnMiss pins the #1487 compaction
// stale-locator guard: a reader that resolved a chunk's locator to the OLD
// block, then had that block deleted after compaction relocated+recommitted the
// chunk into a NEW block, must re-resolve and read through — NOT surface a
// spurious EIO. Without the read-path re-resolve (fetchResolvedBlock) this fails
// closed with ErrChunkNotFound even though the chunk is perfectly live.
func TestDualRead_RelocatedChunkReResolvesOnMiss(t *testing.T) {
	env := newDualReadEnv(t)
	ctx := context.Background()

	const payloadID = "share/relocated-chunk"
	const oldBlockID = "blk-old-precompaction"
	const newBlockID = "blk-new-postcompaction"
	data := []byte("live chunk bytes relocated by compaction")
	hash := dualReadHash(data)

	// Seed the pre-compaction state: the locator points at the old block.
	env.seedBlockChunk(t, ctx, payloadID, oldBlockID, data, hash, uint32(len(data)))

	// Delete the old block object up front (compaction's step-3 DeleteBlock). The
	// reader below still resolves the OLD locator first, so its first GET 404s —
	// the stale-locator window.
	if err := env.innerRS.DeleteBlock(ctx, oldBlockID); err != nil {
		t.Fatalf("DeleteBlock(old): %v", err)
	}

	// The instant the reader issues its (doomed) GET against the old block,
	// compaction commits the relocation (step 1 PutBlock + step 2 last-wins
	// locator rebind). The reader's FIRST fetch still 404s on the already-deleted
	// old object; the guard must re-resolve the now-updated locator and succeed.
	env.rs.onReadChunk = relocateOnFirstRead(t, env, oldBlockID, newBlockID, hash, data)

	got, err := env.syncer.fetchBlock(ctx, payloadID, 0)
	if err != nil {
		t.Fatalf("fetchBlock: relocated chunk must re-resolve and read through, got %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("fetchBlock data mismatch: got %q, want %q", got, data)
	}
	// Exactly two ReadChunk calls: the doomed old-block GET + the re-resolved
	// new-block GET. Proves a single bounded retry, not a loop.
	if n := env.rs.readChunkCalls.Load(); n != 2 {
		t.Errorf("ReadChunk calls = %d, want 2 (one miss + one re-resolved hit)", n)
	}
}

// relocateOnFirstRead returns a one-shot onReadChunk hook that, the first time
// oldBlockID is read, simulates compaction committing the chunk's relocation
// into newBlockID: PutBlock (step 1) then the last-wins DeleteSynced→MarkSynced
// locator rebind DefaultCommitBlock performs (step 2). That reproduces the exact
// #1487 stale-locator window — a locator resolved to the old block, then rebound
// and the old object deleted, before the read's GET lands.
func relocateOnFirstRead(t *testing.T, env *dualReadEnv, oldBlockID, newBlockID string, hash block.ContentHash, data []byte) func(string) {
	t.Helper()
	ctx := context.Background()
	var once gosync.Once
	return func(blockID string) {
		if blockID != oldBlockID {
			return
		}
		once.Do(func() {
			if err := env.innerRS.PutBlock(ctx, newBlockID, bytes.NewReader(data)); err != nil {
				t.Errorf("PutBlock(new): %v", err)
			}
			if err := env.ms.DeleteSynced(ctx, hash); err != nil {
				t.Errorf("DeleteSynced(old): %v", err)
			}
			newLoc := block.ChunkLocator{BlockID: newBlockID, WireOffset: 0, WireLength: int64(len(data))}
			if err := env.ms.MarkSynced(ctx, hash, newLoc); err != nil {
				t.Errorf("MarkSynced(new): %v", err)
			}
		})
	}
}

// TestEnsureAvailableAndRead_RelocatedChunkReResolvesOnMiss drives the CLIENT
// demand read path (EnsureAvailableAndRead → inlineFetchOrWait →
// dispatchRemoteFetch) — the path whose EIO actually propagates to a real
// NFS/SMB read — through the same #1487 compaction relocation scenario. It
// asserts the demand read SUCCEEDS via the shared re-resolve guard in
// dispatchRemoteFetch instead of failing closed. This is the regression guard
// for the path that mattered; the background fetchResolvedBlock path is covered
// by TestDualRead_RelocatedChunkReResolvesOnMiss.
func TestEnsureAvailableAndRead_RelocatedChunkReResolvesOnMiss(t *testing.T) {
	env := newDualReadEnv(t)
	ctx := context.Background()

	const payloadID = "share/relocated-demand"
	const oldBlockID = "blk-old-demand"
	const newBlockID = "blk-new-demand"
	data := []byte("client demand read of a compaction-relocated chunk")
	hash := dualReadHash(data)

	// Pre-compaction state, then delete the old object (compaction step 3) so the
	// reader's first GET against the stale locator 404s.
	env.seedBlockChunk(t, ctx, payloadID, oldBlockID, data, hash, uint32(len(data)))
	if err := env.innerRS.DeleteBlock(ctx, oldBlockID); err != nil {
		t.Fatalf("DeleteBlock(old): %v", err)
	}
	env.rs.onReadChunk = relocateOnFirstRead(t, env, oldBlockID, newBlockID, hash, data)

	dest := make([]byte, len(data))
	if _, err := env.syncer.EnsureAvailableAndRead(ctx, payloadID, 0, uint32(len(data)), dest); err != nil {
		t.Fatalf("EnsureAvailableAndRead: relocated chunk must re-resolve and read through, got %v", err)
	}
	// New contract: EnsureAvailableAndRead ensures every covering chunk is LOCAL
	// and returns (false, nil); it no longer copies into dest (the caller's
	// readLocalByHash does the correct per-offset assembly). Verify the relocated
	// bytes were staged locally, byte-identical.
	staged, err := env.syncer.local.Get(ctx, hash)
	if err != nil {
		t.Fatalf("relocated chunk not staged locally after re-resolve: %v", err)
	}
	if !bytes.Equal(staged, data) {
		t.Fatalf("staged chunk mismatch: got %q, want %q", staged, data)
	}
	// One miss + one re-resolved hit: single bounded retry, not a loop.
	if n := env.rs.readChunkCalls.Load(); n != 2 {
		t.Errorf("ReadChunk calls = %d, want 2 (one miss + one re-resolved hit)", n)
	}
}

// TestDualRead_LegacyRowRefusedPostMigration: a FileChunk
// with a zero ContentHash that reaches dispatchRemoteFetch is migration
// drift. 's boot guard refuses to start against an un-migrated
// store, but if the sentinel is lost or hand-removed and a stray legacy-
// shaped row surfaces at runtime, the read path MUST refuse rather than
// silently return zeros.
func TestDualRead_LegacyRowRefusedPostMigration(t *testing.T) {
	env := newDualReadEnv(t)
	ctx := context.Background()

	const payloadID = "share/legacy-row"
	// Synthesize the legacy "{payloadID}/block-{idx}" key shape directly
	// the helper was deleted in with the rest of the legacy
	// path-keyed surface.
	legacyKey := payloadID + "/block-0"

	// Legacy-shaped FileChunk: Hash zero, BlockStoreKey set.
	fb := &block.FileChunk{
		ID:            fmt.Sprintf("%s/0", payloadID),
		DataSize:      32,
		BlockStoreKey: legacyKey,
		State:         block.BlockStatePending,
		RefCount:      1,
		LastAccess:    time.Now(),
		CreatedAt:     time.Now(),
	}
	if err := env.ms.Put(ctx, fb); err != nil {
		t.Fatalf("PutFileChunk: %v", err)
	}

	got, err := env.syncer.fetchBlock(ctx, payloadID, 0)
	if err == nil {
		t.Fatalf("fetchBlock: expected legacy-row refusal, got nil with data=%v", got)
	}
	// The error message carries the block_id so operators can triage
	// which row the migration tool missed.
	if got := err.Error(); !contains(got, "legacy zero-hash FileChunk") || !contains(got, payloadID) {
		t.Errorf("fetchBlock err = %v, want one mentioning legacy zero-hash + payloadID", err)
	}
	if got != nil {
		t.Errorf("fetchBlock data = %v, want nil on legacy refusal", got)
	}
	if env.rs.readChunkCalls.Load() != 0 {
		t.Errorf("ReadChunk calls = %d, want 0 (legacy row has no hash)", env.rs.readChunkCalls.Load())
	}
}

// contains is a tiny helper that returns true if s contains substr (used
// to keep the error-message assertion above readable).
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
