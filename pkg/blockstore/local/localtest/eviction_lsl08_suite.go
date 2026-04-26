package localtest

import (
	"bytes"
	"context"
	"os"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	"lukechampine.com/blake3"
)

// EvictionLSL08Factory constructs an *fs.FSStore configured for LSL-08
// eviction tests: small maxDisk so eviction is easy to trigger, LRU
// retention policy, eviction enabled. Tests register tempdir cleanup.
//
// Returns *fs.FSStore (not local.LocalStore) because the LSL-08 surface
// (StoreChunk, ReadChunk, ContentHash chunk paths) lives on the concrete
// FS type — mirroring the AppendLogFactory contract.
type EvictionLSL08Factory func(t *testing.T) *fs.FSStore

// RunEvictionLSL08Suite dispatches the five LSL-08 D-27 scenarios:
//   - eviction_lru_order
//   - eviction_no_fbs_calls (the load-bearing assertion)
//   - eviction_re_fetch_after_evict
//   - eviction_concurrent_writes_safe
//   - eviction_lru_seed_on_startup
//
// The factory is expected to set policy=LRU, eviction=true, and a small
// maxDisk so the tests can induce eviction without giant payloads.
func RunEvictionLSL08Suite(t *testing.T, factory EvictionLSL08Factory) {
	t.Run("eviction_lru_order", func(t *testing.T) { testLSL08LRUOrder(t, factory) })
	t.Run("eviction_no_fbs_calls", func(t *testing.T) { testLSL08NoFBSCalls(t, factory) })
	t.Run("eviction_re_fetch_after_evict", func(t *testing.T) { testLSL08RefetchAfterEvict(t, factory) })
	t.Run("eviction_concurrent_writes_safe", func(t *testing.T) { testLSL08ConcurrentWritesSafe(t, factory) })
	t.Run("eviction_lru_seed_on_startup", func(t *testing.T) { testLSL08LRUSeedOnStartup(t, factory) })
}

// hashChunk hashes data with BLAKE3-256 and returns the ContentHash.
func hashChunk(data []byte) blockstore.ContentHash {
	sum := blake3.Sum256(data)
	var h blockstore.ContentHash
	copy(h[:], sum[:])
	return h
}

func testLSL08LRUOrder(t *testing.T, factory EvictionLSL08Factory) {
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
	// Need enough space to force eviction (factory's maxDisk is 600;
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

func testLSL08NoFBSCalls(t *testing.T, factory EvictionLSL08Factory) {
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

func testLSL08RefetchAfterEvict(t *testing.T, factory EvictionLSL08Factory) {
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

func testLSL08ConcurrentWritesSafe(t *testing.T, factory EvictionLSL08Factory) {
	bc := factory(t)
	const N = 50
	hashes := make([]blockstore.ContentHash, N)
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

func testLSL08LRUSeedOnStartup(t *testing.T, factory EvictionLSL08Factory) {
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
