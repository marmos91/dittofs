package fs_test

import (
	"bytes"
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestFSStore_EvictionLSL08Conformance runs the eviction
// conformance scenarios against *fs.FSStore. The factory wires a small
// disk limit (so eviction is easy to trigger) and a counting
// FileBlockStore (so the no-FBS-call assertion can probe).
//
// -06 inlined the scenarios here from
// pkg/block/local/localtest/eviction_lsl08_suite.go (deleted in
// this plan). The scenarios remain fs-specific — they exercise CAS
// chunk paths, LRU eviction policy, and the FBS-call invariant —
// none of which appear on the unified BlockStore contract.
func TestFSStore_EvictionLSL08Conformance(t *testing.T) {
	t.Run("eviction_lru_order", func(t *testing.T) { testLSL08LRUOrder(t, lsl08Factory) })
	t.Run("eviction_no_fbs_calls", func(t *testing.T) { testLSL08NoFBSCalls(t, lsl08Factory) })
	t.Run("eviction_re_fetch_after_evict", func(t *testing.T) { testLSL08RefetchAfterEvict(t, lsl08Factory) })
	t.Run("eviction_concurrent_writes_safe", func(t *testing.T) { testLSL08ConcurrentWritesSafe(t, lsl08Factory) })
	t.Run("eviction_lru_seed_on_startup", func(t *testing.T) { testLSL08LRUSeedOnStartup(t, lsl08Factory) })
}

// lsl08Factory constructs an *fs.FSStore configured for eviction
// tests: small maxDisk so eviction is easy to trigger, LRU retention
// policy, eviction enabled. Tests register tempdir cleanup.
func lsl08Factory(t *testing.T) *fs.FSStore {
	t.Helper()
	dir := t.TempDir()
	mds := memmeta.NewMemoryMetadataStoreWithDefaults()
	spy := &countingFBSWrapper{inner: mds}
	bc, err := fs.NewWithOptions(dir, 600, 1<<30, spy, fs.FSStoreOptions{})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	bc.SetEvictionEnabled(true)
	bc.SetRetentionPolicy(block.RetentionLRU, 0)
	t.Cleanup(func() { _ = bc.Close() })
	return bc
}

// hashChunk hashes data with BLAKE3-256 and returns the ContentHash.
func hashChunk(data []byte) block.ContentHash {
	sum := blake3.Sum256(data)
	var h block.ContentHash
	copy(h[:], sum[:])
	return h
}

func testLSL08LRUOrder(t *testing.T, factory func(t *testing.T) *fs.FSStore) {
	bc := factory(t)
	ctx := context.Background()
	a := bytes.Repeat([]byte{0x01}, 200)
	b := bytes.Repeat([]byte{0x02}, 200)
	hA := hashChunk(a)
	hB := hashChunk(b)
	if err := bc.StoreChunk(ctx, hA, a); err != nil {
		t.Fatalf("StoreChunk a: %v", err)
	}
	if err := bc.StoreChunk(ctx, hB, b); err != nil {
		t.Fatalf("StoreChunk b: %v", err)
	}
	// Read b to promote it; a is now LRU back.
	if _, err := bc.ReadChunk(ctx, hB); err != nil {
		t.Fatalf("ReadChunk b: %v", err)
	}
	// Need enough space to force eviction (factory's maxDisk is 600
	// existing diskUsed = 400; ask for 300 -> 700 > 600 -> evict 1 chunk
	// of 200, free to 500, then stop).
	if err := bc.EnsureSpaceForTest(ctx, 300); err != nil {
		t.Fatalf("EnsureSpaceForTest: %v", err)
	}
	// a should be gone, b should remain.
	if _, err := os.Stat(bc.ChunkPathForTest(hA)); !os.IsNotExist(err) {
		t.Errorf("LRU back chunk should have been evicted (stat err=%v)", err)
	}
	if _, err := os.Stat(bc.ChunkPathForTest(hB)); err != nil {
		t.Errorf("LRU front chunk should remain: %v", err)
	}
}

func testLSL08NoFBSCalls(t *testing.T, factory func(t *testing.T) *fs.FSStore) {
	bc := factory(t)
	// The factory wires a counting FileBlockStore so NoFBSCallsForTest
	// can read and reset the counter.
	ctx := context.Background()
	data := bytes.Repeat([]byte{0x10}, 200)
	if err := bc.StoreChunk(ctx, hashChunk(data), data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}
	bc.ResetFBSCallCounterForTest()
	if err := bc.EnsureSpaceForTest(ctx, 200); err != nil {
		t.Fatalf("EnsureSpaceForTest: %v", err)
	}
	if n := bc.FBSCallCountForTest(); n != 0 {
		t.Errorf("ensureSpace consulted FileBlockStore %d times — LSL-08 violation", n)
	}
}

func testLSL08RefetchAfterEvict(t *testing.T, factory func(t *testing.T) *fs.FSStore) {
	bc := factory(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte{0x20}, 200)
	h := hashChunk(data)
	if err := bc.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}
	// Ask for more than maxDisk-currentUsed to force eviction.
	if err := bc.EnsureSpaceForTest(ctx, 500); err != nil {
		t.Fatalf("EnsureSpaceForTest: %v", err)
	}
	if _, err := bc.ReadChunk(ctx, h); err == nil {
		t.Errorf("expected ErrChunkNotFound after evict")
	}
}

func testLSL08ConcurrentWritesSafe(t *testing.T, factory func(t *testing.T) *fs.FSStore) {
	bc := factory(t)
	const N = 50
	hashes := make([]block.ContentHash, N)
	var hashesMu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < N; i++ {
			data := bytes.Repeat([]byte{byte(i)}, 200)
			h := hashChunk(data)
			if err := bc.StoreChunk(context.Background(), h, data); err != nil {
				return
			}
			hashesMu.Lock()
			hashes[i] = h
			hashesMu.Unlock()
		}
	}()
	for {
		select {
		case <-done:
			_ = bc.EnsureSpaceForTest(context.Background(), 100)
			return
		default:
		}
		for i := 0; i < N; i++ {
			hashesMu.Lock()
			h := hashes[i]
			hashesMu.Unlock()
			if h.IsZero() {
				continue
			}
			_, _ = bc.ReadChunk(context.Background(), h)
		}
	}
}

func testLSL08LRUSeedOnStartup(t *testing.T, factory func(t *testing.T) *fs.FSStore) {
	bc := factory(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte{0x30}, 200)
	h := hashChunk(data)
	if err := bc.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}
	// Re-seed the LRU as if we just opened the store fresh — the
	// already-on-disk chunk should be picked up.
	if !bc.SeedLRUFromDiskForTest() {
		t.Skip("factory does not support reopen — seed-on-startup is implicit at New()")
	}
	// Ask for more than is available to force eviction of the seeded chunk.
	if err := bc.EnsureSpaceForTest(ctx, 500); err != nil {
		t.Fatalf("EnsureSpaceForTest after restart: %v", err)
	}
	if _, err := os.Stat(bc.ChunkPathForTest(h)); !os.IsNotExist(err) {
		t.Errorf("seeded chunk should have been evicted post-restart (stat err=%v)", err)
	}
}

// countingFBSWrapper is a thin call-counting wrapper around a
// block.EngineFileBlockStore. Mirrors the package-internal
// countingFileBlockStore but lives here so it can be wired into the
// conformance factory. Satisfies fs.FBSCounter via the exported
// ResetCount/TotalCount methods.
//
// wraps the wider engine-internal interface
// (the 6 narrowed FileBlockStore methods plus the engine-internal
// GetFileBlock + ListFileBlocks).
type countingFBSWrapper struct {
	inner   block.EngineFileBlockStore
	counter int
}

func (c *countingFBSWrapper) ResetCount()     { c.counter = 0 }
func (c *countingFBSWrapper) TotalCount() int { return c.counter }

func (c *countingFBSWrapper) GetFileBlock(ctx context.Context, id string) (*block.FileBlock, error) {
	c.counter++
	return c.inner.GetFileBlock(ctx, id)
}
func (c *countingFBSWrapper) Put(ctx context.Context, b *block.FileBlock) error {
	c.counter++
	return c.inner.Put(ctx, b)
}
func (c *countingFBSWrapper) Delete(ctx context.Context, id string) error {
	c.counter++
	return c.inner.Delete(ctx, id)
}
func (c *countingFBSWrapper) IncrementRefCount(ctx context.Context, id string) error {
	c.counter++
	return c.inner.IncrementRefCount(ctx, id)
}
func (c *countingFBSWrapper) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	c.counter++
	return c.inner.DecrementRefCount(ctx, id)
}
func (c *countingFBSWrapper) DecrementRefCountAndReap(ctx context.Context, id string) (uint32, error) {
	c.counter++
	return c.inner.DecrementRefCountAndReap(ctx, id)
}
func (c *countingFBSWrapper) AddRef(ctx context.Context, h block.ContentHash, payloadID string, ref block.BlockRef) error {
	c.counter++
	return c.inner.AddRef(ctx, h, payloadID, ref)
}
func (c *countingFBSWrapper) GetByHash(ctx context.Context, h block.ContentHash) (*block.FileBlock, error) {
	c.counter++
	return c.inner.GetByHash(ctx, h)
}
func (c *countingFBSWrapper) ListPending(ctx context.Context, olderThan time.Duration, limit int) ([]*block.FileBlock, error) {
	c.counter++
	return c.inner.ListPending(ctx, olderThan, limit)
}
func (c *countingFBSWrapper) ListFileBlocks(ctx context.Context, payloadID string) ([]*block.FileBlock, error) {
	c.counter++
	return c.inner.ListFileBlocks(ctx, payloadID)
}
