package snapshot_test

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
	memorystore "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// fbHash returns a deterministic ContentHash seeded by seed so tests get
// unique, ordered hashes without RNG flakiness. Mirrors gateHash in
// syncgate_test.go but lives in a separate file to avoid cross-file
// dependencies.
func fbHash(seed byte) blockstore.ContentHash {
	var h blockstore.ContentHash
	for i := range h {
		h[i] = seed + byte(i)
	}
	return h
}

// newMemStore returns a fresh in-memory MetadataStore for each test. The
// memory backend is the simplest backend that satisfies the MetadataStore
// interface and is the canonical fixture used elsewhere in this package
// for surface tests that don't depend on a specific backend's storage
// shape.
func newMemStore(t *testing.T) metadata.MetadataStore {
	t.Helper()
	return memorystore.NewMemoryMetadataStoreWithDefaults()
}

// putBlock inserts a single FileBlock with the given id and hash into
// store via the FileBlockStore.Put interface, which is the same surface
// the engine uses on the write path. Test data goes in via the same door
// as production data so the EnumerateFileBlocks invariants apply.
func putBlock(t *testing.T, store metadata.MetadataStore, id string, hash blockstore.ContentHash) {
	t.Helper()
	block := &metadata.FileBlock{
		ID:    id,
		Hash:  hash,
		State: blockstore.BlockStateRemote, // finalized so EnumerateFileBlocks emits it
	}
	if err := store.Put(context.Background(), block); err != nil {
		t.Fatalf("Put block %s: %v", id, err)
	}
}

// TestHashSetFromMetadataStore_Empty: a freshly-created store returns a
// non-nil HashSet with Len()==0 and a nil error. This is the post-Reset
// shape Runtime.RestoreSnapshot observes between step 5 (Reset) and step 6
// (Restore) — the post-verify walker must not allocate spurious hashes
// from an empty engine.
func TestHashSetFromMetadataStore_Empty(t *testing.T) {
	store := newMemStore(t)

	hs, err := snapshot.HashSetFromMetadataStore(context.Background(), store)
	if err != nil {
		t.Fatalf("HashSetFromMetadataStore(empty): %v", err)
	}
	if hs == nil {
		t.Fatal("HashSetFromMetadataStore(empty): got nil HashSet, want non-nil")
	}
	if hs.Len() != 0 {
		t.Errorf("HashSetFromMetadataStore(empty): Len()=%d, want 0", hs.Len())
	}
}

// TestHashSetFromMetadataStore_ThreeUniqueHashes: three distinct hashes
// across three FileBlock rows yield Len()==3. Each hash appears once in
// the result — the basic enumeration invariant.
func TestHashSetFromMetadataStore_ThreeUniqueHashes(t *testing.T) {
	store := newMemStore(t)

	hA := fbHash(0x10)
	hB := fbHash(0x20)
	hC := fbHash(0x30)
	putBlock(t, store, "blk-a", hA)
	putBlock(t, store, "blk-b", hB)
	putBlock(t, store, "blk-c", hC)

	hs, err := snapshot.HashSetFromMetadataStore(context.Background(), store)
	if err != nil {
		t.Fatalf("HashSetFromMetadataStore: %v", err)
	}
	if hs.Len() != 3 {
		t.Fatalf("HashSetFromMetadataStore: Len()=%d, want 3", hs.Len())
	}
	for _, want := range []blockstore.ContentHash{hA, hB, hC} {
		if !hs.Contains(want) {
			t.Errorf("HashSetFromMetadataStore: missing hash %x", want[:8])
		}
	}
}

// TestHashSetFromMetadataStore_Deduplication: two FileBlock rows that
// share a ContentHash collapse to one entry. Combined with two other
// unique hashes that gives Len()==3 from 4 input rows. This pins the
// dedup property D-24-14 calls out — manifest counts depend on it.
func TestHashSetFromMetadataStore_Deduplication(t *testing.T) {
	store := newMemStore(t)

	shared := fbHash(0x40)
	unique1 := fbHash(0x50)
	unique2 := fbHash(0x60)
	putBlock(t, store, "blk-1", shared)
	putBlock(t, store, "blk-2", shared) // duplicate hash, distinct ID
	putBlock(t, store, "blk-3", unique1)
	putBlock(t, store, "blk-4", unique2)

	hs, err := snapshot.HashSetFromMetadataStore(context.Background(), store)
	if err != nil {
		t.Fatalf("HashSetFromMetadataStore: %v", err)
	}
	if hs.Len() != 3 {
		t.Fatalf("HashSetFromMetadataStore: Len()=%d, want 3 (dedup)", hs.Len())
	}
	for _, want := range []blockstore.ContentHash{shared, unique1, unique2} {
		if !hs.Contains(want) {
			t.Errorf("HashSetFromMetadataStore: missing hash %x", want[:8])
		}
	}
}

// TestHashSetFromMetadataStore_CtxCancellation: a pre-cancelled ctx is
// surfaced through the EnumerateFileBlocks ctx.Done() check; the helper
// returns a wrapped ctx.Canceled. Verifies the cancellation contract the
// post-verify walker requires for ctx-bound Runtime shutdown paths.
func TestHashSetFromMetadataStore_CtxCancellation(t *testing.T) {
	store := newMemStore(t)
	// Populate enough blocks that the loop body has work to do — empty
	// stores short-circuit before the ctx.Err() check.
	for i := 0; i < 4; i++ {
		putBlock(t, store, "blk-"+string(rune('a'+i)), fbHash(byte(0x70+i)))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := snapshot.HashSetFromMetadataStore(ctx, store)
	if err == nil {
		t.Fatal("HashSetFromMetadataStore: expected error from cancelled ctx, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("HashSetFromMetadataStore: error %v is not context.Canceled", err)
	}
}

// TestHashSetFromMetadataStore_SkipsZeroHash: legacy pre-CAS FileBlock
// rows emit the zero ContentHash by interface contract. The helper must
// skip them so VerifyRemoteDurability does not get a phantom hash that
// can never resolve on the remote.
func TestHashSetFromMetadataStore_SkipsZeroHash(t *testing.T) {
	store := newMemStore(t)

	real1 := fbHash(0x80)
	real2 := fbHash(0x81)
	putBlock(t, store, "blk-real-1", real1)
	putBlock(t, store, "blk-real-2", real2)

	// Insert a pre-CAS row with zero hash. State must NOT be a finalized
	// state — Put allows the zero hash but the memory backend's hash
	// index only tracks finalized rows. EnumerateFileBlocks emits every
	// row regardless of state per the interface contract.
	var zero blockstore.ContentHash
	zeroBlock := &metadata.FileBlock{
		ID:   "blk-legacy-zero",
		Hash: zero,
		// Leave State at zero-value (Pending) so the hash-index keeps
		// real1/real2 unambiguous; EnumerateFileBlocks emits zero anyway.
	}
	if err := store.Put(context.Background(), zeroBlock); err != nil {
		t.Fatalf("Put zero-hash block: %v", err)
	}

	hs, err := snapshot.HashSetFromMetadataStore(context.Background(), store)
	if err != nil {
		t.Fatalf("HashSetFromMetadataStore: %v", err)
	}
	if hs.Len() != 2 {
		t.Errorf("HashSetFromMetadataStore: Len()=%d, want 2 (zero hash skipped)", hs.Len())
	}
	if hs.Contains(zero) {
		t.Error("HashSetFromMetadataStore: zero hash leaked into result")
	}
}
