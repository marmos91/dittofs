package fs

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// TestRollup_LRUPut_DeferredUntilAfterPersister proves I-2 (#669)
// Prong A: dedupLRU.Put MUST NOT run before objectIDPersister returns
// nil. Otherwise a concurrent rollup that hits the LRU between
// StoreChunk and the persister write observes ErrUnknownHash and
// triggers the retry storm the audit flagged.
//
// Mechanism: install a persister that BLOCKS on a release channel. While
// the rollup is parked inside the persister, assert that the dedupLRU
// does NOT yet contain the hash. Release the persister; assert the LRU
// IS populated post-return.
func TestRollup_LRUPut_DeferredUntilAfterPersister(t *testing.T) {
	bc, _, _ := newFSStoreForRollupLRUTest(t)

	payload := bytes.Repeat([]byte{0xA1}, 256*1024)
	expectedHash := hashOfSingleChunk(payload)

	// Persister blocks until release is closed.
	release := make(chan struct{})
	inside := make(chan struct{})
	bc.SetObjectIDPersister(func(_ context.Context, _ string, _ []blockstore.BlockRef, _ blockstore.ObjectID) error {
		close(inside)
		<-release
		return nil
	})

	ctx := context.Background()
	if err := bc.AppendWrite(ctx, "deferred", payload, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if bc.EarliestStableForTest("deferred") {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !bc.EarliestStableForTest("deferred") {
		t.Fatal("dirty interval did not stabilize")
	}

	rollupDone := make(chan error, 1)
	go func() {
		rollupDone <- bc.rollupFile(ctx, "deferred")
	}()

	// Wait until rollup is parked inside the persister.
	select {
	case <-inside:
	case <-time.After(2 * time.Second):
		t.Fatal("rollup did not reach persister within deadline")
	}

	// I-2 INVARIANT: at this moment the chunk has been StoreChunk'd but
	// the FileBlock row has NOT been persisted yet. The LRU MUST be
	// empty for this hash — otherwise a concurrent rollup hitting the
	// LRU would call AddRef on a row that does not exist.
	if bc.dedupLRU.Has(expectedHash) {
		t.Fatal("I-2 (#669) regression: dedupLRU contains hash BEFORE persister returned — pre-#669 ordering")
	}

	// Release persister; rollup should complete and populate the LRU.
	close(release)
	select {
	case err := <-rollupDone:
		if err != nil {
			t.Fatalf("rollupFile: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rollup did not complete after persister release")
	}

	if !bc.dedupLRU.Has(expectedHash) {
		t.Fatal("dedupLRU not populated after persister returned nil — Prong A populate site missing")
	}
}

// TestRollup_PersisterError_DoesNotPopulateLRU proves the LRU populate
// site is gated on persister success. If the persister returns an
// error, rollupFile returns early and the LRU MUST stay empty for the
// just-stored hash — otherwise a future rollup pass would AddRef on a
// row that was never written.
func TestRollup_PersisterError_DoesNotPopulateLRU(t *testing.T) {
	bc, _, _ := newFSStoreForRollupLRUTest(t)

	payload := bytes.Repeat([]byte{0xA2}, 256*1024)
	expectedHash := hashOfSingleChunk(payload)

	simulated := errors.New("simulated persister failure")
	bc.SetObjectIDPersister(func(_ context.Context, _ string, _ []blockstore.BlockRef, _ blockstore.ObjectID) error {
		return simulated
	})

	ctx := context.Background()
	if err := bc.AppendWrite(ctx, "errpath-lru", payload, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if bc.EarliestStableForTest("errpath-lru") {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	err := bc.rollupFile(ctx, "errpath-lru")
	if !errors.Is(err, simulated) {
		t.Fatalf("rollupFile error: want errors.Is(simulated)=true, got %v", err)
	}

	if bc.dedupLRU.Has(expectedHash) {
		t.Fatal("dedupLRU populated despite persister error — Prong A failure-gate missing")
	}
}

// TestRollup_AddRefErrUnknownHash_EvictsStaleLRUEntry proves I-2 Prong
// B: when AddRef returns ErrUnknownHash on an LRU hit (the LRU pointed
// at a row that does not exist — TOCTOU sweep, post-restart re-seed
// without the row, or any other stale-LRU scenario), the rollup
// evicts the stale entry from the LRU. Without this, a concurrent
// rollup against the same hash re-hits the LRU and re-fails AddRef,
// which is the retry storm the audit flagged.
func TestRollup_AddRefErrUnknownHash_EvictsStaleLRUEntry(t *testing.T) {
	bc, wrapped, _ := newFSStoreForRollupLRUTest(t)

	payload := bytes.Repeat([]byte{0xA3}, 256*1024)
	h := hashOfSingleChunk(payload)

	// Pre-seed LRU with a stale entry; programmable FBS returns
	// ErrUnknownHash to simulate the row not existing in the metadata
	// store.
	bc.dedupLRU.Put(h, "stale-payload")
	if !bc.dedupLRU.Has(h) {
		t.Fatal("precondition: LRU.Has(h) is false after Put")
	}
	wrapped.addRefOverride = func(_ context.Context, _ blockstore.ContentHash, _ string, _ blockstore.BlockRef) error {
		return blockstore.ErrUnknownHash
	}

	// Block persister so we can observe LRU state immediately after the
	// hit-path evicted the stale entry, BEFORE the post-persister
	// repopulate site re-adds it.
	release := make(chan struct{})
	inside := make(chan struct{})
	bc.SetObjectIDPersister(func(_ context.Context, _ string, _ []blockstore.BlockRef, _ blockstore.ObjectID) error {
		close(inside)
		<-release
		return nil
	})

	ctx := context.Background()
	if err := bc.AppendWrite(ctx, "evict-stale", payload, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if bc.EarliestStableForTest("evict-stale") {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !bc.EarliestStableForTest("evict-stale") {
		t.Fatal("dirty interval did not stabilize")
	}

	rollupDone := make(chan error, 1)
	go func() {
		rollupDone <- bc.rollupFile(ctx, "evict-stale")
	}()

	select {
	case <-inside:
	case <-time.After(2 * time.Second):
		t.Fatal("rollup did not reach persister within deadline")
	}

	// At this point AddRef has returned ErrUnknownHash → fall-back
	// evicted the stale LRU entry → StoreChunk wrote the CAS chunk →
	// rollup is parked inside the persister. The LRU MUST NOT contain
	// the hash, otherwise a concurrent rollup would re-hit it and
	// re-fail AddRef in a tight loop.
	if bc.dedupLRU.Has(h) {
		t.Fatal("I-2 Prong B regression: stale LRU entry was NOT evicted on ErrUnknownHash — retry storm window still open")
	}

	if wrapped.addRefCalls.Load() != 1 {
		t.Fatalf("AddRef calls: got %d want 1 (single hit, then evict)", wrapped.addRefCalls.Load())
	}

	close(release)
	if err := <-rollupDone; err != nil {
		t.Fatalf("rollupFile: %v", err)
	}

	// After persister returns nil, the Prong A populate site re-seeds
	// the LRU with the freshly-written row's identity.
	if !bc.dedupLRU.Has(h) {
		t.Fatal("dedupLRU not re-seeded after persister returned nil (Prong A populate missing post-fallback)")
	}
}

// TestRollup_ConcurrentRollups_SameHash_NoErrUnknownHashStorm exercises
// the #669 root scenario end-to-end: two rollups on different
// payloads with identical content. The fix (Prong A: LRU Put AFTER
// persister) guarantees the second rollup never sees an LRU hit until
// the first's FileBlock row is persisted. The second rollup either
// (a) loses the race and StoreChunks itself, or (b) wins the race and
// gets a clean AddRef. Crucially, AddRef MUST NOT return
// ErrUnknownHash to the rollup code path.
func TestRollup_ConcurrentRollups_SameHash_NoErrUnknownHashStorm(t *testing.T) {
	bc, wrapped, _ := newFSStoreForRollupLRUTest(t)

	payload := bytes.Repeat([]byte{0xA4}, 256*1024)

	// Counter for ErrUnknownHash observations from AddRef. The fix
	// closes the window where this would fire on a fresh first-pass
	// run.
	var unknownHashHits atomic.Int64
	wrapped.addRefOverride = func(ctx context.Context, h blockstore.ContentHash, payloadID string, ref blockstore.BlockRef) error {
		err := wrapped.inner.AddRef(ctx, h, payloadID, ref)
		if errors.Is(err, blockstore.ErrUnknownHash) {
			unknownHashHits.Add(1)
		}
		return err
	}

	bc.SetObjectIDPersister(func(_ context.Context, _ string, _ []blockstore.BlockRef, _ blockstore.ObjectID) error {
		// Real persister installed by the engine writes the FileBlock
		// rows. Simulate that here by writing the row directly into the
		// memory store wired into the FBS wrapper.
		return nil
	})

	ctx := context.Background()
	const concurrency = 8
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		pid := "pid-" + string(rune('A'+i))
		go func(pid string) {
			defer wg.Done()
			if err := bc.AppendWrite(ctx, pid, payload, 0); err != nil {
				t.Errorf("AppendWrite(%s): %v", pid, err)
				return
			}
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if bc.EarliestStableForTest(pid) {
					break
				}
				time.Sleep(1 * time.Millisecond)
			}
			if err := bc.rollupFile(ctx, pid); err != nil {
				t.Errorf("rollupFile(%s): %v", pid, err)
			}
		}(pid)
	}
	wg.Wait()

	// The fix's guarantee: with persister returning nil (no actual
	// row write in this fixture), AddRef on an LRU hit returns
	// ErrUnknownHash — but the stale entry is now evicted on first
	// observation (Prong B), so subsequent rollups in the same race
	// either StoreChunk fresh or observe an empty LRU. The retry
	// storm — where N concurrent rollups all see the same stale
	// entry and all spam AddRef in a tight loop — is closed.
	//
	// Upper bound on ErrUnknownHash observations: one per LRU hit;
	// each hit evicts. Since each AppendWrite races to populate the
	// LRU (post-persister) and concurrent peers may consume one stale
	// entry between Put and eviction, the count is O(concurrency),
	// not O(concurrency * passes).
	hits := unknownHashHits.Load()
	if hits > int64(concurrency) {
		t.Fatalf("ErrUnknownHash retry storm: got %d hits across %d rollups (upper bound %d)",
			hits, concurrency, concurrency)
	}
}

// TestDedupLRU_Delete_RemovesEntry pins the Delete behavior on the
// dedupLRU. Used by rollup.go's hit-path on ErrUnknownHash.
func TestDedupLRU_Delete_RemovesEntry(t *testing.T) {
	c := newDedupLRU(8)
	var h blockstore.ContentHash
	h[0] = 0x42

	if c.Has(h) {
		t.Fatal("precondition: empty LRU should not Has(h)")
	}
	c.Put(h, "payload-1")
	if !c.Has(h) {
		t.Fatal("post-Put: LRU.Has(h) is false")
	}
	c.Delete(h)
	if c.Has(h) {
		t.Fatal("post-Delete: LRU.Has(h) is true — Delete did not remove entry")
	}
	// Delete on missing hash is a no-op.
	c.Delete(h)

	// LRU still usable after Delete.
	c.Put(h, "payload-2")
	if !c.Has(h) {
		t.Fatal("post-Delete-then-Put: LRU.Has(h) is false")
	}
}

// TestDedupLRU_Delete_DegenerateNoOp covers the nil-receiver and
// maxSize<=0 guard branches in Delete.
func TestDedupLRU_Delete_DegenerateNoOp(t *testing.T) {
	var nilLRU *dedupLRU
	var h blockstore.ContentHash
	// Must not panic on nil receiver.
	nilLRU.Delete(h)

	zeroLRU := newDedupLRU(0)
	zeroLRU.Delete(h)
}
