package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// hashSeed returns a deterministic non-zero ContentHash from a seed integer.
// Distinct seeds produce distinct hashes — used to load 100K hashes for the
// memory-bounded behavior test without burning entropy.
func hashSeed(i int) blockstore.ContentHash {
	var h blockstore.ContentHash
	// Fan out 8 bytes from i across the 32-byte hash to keep distinct
	// seeds distinct under prefix iteration.
	for j := 0; j < 8; j++ {
		h[j] = byte(i >> (j * 8))
	}
	for j := 8; j < blockstore.HashSize; j++ {
		h[j] = byte((i ^ j) & 0xff)
	}
	return h
}

// TestGCState_NewCreatesIncompleteFlag: NewGCState creates dir/<runID>/
// with an incomplete.flag file at start.
func TestGCState_NewCreatesIncompleteFlag(t *testing.T) {
	root := t.TempDir()
	runID := "20260425T000000Z-aaaa"
	gcs, err := NewGCState(root, runID)
	if err != nil {
		t.Fatalf("NewGCState: %v", err)
	}
	defer func() { _ = gcs.Close() }()

	flag := filepath.Join(root, runID, gcStateIncompleteFlag)
	if _, err := os.Stat(flag); err != nil {
		t.Errorf("incomplete.flag not created: %v", err)
	}
	if gcs.RunDir() != filepath.Join(root, runID) {
		t.Errorf("RunDir: got %q, want %q", gcs.RunDir(), filepath.Join(root, runID))
	}
}

// TestGCState_AddHas: Add(h) records the hash; Has(h) returns true; Has
// for a non-added hash returns false.
func TestGCState_AddHas(t *testing.T) {
	root := t.TempDir()
	gcs, err := NewGCState(root, "test-run")
	if err != nil {
		t.Fatalf("NewGCState: %v", err)
	}
	defer func() { _ = gcs.Close() }()

	added := hashSeed(1)
	missing := hashSeed(2)

	if err := gcs.Add(added); err != nil {
		t.Fatalf("Add(added): %v", err)
	}

	present, err := gcs.Has(added)
	if err != nil {
		t.Fatalf("Has(added): %v", err)
	}
	if !present {
		t.Errorf("Has(added) = false, want true")
	}

	present, err = gcs.Has(missing)
	if err != nil {
		t.Fatalf("Has(missing): %v", err)
	}
	if present {
		t.Errorf("Has(missing) = true, want false")
	}
}

// TestGCState_AddIdempotent: repeated Adds with same hash succeed.
func TestGCState_AddIdempotent(t *testing.T) {
	root := t.TempDir()
	gcs, err := NewGCState(root, "test-run")
	if err != nil {
		t.Fatalf("NewGCState: %v", err)
	}
	defer func() { _ = gcs.Close() }()

	h := hashSeed(42)
	for i := 0; i < 5; i++ {
		if err := gcs.Add(h); err != nil {
			t.Fatalf("Add (iter %d): %v", i, err)
		}
	}
	present, err := gcs.Has(h)
	if err != nil || !present {
		t.Errorf("Has after repeated Add: present=%v err=%v", present, err)
	}
}

// TestGCState_MarkComplete: removes the incomplete.flag.
func TestGCState_MarkComplete(t *testing.T) {
	root := t.TempDir()
	gcs, err := NewGCState(root, "test-run")
	if err != nil {
		t.Fatalf("NewGCState: %v", err)
	}
	defer func() { _ = gcs.Close() }()

	flag := filepath.Join(gcs.RunDir(), gcStateIncompleteFlag)
	if _, err := os.Stat(flag); err != nil {
		t.Fatalf("incomplete.flag missing before MarkComplete: %v", err)
	}
	if err := gcs.MarkComplete(); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
	if _, err := os.Stat(flag); !os.IsNotExist(err) {
		t.Errorf("incomplete.flag still present after MarkComplete: stat err=%v", err)
	}
	// Idempotent: second call must not error.
	if err := gcs.MarkComplete(); err != nil {
		t.Errorf("second MarkComplete: %v", err)
	}
}

// TestGCState_CleanStaleGCStateDirs: removes every <runID>/ dir whose
// incomplete.flag is still present; leaves complete dirs alone.
func TestGCState_CleanStaleGCStateDirs(t *testing.T) {
	root := t.TempDir()

	// Stale dir with flag — should be removed.
	stale, err := NewGCState(root, "stale-run")
	if err != nil {
		t.Fatalf("NewGCState(stale): %v", err)
	}
	_ = stale.Close()

	// Complete dir — should survive.
	complete, err := NewGCState(root, "complete-run")
	if err != nil {
		t.Fatalf("NewGCState(complete): %v", err)
	}
	if err := complete.MarkComplete(); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
	_ = complete.Close()

	if err := CleanStaleGCStateDirs(root); err != nil {
		t.Fatalf("CleanStaleGCStateDirs: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "stale-run")); !os.IsNotExist(err) {
		t.Errorf("stale-run dir not removed: stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "complete-run")); err != nil {
		t.Errorf("complete-run dir was removed: %v", err)
	}
}

// TestGCState_CleanStaleGCStateDirs_MissingRoot: tolerates a missing rootDir.
func TestGCState_CleanStaleGCStateDirs_MissingRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	if err := CleanStaleGCStateDirs(missing); err != nil {
		t.Errorf("CleanStaleGCStateDirs(missing): %v", err)
	}
}

// TestGCState_CloseIsIdempotent: Close releases the Badger handle so the
// runDir can be removed; calling Close twice is safe.
func TestGCState_CloseIsIdempotent(t *testing.T) {
	root := t.TempDir()
	gcs, err := NewGCState(root, "test-run")
	if err != nil {
		t.Fatalf("NewGCState: %v", err)
	}
	if err := gcs.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := gcs.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	// runDir should now be removable (Badger lock released).
	if err := os.RemoveAll(gcs.RunDir()); err != nil {
		t.Errorf("RemoveAll runDir after Close: %v", err)
	}
}

// TestGCState_LargeFanout: 100,000 distinct hashes survive Add+Has lookups.
// Loose memory-bounded behavior assertion: Badger spills to disk; we don't
// keep a Go-map copy of the live set.
func TestGCState_LargeFanout(t *testing.T) {
	if testing.Short() {
		t.Skip("large fanout test skipped under -short")
	}
	root := t.TempDir()
	gcs, err := NewGCState(root, "fanout-run")
	if err != nil {
		t.Fatalf("NewGCState: %v", err)
	}
	defer func() { _ = gcs.Close() }()

	const n = 100_000
	for i := 0; i < n; i++ {
		if err := gcs.Add(hashSeed(i)); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	// Sample-check 200 entries to confirm Has works under load.
	stride := n / 200
	for i := 0; i < n; i += stride {
		present, err := gcs.Has(hashSeed(i))
		if err != nil {
			t.Fatalf("Has(%d): %v", i, err)
		}
		if !present {
			t.Errorf("Has(%d) = false after Add", i)
		}
	}
	// Negative check: a hash we never Added must report absent.
	neg := hashSeed(n + 1)
	present, err := gcs.Has(neg)
	if err != nil {
		t.Fatalf("Has(neg): %v", err)
	}
	if present {
		t.Errorf("Has(unadded) = true")
	}
}

// TestGCState_BatchedAdd_FlushSemantics asserts the IN-4-04 batched-Add
// contract. Both forms must produce the same observable Has() state for
// callers:
//
//   - Explicit FlushAdd() after a sub-batch run lets Has() see every Add.
//   - Has() with a pending batch implicitly flushes (defensive consistency
//     for tests that interleave Add/Has).
//
// Without this regression test a future refactor could silently regress
// the implicit-flush behavior — the sweep would then read stale state
// for the final partial batch and reap live CAS objects (INV-04 violation
// by data path).
func TestGCState_BatchedAdd_FlushSemantics(t *testing.T) {
	root := t.TempDir()
	gcs, err := NewGCState(root, "batched-flush-run")
	if err != nil {
		t.Fatalf("NewGCState: %v", err)
	}
	defer func() { _ = gcs.Close() }()

	// Add a sub-batch (less than gcAddBatchSize so nothing is auto-flushed).
	const sub = 50
	for i := 0; i < sub; i++ {
		if err := gcs.Add(hashSeed(i)); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	if err := gcs.FlushAdd(); err != nil {
		t.Fatalf("FlushAdd: %v", err)
	}
	for i := 0; i < sub; i++ {
		present, err := gcs.Has(hashSeed(i))
		if err != nil {
			t.Fatalf("Has(%d) after FlushAdd: %v", i, err)
		}
		if !present {
			t.Errorf("Has(%d) = false after explicit FlushAdd — batch lost", i)
		}
	}

	// Implicit flush via Has(): add another sub-batch then query without
	// calling FlushAdd. The contract on Has() is to flush first.
	for i := sub; i < sub*2; i++ {
		if err := gcs.Add(hashSeed(i)); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	present, err := gcs.Has(hashSeed(sub + 7))
	if err != nil {
		t.Fatalf("Has during pending batch: %v", err)
	}
	if !present {
		t.Errorf("Has during pending batch = false — implicit flush regression")
	}
}

// TestGCState_BatchedAdd_AutoFlushAtBatchSize asserts the auto-flush
// trigger fires at gcAddBatchSize. Without this, large mark phases would
// hold an unbounded WriteBatch in memory.
func TestGCState_BatchedAdd_AutoFlushAtBatchSize(t *testing.T) {
	root := t.TempDir()
	gcs, err := NewGCState(root, "batched-autoflush-run")
	if err != nil {
		t.Fatalf("NewGCState: %v", err)
	}
	defer func() { _ = gcs.Close() }()

	for i := 0; i < gcAddBatchSize; i++ {
		if err := gcs.Add(hashSeed(i)); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	if gcs.batch != nil {
		t.Errorf("batch != nil after %d Adds — auto-flush did not fire at batch boundary", gcAddBatchSize)
	}
	if gcs.batchLen != 0 {
		t.Errorf("batchLen = %d after auto-flush, want 0", gcs.batchLen)
	}
	// First hash from the auto-flushed batch must still be queryable.
	present, err := gcs.Has(hashSeed(0))
	if err != nil {
		t.Fatalf("Has after auto-flush: %v", err)
	}
	if !present {
		t.Errorf("Has(0) = false after auto-flush — data lost")
	}
}

// TestGCState_PersistLastRunSummary_RoundTrip: PersistLastRunSummary
// writes JSON that decodes back to the same shape.
func TestGCState_PersistLastRunSummary_RoundTrip(t *testing.T) {
	root := t.TempDir()
	in := GCRunSummary{
		RunID:        "20260425T120000Z-test",
		StartedAt:    time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC),
		CompletedAt:  time.Date(2026, 4, 25, 12, 0, 5, 0, time.UTC),
		HashesMarked: 1234,
		ObjectsSwept: 56,
		BytesFreed:   78901,
		DurationMs:   5000,
		ErrorCount:   2,
		FirstErrors:  []string{"sample-err-1", "sample-err-2"},
		DryRun:       false,
	}
	if err := PersistLastRunSummary(root, in); err != nil {
		t.Fatalf("PersistLastRunSummary: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(root, gcStateLastRunFile))
	if err != nil {
		t.Fatalf("read last-run.json: %v", err)
	}
	var out GCRunSummary
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal last-run.json: %v", err)
	}
	if out.RunID != in.RunID || out.HashesMarked != in.HashesMarked || out.BytesFreed != in.BytesFreed {
		t.Errorf("round-trip mismatch: in=%+v out=%+v", in, out)
	}
}

// TestGCState_PersistLastRunSummary_AtomicReplace: re-persisting overwrites
// the previous summary atomically (no .tmp left behind on success).
func TestGCState_PersistLastRunSummary_AtomicReplace(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 3; i++ {
		summary := GCRunSummary{RunID: fmt.Sprintf("run-%d", i), HashesMarked: int64(i)}
		if err := PersistLastRunSummary(root, summary); err != nil {
			t.Fatalf("PersistLastRunSummary[%d]: %v", i, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, gcStateLastRunFile+".tmp")); !os.IsNotExist(err) {
		t.Errorf(".tmp file leaked after final rename: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(root, gcStateLastRunFile))
	if err != nil {
		t.Fatalf("read last-run.json: %v", err)
	}
	var out GCRunSummary
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.RunID != "run-2" {
		t.Errorf("final RunID = %q, want run-2", out.RunID)
	}
}
