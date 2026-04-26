package fs

import (
	"bytes"
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"lukechampine.com/blake3"
)

// newTestCacheWithDiskLimit creates an FSStore with a specified max disk size
// for eviction testing. Uses in-memory FileBlockStore.
func newTestCacheWithDiskLimit(t *testing.T, maxDisk int64) *FSStore {
	t.Helper()
	dir := t.TempDir()
	blockStore := memory.NewMemoryMetadataStoreWithDefaults()
	bc, err := New(dir, maxDisk, 256*1024*1024, blockStore)
	if err != nil {
		t.Fatalf("failed to create local store: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	bc.Start(ctx)
	t.Cleanup(func() {
		cancel()
		_ = bc.Close()
	})
	return bc
}

// hashBytes returns the BLAKE3 ContentHash of data.
func hashBytes(data []byte) blockstore.ContentHash {
	sum := blake3.Sum256(data)
	var h blockstore.ContentHash
	copy(h[:], sum[:])
	return h
}

// storeChunk writes data through StoreChunk and returns the resulting hash.
// Used by LSL-08 tests to seed the LRU via the canonical write path.
func storeChunk(t *testing.T, bc *FSStore, data []byte) blockstore.ContentHash {
	t.Helper()
	h := hashBytes(data)
	if err := bc.StoreChunk(context.Background(), h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}
	return h
}

// ============================================================================
// Access Tracker Tests (retained — exercise unrelated bookkeeping)
// ============================================================================

func TestAccessTracker_Touch(t *testing.T) {
	at := newAccessTracker()
	before := time.Now()
	at.Touch("file1")
	after := time.Now()
	lastAccess := at.LastAccess("file1")
	if lastAccess.Before(before) || lastAccess.After(after) {
		t.Errorf("expected lastAccess between %v and %v, got %v", before, after, lastAccess)
	}
}

func TestAccessTracker_LastAccess_ZeroForUntouched(t *testing.T) {
	at := newAccessTracker()
	lastAccess := at.LastAccess("never-touched")
	if !lastAccess.IsZero() {
		t.Errorf("expected zero time for untouched file, got %v", lastAccess)
	}
}

func TestAccessTracker_Remove(t *testing.T) {
	at := newAccessTracker()
	at.Touch("file1")
	at.Remove("file1")
	if !at.LastAccess("file1").IsZero() {
		t.Error("expected zero time after Remove")
	}
}

// ============================================================================
// LSL-08 Eviction Tests (in-process LRU keyed by ContentHash)
// ============================================================================

// TestLSL08_PinMode_NeverEvicts asserts ensureSpace never evicts in pin mode
// even when the LRU has candidates and the disk is over budget.
func TestLSL08_PinMode_NeverEvicts(t *testing.T) {
	bc := newTestCacheWithDiskLimit(t, 1024)
	bc.SetEvictionEnabled(true)
	bc.SetRetentionPolicy(blockstore.RetentionPin, 0)

	// Seed the LRU with a 500B chunk.
	_ = storeChunk(t, bc, bytes.Repeat([]byte{0xAA}, 500))

	// diskUsed=500, maxDisk=1024, need 600 -> over limit.
	if err := bc.ensureSpace(context.Background(), 600); err != ErrDiskFull {
		t.Fatalf("expected ErrDiskFull for pin mode, got %v", err)
	}
}

// TestLSL08_LRU_OldestEvictedFirst asserts ensureSpace evicts the
// least-recently-touched chunk first.
func TestLSL08_LRU_OldestEvictedFirst(t *testing.T) {
	bc := newTestCacheWithDiskLimit(t, 1500)
	bc.SetEvictionEnabled(true)
	bc.SetRetentionPolicy(blockstore.RetentionLRU, 0)

	// Three 500B chunks. Insertion order = LRU order (oldest first inserted).
	hOld := storeChunk(t, bc, bytes.Repeat([]byte{0x01}, 500))
	hMid := storeChunk(t, bc, bytes.Repeat([]byte{0x02}, 500))
	hNew := storeChunk(t, bc, bytes.Repeat([]byte{0x03}, 500))

	// Touch hMid and hNew so hOld becomes the LRU back.
	bc.lruTouch(hMid, 500, bc.chunkPath(hMid))
	bc.lruTouch(hNew, 500, bc.chunkPath(hNew))

	// diskUsed=1500, maxDisk=1500, need 100 -> evict 1.
	if err := bc.ensureSpace(context.Background(), 100); err != nil {
		t.Fatalf("ensureSpace: %v", err)
	}

	// hOld file should be gone; hMid + hNew should remain.
	if _, err := os.Stat(bc.chunkPath(hOld)); !os.IsNotExist(err) {
		t.Errorf("oldest chunk should have been evicted (stat err=%v)", err)
	}
	if _, err := os.Stat(bc.chunkPath(hMid)); err != nil {
		t.Errorf("mid chunk should still exist: %v", err)
	}
	if _, err := os.Stat(bc.chunkPath(hNew)); err != nil {
		t.Errorf("new chunk should still exist: %v", err)
	}
}

// TestLSL08_NoFileBlockStoreCallsDuringEviction is the load-bearing assertion
// for D-27: ensureSpace MUST NOT consult FileBlockStore. Wraps the metadata
// store in a strict spy that fails the test on any call.
func TestLSL08_NoFileBlockStoreCallsDuringEviction(t *testing.T) {
	dir := t.TempDir()
	inner := memory.NewMemoryMetadataStoreWithDefaults()
	spy := newCountingFileBlockStore(inner)
	bc, err := New(dir, 1500, 256*1024*1024, spy)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	bc.SetEvictionEnabled(true)
	bc.SetRetentionPolicy(blockstore.RetentionLRU, 0)

	// Seed two chunks via StoreChunk (the canonical write path).
	_ = storeChunk(t, bc, bytes.Repeat([]byte{0x10}, 500))
	_ = storeChunk(t, bc, bytes.Repeat([]byte{0x11}, 500))

	before := spy.snapshot()
	if err := bc.ensureSpace(context.Background(), 600); err != nil {
		t.Fatalf("ensureSpace: %v", err)
	}
	after := spy.snapshot()
	delta := diffSnapshot(before, after)
	if delta != (fbsCallSnapshot{}) {
		t.Errorf("ensureSpace called FileBlockStore: %+v (LSL-08 invariant violated)", delta)
	}
}

// TestLSL08_RefetchAfterEvict asserts that after eviction, a subsequent
// ReadChunk surfaces ErrChunkNotFound — the engine layer is responsible
// for refetching from CAS.
func TestLSL08_RefetchAfterEvict(t *testing.T) {
	bc := newTestCacheWithDiskLimit(t, 600)
	bc.SetEvictionEnabled(true)
	bc.SetRetentionPolicy(blockstore.RetentionLRU, 0)

	hEvictable := storeChunk(t, bc, bytes.Repeat([]byte{0x20}, 500))
	// Force eviction by needing more space than is available.
	if err := bc.ensureSpace(context.Background(), 200); err != nil {
		t.Fatalf("ensureSpace: %v", err)
	}
	// Read should now miss.
	_, err := bc.ReadChunk(context.Background(), hEvictable)
	if err == nil {
		t.Errorf("expected miss after evict, got data")
	}
}

// TestLSL08_LRUSeededOnStartup asserts a freshly-constructed FSStore
// pointing at a directory with pre-existing chunks seeds the LRU and
// makes those chunks evictable.
func TestLSL08_LRUSeededOnStartup(t *testing.T) {
	dir := t.TempDir()
	mds := memory.NewMemoryMetadataStoreWithDefaults()

	// Create a first store, store a chunk, close.
	bc1, err := New(dir, 1<<30, 256*1024*1024, mds)
	if err != nil {
		t.Fatalf("New 1: %v", err)
	}
	hPersist := storeChunk(t, bc1, bytes.Repeat([]byte{0x30}, 500))
	_ = bc1.Close()

	// Reopen: New() should seed the LRU from disk so hPersist is evictable.
	bc2, err := New(dir, 600, 256*1024*1024, mds)
	if err != nil {
		t.Fatalf("New 2: %v", err)
	}
	t.Cleanup(func() { _ = bc2.Close() })
	bc2.SetEvictionEnabled(true)
	bc2.SetRetentionPolicy(blockstore.RetentionLRU, 0)
	bc2.diskUsed.Store(500) // simulate post-recovery diskUsed

	// Force eviction.
	if err := bc2.ensureSpace(context.Background(), 200); err != nil {
		t.Fatalf("ensureSpace: %v", err)
	}
	// Persistent chunk should now be gone.
	if _, err := os.Stat(bc2.chunkPath(hPersist)); !os.IsNotExist(err) {
		t.Errorf("seeded chunk should have been evicted (stat err=%v)", err)
	}
}

// TestLSL08_ConcurrentWritesSafe asserts the LRU is race-free under
// concurrent StoreChunk + ReadChunk + ensureSpace activity. Run with -race.
func TestLSL08_ConcurrentWritesSafe(t *testing.T) {
	bc := newTestCacheWithDiskLimit(t, 64*1024)
	bc.SetEvictionEnabled(true)
	bc.SetRetentionPolicy(blockstore.RetentionLRU, 0)

	const N = 50
	hashes := make([]blockstore.ContentHash, N)
	var hashesMu sync.Mutex
	done := make(chan struct{})

	// Writer goroutine — uses errors instead of t.Fatalf (race-safe).
	go func() {
		defer close(done)
		for i := 0; i < N; i++ {
			data := bytes.Repeat([]byte{byte(i)}, 1024)
			h := hashBytes(data)
			if err := bc.StoreChunk(context.Background(), h, data); err != nil {
				return
			}
			hashesMu.Lock()
			hashes[i] = h
			hashesMu.Unlock()
		}
	}()

	// Concurrent reader.
	for {
		select {
		case <-done:
			// Final eviction sweep.
			_ = bc.ensureSpace(context.Background(), 100)
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
