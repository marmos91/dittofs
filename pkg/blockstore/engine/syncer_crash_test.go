package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// Phase 11 Plan 02 D-16: deterministic crash-injection unit tests for
// INV-03. The invariant says no orphan uploads can leave a State=Remote
// row without a successful PUT, and a successful PUT followed by a
// metadata-txn crash leaves the row at Syncing (so GC can reap the S3
// object after grace).
//
// Three kill points:
//   - pre-PUT:                   crash before any S3 call         → 0 objects, row Syncing, no orphan
//   - between-PUT-and-meta:      PUT succeeds, metadata fails     → 1 object, row Syncing
//   - post-meta:                 both succeed                     → 1 object, row Remote
//
// All tests drive uploadOne directly (no periodic loop) for determinism.

// errKillPoint is the sentinel returned by both crash wrappers.
var errKillPoint = errors.New("kill point injected")

// crashingRemoteStore wraps an in-memory RemoteStore and can be told to
// fail WriteBlockWithHash before forwarding to the real store. Used to
// simulate a kill before the S3 PUT lands.
type crashingRemoteStore struct {
	remote.RemoteStore
	failBeforePut bool
	puts          atomic.Int64
}

func (c *crashingRemoteStore) WriteBlockWithHash(ctx context.Context, key string, hash blockstore.ContentHash, data []byte) error {
	if c.failBeforePut {
		return errKillPoint
	}
	c.puts.Add(1)
	return c.RemoteStore.WriteBlockWithHash(ctx, key, hash, data)
}

// crashingFileBlockStore wraps a real FileBlockStore and can be told to
// fail PutFileBlock on a specific call number. failOnNthPut == 0 disables;
// failOnNthPut == 1 fails the very first put; etc. The test seeds the
// fixture with PutFileBlock to flip the row to Syncing (counted as the
// first put), then drives uploadOne which performs the second put — the
// "between-PUT-and-meta" scenario sets failOnNthPut == 2.
type crashingFileBlockStore struct {
	blockstore.FileBlockStore
	failOnNthPut int64
	putCount     atomic.Int64
}

func (c *crashingFileBlockStore) PutFileBlock(ctx context.Context, b *blockstore.FileBlock) error {
	n := c.putCount.Add(1)
	if c.failOnNthPut > 0 && n == c.failOnNthPut {
		return errKillPoint
	}
	return c.FileBlockStore.PutFileBlock(ctx, b)
}

// EnumerateSyncingBlocks forwards to the embedded store when it implements
// the syncingEnumerator capability — required for the recovery janitor
// test to surface the stale row through the wrapper.
func (c *crashingFileBlockStore) EnumerateSyncingBlocks(ctx context.Context) ([]*blockstore.FileBlock, error) {
	if e, ok := c.FileBlockStore.(interface {
		EnumerateSyncingBlocks(context.Context) ([]*blockstore.FileBlock, error)
	}); ok {
		return e.EnumerateSyncingBlocks(ctx)
	}
	return nil, nil
}

// crashFixture bundles the wrappers, the underlying memory backends, and
// a Syncing-state FileBlock pointing at a real local file. The fixture
// performs the Pending → Syncing transition itself so uploadOne can run
// against a ready row.
type crashFixture struct {
	syncer        *Syncer
	rs            *crashingRemoteStore
	memRemote     *remotememory.Store
	ms            *crashingFileBlockStore
	memMeta       *metadatamemory.MemoryMetadataStore
	fb            *blockstore.FileBlock
	expectedKey   string
	expectedBytes []byte
}

func setupCrashFixture(t *testing.T) *crashFixture {
	t.Helper()
	tmp := t.TempDir()
	memMeta := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := fs.New(tmp, 0, 0, memMeta)
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	memRemote := remotememory.New()
	t.Cleanup(func() { _ = memRemote.Close() })

	ms := &crashingFileBlockStore{FileBlockStore: memMeta}
	rs := &crashingRemoteStore{RemoteStore: memRemote}

	cfg := DefaultConfig()
	cfg.ClaimBatchSize = 4
	cfg.UploadConcurrency = 2
	cfg.ClaimTimeout = 50 * time.Millisecond
	syncer := NewSyncer(bc, rs, ms, cfg)
	t.Cleanup(func() { _ = syncer.Close() })

	// Seed a local file + Pending FileBlock.
	payload := []byte("crash-test-payload")
	path := filepath.Join(tmp, "crash.blk")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fb := &blockstore.FileBlock{
		ID:         "share/0",
		LocalPath:  path,
		DataSize:   uint32(len(payload)),
		State:      blockstore.BlockStatePending,
		RefCount:   1,
		LastAccess: time.Now(),
		CreatedAt:  time.Now(),
	}

	// Manually flip to Syncing using the underlying (non-crashing) store so
	// the put-counter on the wrapper starts at 0 — uploadOne's metadata
	// write will be the first counted put.
	fb.State = blockstore.BlockStateSyncing
	fb.LastSyncAttemptAt = time.Now()
	if err := memMeta.PutFileBlock(context.Background(), fb); err != nil {
		t.Fatalf("PutFileBlock(syncing): %v", err)
	}

	// Pre-compute the CAS key the test expects.
	// (We don't have to match exactly — uploadOne will compute it from the
	// payload's BLAKE3 hash — but the assertion is structural.)
	return &crashFixture{
		syncer:        syncer,
		rs:            rs,
		memRemote:     memRemote,
		ms:            ms,
		memMeta:       memMeta,
		fb:            fb,
		expectedBytes: payload,
	}
}

// TestSyncerCrash_PrePut: WriteBlockWithHash returns errKillPoint before
// touching the in-memory remote. Assert that no object exists, the
// FileBlock row remains Syncing, and no metadata write occurred after
// the failure.
func TestSyncerCrash_PrePut(t *testing.T) {
	f := setupCrashFixture(t)
	f.rs.failBeforePut = true

	err := f.syncer.uploadOne(context.Background(), f.fb)
	if err == nil || !errors.Is(err, errKillPoint) {
		t.Fatalf("uploadOne err = %v, want wrap of errKillPoint", err)
	}

	if got := f.memRemote.BlockCount(); got != 0 {
		t.Errorf("remote object count = %d, want 0 (no orphan)", got)
	}
	if got := f.rs.puts.Load(); got != 0 {
		t.Errorf("PUT counter = %d, want 0 (PUT must not have started)", got)
	}
	if got := f.ms.putCount.Load(); got != 0 {
		t.Errorf("metadata put counter = %d, want 0 (no meta write after PUT failure)", got)
	}

	// Re-fetch the persisted row from the underlying store; State must be
	// Syncing (not Remote, not reverted to Pending).
	persisted, err := f.memMeta.GetFileBlock(context.Background(), f.fb.ID)
	if err != nil {
		t.Fatalf("GetFileBlock: %v", err)
	}
	if persisted.State != blockstore.BlockStateSyncing {
		t.Errorf("persisted.State = %v, want Syncing (INV-03: no orphan promotion)", persisted.State)
	}
}

// TestSyncerCrash_BetweenPutAndMeta: WriteBlockWithHash succeeds (object
// recorded in memory remote), then the next PutFileBlock returns
// errKillPoint. Assert that exactly 1 object exists at the CAS key, the
// persisted row stays State=Syncing (because the txn failed), and the
// in-memory struct's mutated fields do NOT leak into storage.
func TestSyncerCrash_BetweenPutAndMeta(t *testing.T) {
	f := setupCrashFixture(t)
	// Fail on the FIRST put issued by uploadOne (the Syncing→Remote one);
	// the seed put used the underlying memMeta directly, so the wrapper's
	// counter is still at 0.
	f.ms.failOnNthPut = 1

	err := f.syncer.uploadOne(context.Background(), f.fb)
	if err == nil || !errors.Is(err, errKillPoint) {
		t.Fatalf("uploadOne err = %v, want wrap of errKillPoint", err)
	}

	if got := f.memRemote.BlockCount(); got != 1 {
		t.Errorf("remote object count = %d, want 1 (PUT succeeded)", got)
	}
	if got := f.rs.puts.Load(); got != 1 {
		t.Errorf("PUT counter = %d, want 1", got)
	}

	// The persisted row MUST still be Syncing — INV-03: no Remote without
	// a successful metadata-txn. The in-memory fb may already have its
	// State field flipped optimistically; re-fetching from storage is the
	// authoritative check.
	persisted, err := f.memMeta.GetFileBlock(context.Background(), f.fb.ID)
	if err != nil {
		t.Fatalf("GetFileBlock: %v", err)
	}
	if persisted.State != blockstore.BlockStateSyncing {
		t.Errorf("persisted.State = %v, want Syncing (INV-03)", persisted.State)
	}
	// LastSyncAttemptAt should still be the seed timestamp (non-zero) —
	// if a faulty implementation cleared it on failure the janitor would
	// requeue immediately on the next tick.
	if persisted.LastSyncAttemptAt.IsZero() {
		t.Error("persisted.LastSyncAttemptAt was cleared; janitor would requeue prematurely")
	}
}

// TestSyncerCrash_PostMeta: both PUT and metadata-txn succeed. Assert
// the happy-path final state — 1 object exists at the CAS key, the
// persisted row reads State=Remote with the correct CAS key.
func TestSyncerCrash_PostMeta(t *testing.T) {
	f := setupCrashFixture(t)
	// No failure injected.

	if err := f.syncer.uploadOne(context.Background(), f.fb); err != nil {
		t.Fatalf("uploadOne: %v", err)
	}

	if got := f.memRemote.BlockCount(); got != 1 {
		t.Fatalf("remote object count = %d, want 1", got)
	}
	persisted, err := f.memMeta.GetFileBlock(context.Background(), f.fb.ID)
	if err != nil {
		t.Fatalf("GetFileBlock: %v", err)
	}
	if persisted.State != blockstore.BlockStateRemote {
		t.Errorf("persisted.State = %v, want Remote", persisted.State)
	}
	if persisted.BlockStoreKey == "" {
		t.Fatal("persisted.BlockStoreKey is empty after happy-path uploadOne")
	}
	if _, err := blockstore.ParseCASKey(persisted.BlockStoreKey); err != nil {
		t.Errorf("BlockStoreKey %q is not a valid CAS key: %v", persisted.BlockStoreKey, err)
	}
	// And the bytes are actually retrievable from the remote.
	if _, err := f.memRemote.ReadBlock(context.Background(), persisted.BlockStoreKey); err != nil {
		t.Errorf("ReadBlock(%s) after happy uploadOne: %v", persisted.BlockStoreKey, err)
	}
}

// TestSyncerCrash_RecoveryRequeuesStale: drives the BetweenPutAndMeta
// crash, waits past ClaimTimeout, then runs recoverStaleSyncing and
// asserts the row flips back to Pending with a zero LastSyncAttemptAt.
// This exercises the D-14 janitor against a row left stranded by the
// INV-03 crash.
func TestSyncerCrash_RecoveryRequeuesStale(t *testing.T) {
	f := setupCrashFixture(t)
	f.ms.failOnNthPut = 1

	if err := f.syncer.uploadOne(context.Background(), f.fb); err == nil {
		t.Fatal("uploadOne returned nil under between-PUT-and-meta crash")
	}

	// Wait past ClaimTimeout (50ms) so the seed timestamp is stale.
	time.Sleep(80 * time.Millisecond)

	if err := f.syncer.recoverStaleSyncing(context.Background()); err != nil {
		t.Fatalf("recoverStaleSyncing: %v", err)
	}

	persisted, err := f.memMeta.GetFileBlock(context.Background(), f.fb.ID)
	if err != nil {
		t.Fatalf("GetFileBlock: %v", err)
	}
	if persisted.State != blockstore.BlockStatePending {
		t.Errorf("after janitor: State = %v, want Pending (D-14 requeue)", persisted.State)
	}
	if !persisted.LastSyncAttemptAt.IsZero() {
		t.Errorf("after janitor: LastSyncAttemptAt = %v, want zero (D-14 reset)", persisted.LastSyncAttemptAt)
	}
}
