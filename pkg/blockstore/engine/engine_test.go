package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local/memory"
)

// stubFileBlockStore is an in-memory blockstore.EngineFileBlockStore
// for the engine test harness. It carries the minimum read-path
// surface the post-Phase-18 CAS engine consumes: GetFileBlock (used by
// the syncer's resolveFileBlock) and Put (populated by the memory
// local store's chunk emitter via engine.New's wiring). Mutating
// methods (Delete, IncrementRefCount, DecrementRefCount) maintain
// just enough state for the cascade tests; ListPending /
// ListFileBlocks return empty / per-payload subsets.
type stubFileBlockStore struct {
	mu     sync.Mutex
	blocks map[string]*blockstore.FileBlock
}

func newStubFileBlockStore() *stubFileBlockStore {
	return &stubFileBlockStore{blocks: make(map[string]*blockstore.FileBlock)}
}

func (s *stubFileBlockStore) GetByHash(_ context.Context, h blockstore.ContentHash) (*blockstore.FileBlock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, fb := range s.blocks {
		if fb.Hash == h {
			return fb, nil
		}
	}
	return nil, nil
}
func (s *stubFileBlockStore) Put(_ context.Context, block *blockstore.FileBlock) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blocks == nil {
		s.blocks = make(map[string]*blockstore.FileBlock)
	}
	// Defensive copy to avoid aliasing into caller state.
	cp := *block
	s.blocks[block.ID] = &cp
	return nil
}
func (s *stubFileBlockStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.blocks, id)
	return nil
}
func (s *stubFileBlockStore) IncrementRefCount(_ context.Context, _ string) error {
	return nil
}
func (s *stubFileBlockStore) DecrementRefCount(_ context.Context, _ string) (uint32, error) {
	return 0, nil
}
func (s *stubFileBlockStore) ListPending(_ context.Context, _ time.Duration, _ int) ([]*blockstore.FileBlock, error) {
	return nil, nil
}

// Engine-internal surface (kept off the public FileBlockStore).
func (s *stubFileBlockStore) GetFileBlock(_ context.Context, id string) (*blockstore.FileBlock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fb, ok := s.blocks[id]
	if !ok {
		return nil, blockstore.ErrFileBlockNotFound
	}
	return fb, nil
}
func (s *stubFileBlockStore) ListFileBlocks(_ context.Context, payloadID string) ([]*blockstore.FileBlock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := payloadID + "/"
	var out []*blockstore.FileBlock
	for id, fb := range s.blocks {
		if len(id) >= len(prefix) && id[:len(prefix)] == prefix {
			out = append(out, fb)
		}
	}
	return out, nil
}

// newTestEngine creates an engine.BlockStore with memory local store, nil remote,
// optional cache budget and prefetch settings. Coordinator is left nil — tests
// that exercise Coordinator-dependent paths (CopyPayload/Delete/Truncate
// with non-empty BlockRef list) should use newTestEngineWithCoordinator.
func newTestEngine(t *testing.T, readBufferBytes int64, prefetchWorkers int) *BlockStore {
	t.Helper()
	localStore := memory.New()
	fbs := newStubFileBlockStore()
	syncer := NewSyncer(localStore, nil, fbs, DefaultConfig())

	bs, err := New(Config{
		Local:           localStore,
		Remote:          nil,
		Syncer:          syncer,
		FileBlockStore:  fbs,
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
	fbs := newStubFileBlockStore()
	syncer := NewSyncer(localStore, nil, fbs, DefaultConfig())

	bs, err := New(Config{
		Local:          localStore,
		Remote:         nil,
		Syncer:         syncer,
		FileBlockStore: fbs,
		Coordinator:    c,
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
	fbs := newStubFileBlockStore()
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

// No per-file block-file cleanup assertion lives here: the local store
// does not write legacy per-file block files (the unified CAS chunk
// store under blocks/<hh>/ is the only on-disk layout), so there is
// nothing to observe at this seam. End-to-end coverage of the
// engine.Delete refcount → GC path is provided by
// TestEngine_Delete_CascadesDeleteSynced in engine_delete_test.go and
// by the integration tests in syncer_test.go.
