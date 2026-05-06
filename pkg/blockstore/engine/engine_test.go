package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	"github.com/marmos91/dittofs/pkg/blockstore/local/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// stubFileBlockStore is a minimal blockstore.EngineFileBlockStore for
// testing that satisfies the interface but stores nothing. We only need
// it to construct a Syncer. Phase 12 (META-03 / D-09): the public
// FileBlockStore narrowed to 6 methods; the engine still consumes the
// wider EngineFileBlockStore (adds GetFileBlock + ListFileBlocks).
type stubFileBlockStore struct{}

func (s *stubFileBlockStore) GetByHash(_ context.Context, _ blockstore.ContentHash) (*blockstore.FileBlock, error) {
	return nil, nil
}
func (s *stubFileBlockStore) Put(_ context.Context, _ *blockstore.FileBlock) error {
	return nil
}
func (s *stubFileBlockStore) Delete(_ context.Context, _ string) error { return nil }
func (s *stubFileBlockStore) IncrementRefCount(_ context.Context, _ string) error {
	return nil
}
func (s *stubFileBlockStore) DecrementRefCount(_ context.Context, _ string) (uint32, error) {
	return 0, nil
}
func (s *stubFileBlockStore) ListPending(_ context.Context, _ time.Duration, _ int) ([]*blockstore.FileBlock, error) {
	return nil, nil
}

// Engine-internal surface (kept off the public FileBlockStore per
// META-03 / D-09).
func (s *stubFileBlockStore) GetFileBlock(_ context.Context, _ string) (*blockstore.FileBlock, error) {
	return nil, blockstore.ErrFileBlockNotFound
}
func (s *stubFileBlockStore) ListFileBlocks(_ context.Context, _ string) ([]*blockstore.FileBlock, error) {
	return nil, nil
}

// newTestEngine creates an engine.BlockStore with memory local store, nil remote,
// optional cache budget and prefetch settings. Coordinator is left nil — tests
// that exercise Coordinator-dependent paths (CopyPayload/Delete/Truncate
// with non-empty BlockRef list) should use newTestEngineWithCoordinator.
func newTestEngine(t *testing.T, readBufferBytes int64, prefetchWorkers int) *BlockStore {
	t.Helper()
	localStore := memory.New()
	fbs := &stubFileBlockStore{}
	syncer := NewSyncer(localStore, nil, fbs, DefaultConfig())

	bs, err := New(Config{
		Local:           localStore,
		Remote:          nil,
		Syncer:          syncer,
		ReadBufferBytes: readBufferBytes,
		PrefetchWorkers: prefetchWorkers,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs
}

// newTestEngineWithCoordinator creates an engine.BlockStore with the
// supplied MetadataCoordinator wired in (Phase 12 Plan 07 Task 0).
// Used by tests that assert engine-coordinator integration without
// touching the heavier Syncer/Remote setup.
func newTestEngineWithCoordinator(t *testing.T, c MetadataCoordinator) *BlockStore {
	t.Helper()
	localStore := memory.New()
	fbs := &stubFileBlockStore{}
	syncer := NewSyncer(localStore, nil, fbs, DefaultConfig())

	bs, err := New(Config{
		Local:       localStore,
		Remote:      nil,
		Syncer:      syncer,
		Coordinator: c,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs
}

// TestReadAt_BasicRoundtrip exercises ReadAt + WriteAt without any cache
// integration concerns. Phase 12 Plan 09 simplified the read path:
// reads always go through local/remote stores; the Cache is hint-only
// (post-read OnRead hook) and does not serve bytes.
func TestReadAt_BasicRoundtrip(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)

	ctx := context.Background()
	payloadID := "test-file-1"
	data := []byte("hello world")

	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	buf := make([]byte, len(data))
	n, err := bs.ReadAt(ctx, payloadID, nil, buf, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if n != len(data) {
		t.Fatalf("ReadAt returned %d bytes, expected %d", n, len(data))
	}
	if string(buf) != string(data) {
		t.Fatalf("data mismatch: got %q, want %q", buf, data)
	}
}

// TestReadAt_CacheDisabled verifies the engine works with the Null
// Object cache (ReadBufferBytes=0). Plan 09 WARN-8: there are no
// nil-checks anywhere — the cache is always callable.
func TestReadAt_CacheDisabled(t *testing.T) {
	bs := newTestEngine(t, 0, 0)

	ctx := context.Background()
	payloadID := "no-cache-test"
	data := []byte("works without cache")

	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	buf := make([]byte, len(data))
	n, err := bs.ReadAt(ctx, payloadID, nil, buf, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if n != len(data) {
		t.Fatalf("ReadAt returned %d bytes, expected %d", n, len(data))
	}

	// Null Object substituted; cache is callable but a no-op.
	if _, ok := bs.cache.(nullCache); !ok {
		t.Fatal("expected nullCache when ReadBufferBytes=0")
	}
}

// TestReadAt_InvokesCacheOnRead — Task 3 behavior 1.
// engine.ReadAt invokes cache.OnRead with the BlockRef hashes and a
// fileSize derived from max(Offset+Size) after a successful read.
//
// We swap in the recording cache AFTER WriteAt so the writer's
// tracker-reset OnRead(nil) doesn't pollute the count we want to
// assert on.
func TestReadAt_InvokesCacheOnRead(t *testing.T) {
	bs := newTestEngine(t, 0, 0) // start with nullCache

	ctx := context.Background()
	payloadID := "onread-test"
	data := []byte("hello onread")

	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Swap in recording cache after the write so we observe only the
	// ReadAt path's OnRead invocation.
	rec := &recordingCache{}
	bs.cache = rec

	// Pass a non-empty []BlockRef so OnRead fires with hashes.
	refs := []blockstore.BlockRef{
		{Hash: hashN(0xAA), Offset: 0, Size: uint32(len(data))},
	}
	buf := make([]byte, len(data))
	if _, err := bs.ReadAt(ctx, payloadID, refs, buf, 0); err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.onReadCalls != 1 {
		t.Fatalf("expected 1 OnRead call, got %d", rec.onReadCalls)
	}
	if rec.lastPayloadID != payloadID {
		t.Fatalf("OnRead payloadID = %q, want %q", rec.lastPayloadID, payloadID)
	}
	if len(rec.lastHashes) != 1 || rec.lastHashes[0] != refs[0].Hash {
		t.Fatalf("OnRead hashes mismatch: got %v", rec.lastHashes)
	}
	if rec.lastFileSize != uint64(len(data)) {
		t.Fatalf("OnRead fileSize = %d, want %d", rec.lastFileSize, len(data))
	}
}

// TestEngine_NullCache_NoNilChecks — Task 3 behavior 2.
// Construct BlockStore with nil/zero ReadBufferBytes; constructor
// substitutes nullCache; ReadAt + WriteAt + Truncate + Delete run
// without panicking. The "no nil-checks" enforcement is asserted by
// the package-level grep in the done criteria; this test verifies the
// runtime side.
func TestEngine_NullCache_NoNilChecks(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	if _, ok := bs.cache.(nullCache); !ok {
		t.Fatal("constructor must substitute nullCache when budget=0")
	}

	ctx := context.Background()
	payloadID := "nil-check-test"
	data := []byte("just don't panic")

	// All these unconditionally call bs.cache.* internally.
	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt panicked or errored: %v", err)
	}
	buf := make([]byte, len(data))
	if _, err := bs.ReadAt(ctx, payloadID, nil, buf, 0); err != nil {
		t.Fatalf("ReadAt panicked or errored: %v", err)
	}
	if _, err := bs.Truncate(ctx, payloadID, nil, uint64(len(data))); err != nil {
		t.Fatalf("Truncate panicked or errored: %v", err)
	}
	if err := bs.Delete(ctx, payloadID, nil); err != nil {
		t.Fatalf("Delete panicked or errored: %v", err)
	}
}

// TestEngine_NullCache_Methods_NoOp — Task 3 behavior 3.
// Direct unit test: every nullCache method is a safe no-op.
func TestEngine_NullCache_Methods_NoOp(t *testing.T) {
	var c CacheInterface = nullCache{}

	got, ok := c.Get(hashN(1))
	if got != nil || ok {
		t.Fatalf("nullCache.Get must return (nil, false); got (%v, %v)", got, ok)
	}
	c.Put(hashN(1), []byte("ignored"))
	c.OnRead("p", []blockstore.ContentHash{hashN(1)}, 0)
	c.InvalidateFile("p", []blockstore.ContentHash{hashN(1)})
	stats := c.Stats()
	if stats != (CacheStats{}) {
		t.Fatalf("nullCache.Stats must be zero-value; got %+v", stats)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("nullCache.Close must return nil; got %v", err)
	}
}

// recordingCache is a CacheInterface impl used by Task 3 tests to
// observe engine -> cache calls.
type recordingCache struct {
	mu              sync.Mutex
	onReadCalls     int
	invalidateCalls int
	lastPayloadID   string
	lastHashes      []blockstore.ContentHash
	lastFileSize    uint64
	lastInvHashes   []blockstore.ContentHash
	closed          atomic.Bool
}

func (r *recordingCache) Get(blockstore.ContentHash) ([]byte, bool) { return nil, false }
func (r *recordingCache) Put(blockstore.ContentHash, []byte)        {}
func (r *recordingCache) OnRead(payloadID string, hashes []blockstore.ContentHash, fileSize uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onReadCalls++
	r.lastPayloadID = payloadID
	r.lastHashes = append([]blockstore.ContentHash(nil), hashes...)
	r.lastFileSize = fileSize
}
func (r *recordingCache) InvalidateFile(payloadID string, removed []blockstore.ContentHash) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.invalidateCalls++
	r.lastPayloadID = payloadID
	r.lastInvHashes = append([]blockstore.ContentHash(nil), removed...)
}
func (r *recordingCache) Stats() CacheStats { return CacheStats{} }
func (r *recordingCache) Close() error      { r.closed.Store(true); return nil }

// TestClose_ClosesCache verifies BlockStore.Close calls the cache's
// Close. Uses a recording fake so we can observe it.
func TestClose_ClosesCache(t *testing.T) {
	localStore := memory.New()
	fbs := &stubFileBlockStore{}
	syncer := NewSyncer(localStore, nil, fbs, DefaultConfig())

	bs, err := New(Config{
		Local:  localStore,
		Remote: nil,
		Syncer: syncer,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	rec := &recordingCache{}
	bs.cache = rec

	if err := bs.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if !rec.closed.Load() {
		t.Fatal("BlockStore.Close must invoke cache.Close")
	}
}

// TestCopyPayload_EmptySource verifies CopyPayload handles empty source gracefully.
// Phase 12 Plan 07: with an empty []BlockRef the engine returns nil
// without invoking the coordinator (no work to do).
func TestCopyPayload_EmptySource(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	ctx := context.Background()

	dst, err := bs.CopyPayload(ctx, "nonexistent", "dst", nil)
	if err != nil {
		t.Fatalf("CopyPayload should succeed for empty source, got: %v", err)
	}
	if len(dst) != 0 {
		t.Fatalf("CopyPayload returned %d blocks, expected 0", len(dst))
	}
}

// newFSTestEngine constructs an engine.BlockStore backed by an on-disk FSStore
// rooted at a temp dir, so .blk files can be observed on the filesystem.
// Returns the engine and the base directory holding the FSStore's .blk files.
func newFSTestEngine(t *testing.T) (*BlockStore, string) {
	t.Helper()

	tmpDir := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	localStore, err := fs.New(tmpDir, 100*1024*1024, 16*1024*1024, ms)
	if err != nil {
		t.Fatalf("fs.New failed: %v", err)
	}

	syncer := NewSyncer(localStore, nil, ms, DefaultConfig())

	bs, err := New(Config{
		Local:           localStore,
		Remote:          nil,
		Syncer:          syncer,
		FileBlockStore:  ms,
		ReadBufferBytes: 0,
		PrefetchWorkers: 0,
	})
	if err != nil {
		t.Fatalf("engine.New failed: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("engine.Start failed: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	return bs, tmpDir
}

// countBlkFiles walks dir and returns the number of .blk files present.
func countBlkFiles(t *testing.T, dir string) int {
	t.Helper()

	count := 0
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() && filepath.Ext(info.Name()) == ".blk" {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s failed: %v", dir, err)
	}
	return count
}

// TestEngineDelete_RemovesBlockFiles verifies engine.Delete removes on-disk
// .blk files for the payloadID. Regression test for TD-02c: before the fix,
// Delete only called EvictMemory + syncer.Delete, leaving .blk files orphaned
// on local disk.
func TestEngineDelete_RemovesBlockFiles(t *testing.T) {
	bs, baseDir := newFSTestEngine(t)
	ctx := context.Background()
	payloadID := "export/td-02c/test.bin"

	// Write data spanning 2 blocks so at least 2 .blk files land on disk.
	data := make([]byte, int(blockstore.BlockSize)+4096)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if _, err := bs.WriteAt(ctx, payloadID, nil, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Flush dirty in-memory blocks to disk as .blk files.
	if _, err := bs.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	// Sanity check: at least one .blk file must exist before Delete.
	if got := countBlkFiles(t, baseDir); got < 1 {
		t.Fatalf("expected >=1 .blk file before Delete, got %d", got)
	}

	// Delete the payload.
	if err := bs.Delete(ctx, payloadID, nil); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// After Delete, zero .blk files must remain for the deleted payload.
	if got := countBlkFiles(t, baseDir); got != 0 {
		t.Fatalf("expected 0 .blk files after Delete, got %d (TD-02c regression)", got)
	}
}
