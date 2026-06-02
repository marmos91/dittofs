package fs

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestConsumeUpToStable_PreservesTouchedAfter is the unit-level guard for the
// C1 dirty-interval consume. ConsumeUpToStable must delete only intervals
// fully inside [0, endExclusive) whose Touched timestamp is <= notAfter, and
// must preserve any interval touched AFTER that instant (a write that raced
// the rollup's Phase B).
func TestConsumeUpToStable_PreservesTouchedAfter(t *testing.T) {
	tr := newIntervalTree()
	old := time.Unix(1000, 0)
	notAfter := time.Unix(1001, 0)
	fresh := time.Unix(1002, 0) // strictly after notAfter

	tr.Insert(0, 100, old)    // stable, within window -> consumed
	tr.Insert(200, 50, old)   // stable, within window -> consumed
	tr.Insert(100, 50, fresh) // raced Phase B -> MUST survive
	tr.Insert(400, 50, old)   // outside [0,300) window -> untouched

	tr.ConsumeUpToStable(300, notAfter)

	if got := tr.Len(); got != 2 {
		t.Fatalf("after consume: want 2 surviving intervals, got %d", got)
	}
	// The fresh in-window interval at offset 100 must remain.
	var offsets []uint64
	tr.t.Ascend(func(iv *interval) bool {
		offsets = append(offsets, iv.Offset)
		return true
	})
	if len(offsets) != 2 || offsets[0] != 100 || offsets[1] != 400 {
		t.Fatalf("unexpected survivors %v (want [100 400])", offsets)
	}
}

// TestConsumeUpToStable_EqualTimestampConsumed pins the boundary: an interval
// whose Touched == notAfter is consumed (the predicate is <=, not <), so the
// rollup's own stable interval — captured at phaseStart — is always swept.
func TestConsumeUpToStable_EqualTimestampConsumed(t *testing.T) {
	tr := newIntervalTree()
	ts := time.Unix(2000, 0)
	tr.Insert(0, 100, ts)
	tr.ConsumeUpToStable(100, ts)
	if got := tr.Len(); got != 0 {
		t.Fatalf("interval with Touched == notAfter should be consumed; Len=%d", got)
	}
}

// TestRollup_C1_WriteDuringPhaseB_NotConsumed is the integration guard for the
// C1 lock-split. It injects an AppendWrite into the exact window a rollup is
// committing — fired from rollupPhaseBHook, i.e. AFTER the append mutex was
// released for Phase B — and asserts the racing write's dirty marker survives
// the pass. Under the previous whole-pass-mutex design this race could not
// occur; under the split it is guarded by ConsumeUpToStable. With a naive
// ConsumeUpTo the injected interval would be swept and its bytes lost.
func TestRollup_C1_WriteDuringPhaseB_NotConsumed(t *testing.T) {
	// Mutates the package-global rollupPhaseBHook — must not run in parallel
	// with other rollup tests and must restore the hook on exit.
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreForTest(t, FSStoreOptions{
		MaxLogBytes:     1 << 30,
		RollupWorkers:   1,
		StabilizationMS: 1,
		RollupStore:     rs,
	})
	// Deliberately do NOT StartRollup: ForceRollupForTest drives the pass
	// synchronously so the hook fires on this goroutine.
	ctx := context.Background()
	const pid = "perf/c1/write-during-phaseb"

	// Initial write the rollup will consume.
	if err := bc.AppendWrite(ctx, pid, bytes.Repeat([]byte{0xAA}, 4096), 0); err != nil {
		t.Fatalf("initial AppendWrite: %v", err)
	}
	// Wait until the interval has aged past the (1ms) stabilization window.
	deadline := time.Now().Add(2 * time.Second)
	for !bc.EarliestStableForTest(pid) {
		if time.Now().After(deadline) {
			t.Fatal("interval never stabilized")
		}
		time.Sleep(2 * time.Millisecond)
	}

	var fired atomic.Bool
	rollupPhaseBHook = func(p string) {
		if p != pid || !fired.CompareAndSwap(false, true) {
			return
		}
		// Racing overwrite of the same region, mid-rollup. AppendWrite takes
		// the append mutex (released for Phase B) — not the rollup mutex — so
		// this does not deadlock against the in-flight pass on this goroutine.
		if err := bc.AppendWrite(ctx, pid, bytes.Repeat([]byte{0xBB}, 4096), 0); err != nil {
			t.Errorf("injected AppendWrite: %v", err)
		}
	}
	defer func() { rollupPhaseBHook = nil }()

	if err := bc.ForceRollupForTest(ctx, pid); err != nil {
		t.Fatalf("ForceRollupForTest: %v", err)
	}
	if !fired.Load() {
		t.Fatal("rollupPhaseBHook never fired — Phase B was not reached")
	}

	// The racing write must still be marked dirty so a later pass rolls it up.
	// Under plain ConsumeUpTo this would be 0 (the write was silently lost).
	if n := bc.IntervalsLenForTest(pid); n == 0 {
		t.Fatal("racing Phase-B write was consumed (lost) — ConsumeUpToStable regression")
	}

	// And the rollup must still make forward progress on the bytes it DID
	// process — a second forced pass rolls up the racing write too, once it
	// has aged past the stabilization window. Poll for stability first so the
	// second pass is not racing the 1ms window (the racing write's Touched was
	// set during Phase B above, only moments ago).
	deadline = time.Now().Add(2 * time.Second)
	for !bc.EarliestStableForTest(pid) {
		if time.Now().After(deadline) {
			t.Fatal("racing write never stabilized for the second pass")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := bc.ForceRollupForTest(ctx, pid); err != nil {
		t.Fatalf("second ForceRollupForTest: %v", err)
	}
	if n := bc.IntervalsLenForTest(pid); n != 0 {
		t.Fatalf("second pass did not drain the racing write: intervals=%d", n)
	}
}

// TestRollup_C1_ConcurrentOverwriteStorm hammers a few payloads with
// overlapping AppendWrites while the rollup worker pool runs, exercising the
// Phase-A/B/C lock-split (append mutex released during CAS store) under -race.
// It asserts no deadlock/panic and that every payload's log eventually drains
// to a quiescent state.
//
// Note: this workload can rarely emit a "dropping divergent stable interval"
// Warn — the benign C1 stale-tree-marker case documented in rollupFile (the
// offset's bytes are already in CAS; only a leftover dirty marker is dropped).
// It does not fail the test and is not a lost write.
func TestRollup_C1_ConcurrentOverwriteStorm(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreForTest(t, FSStoreOptions{
		MaxLogBytes:     1 << 30,
		RollupWorkers:   3,
		StabilizationMS: 2,
		RollupStore:     rs,
	})
	if err := bc.StartRollup(context.Background()); err != nil {
		t.Fatalf("StartRollup: %v", err)
	}
	ctx := context.Background()

	pids := []string{"perf/c1/storm/0", "perf/c1/storm/1", "perf/c1/storm/2"}
	const writersPerPid = 4
	const itersPerWriter = 60

	var wg sync.WaitGroup
	for _, pid := range pids {
		for w := 0; w < writersPerPid; w++ {
			wg.Add(1)
			go func(pid string, w int) {
				defer wg.Done()
				buf := make([]byte, 4096)
				for i := 0; i < itersPerWriter; i++ {
					// Overlap: writers target overlapping offsets within the
					// same file so rollup and AppendWrite contend on the same
					// payload's append mutex.
					off := uint64((i % 4) * 4096)
					for b := range buf {
						buf[b] = byte(w*31 + i)
					}
					if err := bc.AppendWrite(ctx, pid, buf, off); err != nil {
						t.Errorf("AppendWrite %s off=%d: %v", pid, off, err)
						return
					}
				}
			}(pid, w)
		}
	}
	wg.Wait()

	// Drain: force a few rollup passes per payload until intervals quiesce.
	for _, pid := range pids {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if err := bc.ForceRollupForTest(ctx, pid); err != nil {
				t.Fatalf("drain ForceRollup %s: %v", pid, err)
			}
			if bc.IntervalsLenForTest(pid) == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("payload %s did not drain: intervals=%d", pid, bc.IntervalsLenForTest(pid))
			}
			time.Sleep(3 * time.Millisecond)
		}
	}
}
