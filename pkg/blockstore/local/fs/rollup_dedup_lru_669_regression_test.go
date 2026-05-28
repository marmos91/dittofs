package fs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// #669 regression tests for dedup-LRU populate ordering and
// (hash, payloadID) scoping. Two prongs:
//
//	Prong A — populate AFTER persister. The pre-fix code Put the hash
//	into the LRU between StoreChunk and the ObjectIDPersister callback.
//	A concurrent rollup on a different payload that hashed to the same
//	content (or the same payload reentering rollup during persister)
//	saw the LRU hit, called FileBlockStore.AddRef, and got
//	ErrUnknownHash because the FileBlock row was not yet written.
//
//	Prong B — (hash, payloadID) scoping. Cross-payload LRU short-
//	circuit is no longer reachable; the only AddRef triggered by the
//	LRU path is for a (hash, payloadID) pair THIS payload's prior
//	rollup pass produced, which guarantees the row's owner matches
//	(no wrong-row-owner refcount bump).

// TestRollup_DedupLRU_NotPopulatedBeforePersister asserts the Prong A
// invariant directly: a persister whose callback observes the LRU
// state must find that the rollup pass's hash is NOT yet present.
// Populating the LRU before persister returns is exactly the #669
// crash window that this fix closes.
func TestRollup_DedupLRU_NotPopulatedBeforePersister(t *testing.T) {
	bc, _, _ := newFSStoreForRollupLRUTest(t)

	const pid = "pre-persister"
	payload := bytes.Repeat([]byte{0x10}, 256*1024)
	h := hashOfSingleChunk(payload)

	var observedDuringPersister atomic.Bool
	bc.SetObjectIDPersister(func(_ context.Context, _ string, _ []blockstore.BlockRef, _ blockstore.ObjectID) error {
		// Crucial: the LRU MUST be empty for h at this point. If the
		// pre-fix ordering returns, this flips true.
		if bc.dedupLRU.Has(h, pid) {
			observedDuringPersister.Store(true)
		}
		return nil
	})

	runRollupOnce(t, bc, pid, payload)

	if observedDuringPersister.Load() {
		t.Fatal("#669 regression: dedupLRU populated BEFORE persister returned — concurrent rollup would observe a hash whose FileBlock row is not yet written and AddRef would return ErrUnknownHash")
	}
	// Sanity: after the full rollup, the LRU MUST hold the hash so
	// subsequent idempotent rewrites can hit the AddRef fast path.
	if !bc.dedupLRU.Has(h, pid) {
		t.Fatal("post-rollup: LRU does not contain h — PutMany after persister did not fire")
	}
}

// TestRollup_DedupLRU_PersisterFailure_StaysEmpty asserts that when the
// ObjectIDPersister callback FAILS, the LRU is NOT populated for the
// in-flight hashes. A pre-fix Put-before-persister code path would
// leave the LRU populated for a hash whose FileBlock row never
// landed; a subsequent rollup would hit the LRU, call AddRef, and
// get ErrUnknownHash — the exact #669 storm.
func TestRollup_DedupLRU_PersisterFailure_StaysEmpty(t *testing.T) {
	bc, _, _ := newFSStoreForRollupLRUTest(t)

	const pid = "persister-fail"
	payload := bytes.Repeat([]byte{0x20}, 256*1024)
	h := hashOfSingleChunk(payload)

	simulated := errors.New("simulated persister failure")
	bc.SetObjectIDPersister(func(_ context.Context, _ string, _ []blockstore.BlockRef, _ blockstore.ObjectID) error {
		return simulated
	})

	ctx := context.Background()
	if err := bc.AppendWrite(ctx, pid, payload, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	// Drive stabilization, then call rollupFile directly so we can
	// observe the propagated persister error.
	waitForStable(t, bc, pid)
	err := bc.rollupFile(ctx, pid)
	if !errors.Is(err, simulated) {
		t.Fatalf("rollupFile error: got %v, want errors.Is(simulated)", err)
	}

	if bc.dedupLRU.Has(h, pid) {
		t.Fatal("#669 regression: dedupLRU populated for a hash whose persister callback failed — a future rollup pass would observe the LRU hit and call AddRef on a non-existent row (ErrUnknownHash storm)")
	}
}

// TestRollup_DedupLRU_CrossPayload_NoShortCircuit asserts the Prong B
// invariant: after payload P1 rolls up content with hash H, a second
// payload P2 writing the same content does NOT short-circuit via
// the LRU. The "wrong-row-owner" subcase is therefore unreachable
// — AddRef is only called for a (hash, payloadID) pair that THIS
// payload itself populated, so the row found by hash belongs to
// THIS payload (modulo legacy multi-row-per-hash data; even then
// the bump still targets a row this payload created, not a row
// owned by a different payload).
func TestRollup_DedupLRU_CrossPayload_NoShortCircuit(t *testing.T) {
	bc, wrapped, _ := newFSStoreForRollupLRUTest(t)

	payload := bytes.Repeat([]byte{0x30}, 256*1024)
	h := hashOfSingleChunk(payload)

	// First payload: cold LRU → StoreChunk + PutMany after persister.
	runRollupOnce(t, bc, "p1", payload)
	if !bc.dedupLRU.Has(h, "p1") {
		t.Fatal("precondition: p1's rollup did not populate LRU for (h, p1)")
	}
	if bc.dedupLRU.Has(h, "p2") {
		t.Fatal("LRU compound-key scoping violated: p1's Put leaked to p2's lookup")
	}

	addRefBefore := wrapped.addRefCalls.Load()

	// Second payload, same content, DIFFERENT payloadID. With compound
	// (hash, payloadID) keying the LRU misses → StoreChunk path runs
	// — AddRef MUST NOT be invoked for cross-payload short-circuit.
	runRollupOnce(t, bc, "p2", payload)

	if got := wrapped.addRefCalls.Load() - addRefBefore; got != 0 {
		t.Fatalf("#669 regression: cross-payload LRU short-circuit triggered AddRef (calls delta=%d, want 0). The 'wrong-row-owner' subcase would let AddRef bump RefCount on p1's row.", got)
	}
	// p2's rollup must independently populate its own (h, p2) slot.
	if !bc.dedupLRU.Has(h, "p2") {
		t.Fatal("post-rollup: LRU missing (h, p2) — p2's own PutMany did not fire")
	}
}

// TestRollup_DedupLRU_ConcurrentRollups_NoErrUnknownHash drives two
// concurrent rollups on different payloads with identical content
// and asserts no ErrUnknownHash escapes the rollup pipeline. Under
// the pre-fix ordering, a tight enough race would let payload B
// observe payload A's pre-persister LRU populate, call AddRef, and
// hit ErrUnknownHash (silent retry storm under load — the #669
// production signature).
//
// Compound-key scoping (Prong B) means cross-payload LRU short-
// circuit cannot happen at all, which is the strongest possible
// regression guard: even if Prong A regresses, B cannot wedge the
// pipeline cross-payload.
func TestRollup_DedupLRU_ConcurrentRollups_NoErrUnknownHash(t *testing.T) {
	bc, wrapped, _ := newFSStoreForRollupLRUTest(t)

	payload := bytes.Repeat([]byte{0x40}, 256*1024)

	// Force AddRef to record any ErrUnknownHash escape. If it sees
	// one, the test fails — the production signature of #669.
	var unknownHashSeen atomic.Int64
	wrapped.addRefOverride = func(ctx context.Context, h blockstore.ContentHash, pid string, ref blockstore.BlockRef) error {
		err := wrapped.inner.AddRef(ctx, h, pid, ref)
		if errors.Is(err, blockstore.ErrUnknownHash) {
			unknownHashSeen.Add(1)
		}
		return err
	}

	const N = 8
	var wg sync.WaitGroup
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pid := "concurrent-" + string(rune('a'+i))
			// t.FailNow (via t.Fatal*) is undefined when called from
			// a non-test goroutine — fan errors back via channel so
			// the test goroutine reports them.
			if err := runRollupOnceErr(bc, pid, payload); err != nil {
				errCh <- fmt.Errorf("%s: %w", pid, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent rollup: %v", err)
	}
	if t.Failed() {
		t.FailNow()
	}

	if got := unknownHashSeen.Load(); got != 0 {
		t.Fatalf("#669 regression: AddRef returned ErrUnknownHash %d times under concurrent same-content rollups across distinct payloads — the #669 storm has not been closed", got)
	}
}

// waitForStable polls EarliestStableForTest until the dirty interval
// for payloadID becomes available (matches the helper pattern used
// elsewhere in this test file). Fails the test if it does not
// stabilize within 500 ms.
func waitForStable(t *testing.T, bc *FSStore, payloadID string) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if bc.EarliestStableForTest(payloadID) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !bc.EarliestStableForTest(payloadID) {
		t.Fatal("dirty interval did not stabilize within 500 ms")
	}
}
