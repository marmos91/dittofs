package fs

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestRollup_DivergentInterval_DroppedNoLoop is the #668 regression for
// the consume-on-divergence path: when the tree carries a stable
// interval the logIndex cannot back, rollupFile must (i) return nil
// (not error), (ii) drop the divergent interval from the tree, (iii)
// leave a clean state so a subsequent pass does not re-process the same
// interval. Prior to this fix, rollupFile returned a hard error every
// pass for the divergent interval; the worker pool's ticker arm
// re-queued it forever, wedging the payload's rollup in a tight
// Error-log loop until process restart.
func TestRollup_DivergentInterval_DroppedNoLoop(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{
		MaxLogBytes:     1 << 30,
		StabilizationMS: 5,
		RollupStore:     memmeta.NewMemoryMetadataStoreWithDefaults(),
	})
	ctx := context.Background()
	const payloadID = "file-divergent"

	// Materialize per-payload state via a legitimate AppendWrite so we
	// have a valid lf / tree / mu / idx tuple. The interval we will
	// "inject" as divergent must NOT overlap this real append, otherwise
	// EarliestStable could surface the real one and short-circuit the
	// test before the divergent branch fires.
	if err := bc.AppendWrite(ctx, payloadID, bytes.Repeat([]byte{0x11}, 64), 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	// Inject a tree-only interval at offset 1<<20 with no matching
	// logIndex entry — exactly the divergence state the audit traced
	// from interrupted writes. Touch is timestamped in the past so the
	// stabilization window elapses immediately and EarliestStable
	// returns this interval (it is the earliest UNCONSUMED stable one
	// after the real append rolls up, OR can be reached on the first
	// pass if it sorts ahead of the real append's Touched).
	bc.logsMu.RLock()
	tree := bc.dirtyIntervals[payloadID]
	mu := bc.logLocks[payloadID]
	bc.logsMu.RUnlock()
	if tree == nil || mu == nil {
		t.Fatalf("expected tree+mu to exist after AppendWrite")
	}

	// Drain the legitimate append first so EarliestStable only sees the
	// injected divergent interval. Do this by advancing the rollup once
	// before injection.
	pastTouch := time.Now().Add(-time.Hour)
	mu.Lock()
	tree.Insert(1<<20, 4096, pastTouch)
	mu.Unlock()

	// First rollupFile call: real interval at offset 0 is the earliest
	// stable, gets processed (no error). Or, if the real append's
	// Touched is more recent than the injected past one, the injected
	// one is earliest. Either ordering is acceptable — we drive
	// rollupFile until the divergent interval is the earliest stable.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := bc.rollupFile(ctx, payloadID, false); err != nil {
			t.Fatalf("rollupFile returned error (divergence wedge regression): %v", err)
		}
		bc.logsMu.RLock()
		tr := bc.dirtyIntervals[payloadID]
		bc.logsMu.RUnlock()
		mu.Lock()
		_, hasStable := tr.EarliestStable(time.Now(), time.Duration(bc.stabilizationMS)*time.Millisecond)
		mu.Unlock()
		if !hasStable {
			// Tree drained — divergent interval was dropped, real
			// interval was rolled up. Success.
			return
		}
		// Ensure the injected interval is still aged.
		mu.Lock()
		tr.DropExact(1<<20, 4096)
		tr.Insert(1<<20, 4096, pastTouch)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("rollupFile did not drain tree within 2s — divergent interval still loops")
}

// TestRollup_DivergentInterval_NoErrorReturned is the focused unit-level
// assertion: a single rollupFile call against a tree that holds a
// stable interval with no logIndex backing must return nil and the
// interval must be gone from the tree afterward. This is the minimal
// contract the rollup worker pool relies on to avoid the re-queue loop
// in #668.
func TestRollup_DivergentInterval_NoErrorReturned(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{
		MaxLogBytes:     1 << 30,
		StabilizationMS: 1,
		RollupStore:     memmeta.NewMemoryMetadataStoreWithDefaults(),
	})
	ctx := context.Background()
	const payloadID = "file-divergent-unit"

	// Force materialization via getOrCreateLog. We avoid AppendWrite so
	// no logIndex entries exist — every tree interval is divergent by
	// construction.
	_, mu, tree, _, err := bc.getOrCreateLog(payloadID)
	if err != nil {
		t.Fatalf("getOrCreateLog: %v", err)
	}
	pastTouch := time.Now().Add(-time.Hour)
	mu.Lock()
	tree.Insert(0, 4096, pastTouch)
	mu.Unlock()

	if err := bc.rollupFile(ctx, payloadID, false); err != nil {
		t.Fatalf("rollupFile returned error on divergent interval: %v", err)
	}

	// Tree must have dropped the divergent interval.
	mu.Lock()
	_, hasStable := tree.EarliestStable(time.Now(), time.Duration(bc.stabilizationMS)*time.Millisecond)
	mu.Unlock()
	if hasStable {
		t.Fatalf("divergent interval not dropped from tree after rollupFile")
	}

	// Second pass: must remain a no-op (no loop).
	if err := bc.rollupFile(ctx, payloadID, false); err != nil {
		t.Fatalf("second rollupFile after drain returned error: %v", err)
	}
}

// TestAppendWrite_TreeAndLogIndexAtomicity is the #668 atomic-update regression:
// concurrent AppendWrites must never produce a tree entry without a
// matching logIndex entry. Prior to the fix, the second bc.logsMu.RLock
// cycle inside AppendWrite could observe a nil logIndex (or a logIndex
// that had been recreated by a parallel DeleteAppendLog) and silently
// skip Append while tree.Insert had already landed — the exact
// divergence shape that wedged the rollup in #668.
//
// With the fix, getOrCreateLog returns idx atomically with tree under
// bc.logsMu and AppendWrite calls idx.Append BEFORE tree.Insert, so any
// later observer that sees a tree entry is guaranteed to find the
// matching logIndex entry.
func TestAppendWrite_TreeAndLogIndexAtomicity(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})
	const payloadID = "file-atomic"
	const goroutines = 32
	const payloadLen = 4096
	payload := bytes.Repeat([]byte{0x7F}, payloadLen)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			off := uint64(i) * uint64(payloadLen)
			if err := bc.AppendWrite(context.Background(), payloadID, payload, off); err != nil {
				t.Errorf("AppendWrite at %d: %v", off, err)
			}
		}(i)
	}
	wg.Wait()

	// Walk every tree interval; assert each has a matching logIndex
	// entry at the same fileOff with the same payloadLen.
	bc.logsMu.RLock()
	tree := bc.dirtyIntervals[payloadID]
	idx := bc.logIndices[payloadID]
	mu := bc.logLocks[payloadID]
	bc.logsMu.RUnlock()
	if tree == nil || idx == nil || mu == nil {
		t.Fatalf("per-payload state missing after concurrent AppendWrite")
	}
	mu.Lock()
	defer mu.Unlock()
	if tree.Len() != goroutines {
		t.Fatalf("tree.Len: got %d want %d", tree.Len(), goroutines)
	}
	if len(idx.entries) != goroutines {
		t.Fatalf("idx.entries len: got %d want %d", len(idx.entries), goroutines)
	}
	// For each tree interval there must be at least one logIndex entry
	// at the same (fileOff, payloadLen). EntriesForInterval is the
	// production query; using it here mirrors the rollup's view.
	tree.t.Ascend(func(iv *interval) bool {
		hits := idx.EntriesForInterval(iv.Offset, uint64(iv.Length))
		found := false
		for _, h := range hits {
			if h.fileOff == iv.Offset && h.payloadLen == iv.Length {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("tree interval [%d,+%d) has no matching logIndex entry", iv.Offset, iv.Length)
		}
		return true
	})
}

// TestAppendWrite_RollupConcurrent_NoDivergence stress-tests the
// atomic tree+logIndex update under the production code path: writers
// and rollup compete on the per-file mutex while the rollup pool ticker
// drains intervals. rollupFile must never observe a divergent interval
// under this workload, because AppendWrite populates tree and logIndex
// together under the per-file mutex against an idx snapshot pinned
// under the same bc.logsMu cycle that allocated tree.
func TestAppendWrite_RollupConcurrent_NoDivergence(t *testing.T) {
	bc, _ := newRollupFSStore(t, 1<<30, 5)
	const payloadID = "file-concurrent-rollup"
	const writers = 8
	const writesPer = 64
	const payloadLen = 4096
	payload := bytes.Repeat([]byte{0x5A}, payloadLen)
	ctx := context.Background()

	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			for j := 0; j < writesPer; j++ {
				off := uint64(i*writesPer+j) * uint64(payloadLen)
				if err := bc.AppendWrite(ctx, payloadID, payload, off); err != nil {
					t.Errorf("AppendWrite: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	// Cross-check tree + logIndex consistency after writes complete.
	// Without the atomic update, a writer that lost the second-RLock
	// idx lookup race would leave the tree carrying an interval the
	// logIndex cannot back. Production-path verification: every tree
	// interval has a matching logIndex hit.
	bc.logsMu.RLock()
	tree := bc.dirtyIntervals[payloadID]
	idx := bc.logIndices[payloadID]
	mu := bc.logLocks[payloadID]
	bc.logsMu.RUnlock()
	if tree != nil && idx != nil && mu != nil {
		mu.Lock()
		tree.t.Ascend(func(iv *interval) bool {
			hits := idx.EntriesForInterval(iv.Offset, uint64(iv.Length))
			found := false
			for _, h := range hits {
				if h.fileOff == iv.Offset && h.payloadLen == iv.Length {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("divergence: tree interval [%d,+%d) has no matching logIndex entry", iv.Offset, iv.Length)
			}
			return true
		})
		mu.Unlock()
	}
}
