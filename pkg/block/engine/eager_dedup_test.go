// — eager small-file dedup (Opt 4) unit tests.
//
// tryEagerSmallFileDedup is the file-level dedup fast-path for files at
// or below chunker.MinChunkSize. Hashes whole content in RAM
// computes the single-block ObjectID, consults FindByObjectID, and on
// hit short-circuits chunker + log + CAS write entirely. Sits BEFORE
// trySpeculativeFileLevelDedup in engine.Flush.
//
// These tests pin the contract
//
// -: threshold = chunker.MinChunkSize; files > threshold return
//     (false, nil) immediately without consulting FindByObjectID.
// -: hit path uses applyFileLevelDedupHit to honor
// + cache invalidation invariants identical to the
//     speculative path.
// -: on HIT, cache is populated with the hashed bytes (no extra
//     disk hop on subsequent reads).
//   - Defensive: empty data and nil-coordinator return (false, nil).

package engine

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/chunker"
	"lukechampine.com/blake3"
)

// recordingPutCache is a cacheInterface that records every Put so eager
// dedup tests can assert cache warming (the existing recordingCache
// has a no-op Put, which can't observe the warming).
type recordingPutCache struct {
	mu        sync.Mutex
	putCalls  int
	putHashes []block.ContentHash
	putData   map[block.ContentHash][]byte
}

func newRecordingPutCache() *recordingPutCache {
	return &recordingPutCache{putData: make(map[block.ContentHash][]byte)}
}

func (r *recordingPutCache) Get(h block.ContentHash) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.putData[h]
	if !ok {
		return nil, false
	}
	cp := append([]byte(nil), d...)
	return cp, true
}
func (r *recordingPutCache) Put(h block.ContentHash, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.putCalls++
	r.putHashes = append(r.putHashes, h)
	r.putData[h] = append([]byte(nil), data...)
}
func (r *recordingPutCache) OnRead(string, []block.ContentHash, uint64) {}
func (r *recordingPutCache) InvalidateFile(string, []block.ContentHash) {}
func (r *recordingPutCache) Stats() CacheStats                          { return CacheStats{} }
func (r *recordingPutCache) Close() error                               { return nil }

// hashContent mirrors blake3ContentHash from pkg/block/local/fs/rollup.go
// (private to that package). Tests need the same content-address hash
// so seeded ObjectIDs match what the production eager-dedup path
// computes from arbitrary test data.
func hashContent(data []byte) block.ContentHash {
	var h block.ContentHash
	sum := blake3.Sum256(data)
	copy(h[:], sum[:])
	return h
}

// singleBlockObjectID computes the ObjectID a small file would produce
// under eager dedup: BLAKE3(prefix || h) where h = BLAKE3(data).
func singleBlockObjectID(data []byte) block.ObjectID {
	h := hashContent(data)
	return block.ComputeObjectID([]block.BlockRef{
		{Hash: h, Offset: 0, Size: uint32(len(data))},
	})
}

// TestTryEagerSmallFileDedup_DataAboveThreshold_ReturnsFalse —
// files > chunker.MinChunkSize bypass the eager path and do NOT
// invoke FindByObjectID (avoids hashing a multi-MiB buffer for no win).
func TestTryEagerSmallFileDedup_DataAboveThreshold_ReturnsFalse(t *testing.T) {
	ctx := context.Background()
	m, fc := dedupTestSetup(t)

	// MinChunkSize + 1 byte = above threshold.
	data := []byte(strings.Repeat("x", chunker.MinChunkSize+1))

	hit, err := m.tryEagerSmallFileDedup(ctx, "pid", data)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if hit {
		t.Errorf("hit=true on data > MinChunkSize; want false (D-13 threshold)")
	}
	if got := len(fc.findCalls); got != 0 {
		t.Errorf("findCalls=%d on data > MinChunkSize; want 0 (D-13: avoid the hash)", got)
	}
}

// TestTryEagerSmallFileDedup_DataAtThreshold_Proceeds —
// files at exactly chunker.MinChunkSize trigger the eager path
// (inclusive upper bound; <= MinChunkSize). FindByObjectID is invoked.
func TestTryEagerSmallFileDedup_DataAtThreshold_Proceeds(t *testing.T) {
	ctx := context.Background()
	m, fc := dedupTestSetup(t)

	data := []byte(strings.Repeat("x", chunker.MinChunkSize))

	hit, err := m.tryEagerSmallFileDedup(ctx, "pid", data)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if hit {
		t.Errorf("hit=true on un-seeded ObjectID; want false (miss path)")
	}
	if got := len(fc.findCalls); got != 1 {
		t.Errorf("findCalls=%d at threshold; want 1 (D-13: at-threshold triggers)", got)
	}
	want := singleBlockObjectID(data)
	if len(fc.findCalls) == 1 && fc.findCalls[0] != want {
		t.Errorf("FindByObjectID arg=%s; want provisional %s",
			fc.findCalls[0].String(), want.String())
	}
}

// TestTryEagerSmallFileDedup_Hit_ReturnsTrue —
// seeded ObjectID hit short-circuits with hit=true; the shared
// finalize machinery (applyFileLevelDedupHit) was invoked, evidenced
// by PersistFileBlocks recording the provisional ObjectID + target
// blocks, and IncrementRefCount being called on each target hash.
func TestTryEagerSmallFileDedup_Hit_ReturnsTrue(t *testing.T) {
	ctx := context.Background()
	m, fc := dedupTestSetup(t)

	data := []byte("small file content")
	provisional := singleBlockObjectID(data)

	// Seed: a previously-quiesced file with the same single-block hash
	// exists in metadata. Target's BlockRef list has one ref with the
	// same content hash (the only shape that produces the same ObjectID).
	contentHash := hashContent(data)
	target := []block.BlockRef{
		{Hash: contentHash, Offset: 0, Size: uint32(len(data))},
	}
	fc.objectIDHits[provisional] = target

	hit, err := m.tryEagerSmallFileDedup(ctx, "pid", data)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !hit {
		t.Fatalf("hit=false on seeded ObjectID; want true (D-14)")
	}

	// applyFileLevelDedupHit fingerprint: PersistFileBlocks recorded once
	// with target blocks + provisional ObjectID.
	if got := len(fc.persistCalls); got != 1 {
		t.Fatalf("PersistFileBlocks calls=%d; want 1 (D-14 single-txn write)", got)
	}
	if fc.persistCalls[0].objectID != provisional {
		t.Errorf("PersistFileBlocks objectID=%s; want provisional %s",
			fc.persistCalls[0].objectID.String(), provisional.String())
	}
	// IncrementRefCount called once on the target hash (step 1).
	if got := len(fc.incHashes); got != 1 {
		t.Errorf("IncrementRefCount calls=%d; want 1 (one target hash)", got)
	}
	if len(fc.incHashes) == 1 && fc.incHashes[0] != contentHash {
		t.Errorf("IncrementRefCount arg=%s; want target hash %s",
			fc.incHashes[0].String(), contentHash.String())
	}
}

// TestTryEagerSmallFileDedup_Miss_ReturnsFalse —
// Miss path: FindByObjectID returns nil; eager returns (false, nil)
// without touching the finalize machinery (PersistFileBlocks /
// IncrementRefCount NOT called).
func TestTryEagerSmallFileDedup_Miss_ReturnsFalse(t *testing.T) {
	ctx := context.Background()
	m, fc := dedupTestSetup(t)

	data := []byte("never-before-seen content")

	hit, err := m.tryEagerSmallFileDedup(ctx, "pid", data)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if hit {
		t.Errorf("hit=true on miss; want false")
	}
	if got := len(fc.findCalls); got != 1 {
		t.Errorf("findCalls=%d; want 1 (miss path still issues the lookup)", got)
	}
	if got := len(fc.persistCalls); got != 0 {
		t.Errorf("PersistFileBlocks calls=%d on miss; want 0", got)
	}
	if got := len(fc.incHashes); got != 0 {
		t.Errorf("IncrementRefCount calls=%d on miss; want 0", got)
	}
}

// TestTryEagerSmallFileDedup_Hit_PopulatesCache —
// on HIT the engine Cache is populated with the in-RAM bytes via
// Cache.Put(h, data). The MISS case is covered by the rollup path's
// OnChunkComplete wiring — not exercised here.
func TestTryEagerSmallFileDedup_Hit_PopulatesCache(t *testing.T) {
	ctx := context.Background()
	fc := newFakeCoordinator()
	bs := newTestEngineWithCoordinator(t, fc)
	rec := newRecordingPutCache()
	bs.cache = rec
	m := bs.syncer

	data := []byte("cache me on hit")
	contentHash := hashContent(data)
	provisional := singleBlockObjectID(data)
	fc.objectIDHits[provisional] = []block.BlockRef{
		{Hash: contentHash, Offset: 0, Size: uint32(len(data))},
	}

	hit, err := m.tryEagerSmallFileDedup(ctx, "pid", data)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !hit {
		t.Fatalf("hit=false; want true (seeded ObjectID)")
	}

	// Capture state under lock, then assert outside to avoid deadlocking
	// against Get (which also acquires rec.mu).
	rec.mu.Lock()
	calls := rec.putCalls
	hashes := append([]block.ContentHash(nil), rec.putHashes...)
	rec.mu.Unlock()

	if calls != 1 {
		t.Fatalf("Cache.Put calls=%d; want 1 (D-16: warm cache on eager hit)", calls)
	}
	if hashes[0] != contentHash {
		t.Errorf("Cache.Put hash=%s; want content hash %s",
			hashes[0].String(), contentHash.String())
	}
	got, ok := rec.Get(contentHash)
	if !ok {
		t.Fatalf("Cache.Get after Put returned not-found")
	}
	if string(got) != string(data) {
		t.Errorf("Cache.Put data mismatch: got %q, want %q", got, data)
	}
}

// TestTryEagerSmallFileDedup_EmptyData_ReturnsFalse —
// Defensive: zero-length input returns (false, nil) without computing a
// hash. Empty files have ObjectID = BLAKE3(prefix) (the canonical empty-
// file constant), but the eager path opts out — the speculative path
// has its own gates for the empty case.
func TestTryEagerSmallFileDedup_EmptyData_ReturnsFalse(t *testing.T) {
	ctx := context.Background()
	m, fc := dedupTestSetup(t)

	hit, err := m.tryEagerSmallFileDedup(ctx, "pid", nil)
	if err != nil {
		t.Fatalf("err on nil data: %v", err)
	}
	if hit {
		t.Errorf("hit=true on nil data; want false (defensive gate)")
	}
	if got := len(fc.findCalls); got != 0 {
		t.Errorf("findCalls=%d on nil data; want 0 (defensive: skip the hash)", got)
	}

	hit, err = m.tryEagerSmallFileDedup(ctx, "pid", []byte{})
	if err != nil {
		t.Fatalf("err on empty slice: %v", err)
	}
	if hit {
		t.Errorf("hit=true on empty slice; want false (defensive gate)")
	}
	if got := len(fc.findCalls); got != 0 {
		t.Errorf("findCalls=%d on empty slice; want 0", got)
	}
}

// TestTryEagerSmallFileDedup_NilCoordinator_ReturnsFalse —
// Mirror trySpeculativeFileLevelDedup's nil-coordinator gate: with no
// MetadataCoordinator wired (test ergonomics), eager dedup is a no-op
// short-circuit returning (false, nil).
func TestTryEagerSmallFileDedup_NilCoordinator_ReturnsFalse(t *testing.T) {
	ctx := context.Background()
	bs := newTestEngine(t, 0, 0) // coordinator left nil
	m := bs.syncer

	data := []byte("any content")

	hit, err := m.tryEagerSmallFileDedup(ctx, "pid", data)
	if err != nil {
		t.Fatalf("err on nil coordinator: %v", err)
	}
	if hit {
		t.Errorf("hit=true with nil coordinator; want false (nil-coordinator gate)")
	}
}
