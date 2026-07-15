package fs

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// activeBlobForTest returns the ID and durable size of the log-blob Manager's
// currently active blob, so a test can truncate it back to that size to model
// a power-loss that discards an unsynced tail.
func activeBlobForTest(t *testing.T, bc *FSStore) (string, int64) {
	t.Helper()
	blobs, err := bc.logBlob.ListBlobs()
	if err != nil {
		t.Fatalf("ListBlobs: %v", err)
	}
	for _, b := range blobs {
		if b.Active {
			return b.LogBlobID, b.Size
		}
	}
	t.Fatal("no active log blob")
	return "", 0
}

// waitStableForTest polls until payloadID's earliest interval ages past the
// stabilization window (or fails the test on timeout).
func waitStableForTest(t *testing.T, bc *FSStore, payloadID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !bc.EarliestStableForTest(payloadID) {
		if time.Now().After(deadline) {
			t.Fatal("interval never stabilized")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestRollup_LocalIndex_FencedBehindBlobSync is the crash-durability guard for
// the log-blob rollup path. It reproduces the power-loss window between a
// chunk's blob append and the per-pass blob fsync:
//
//   - Phase B appends the chunk bytes to the blob (page cache, NOT fsynced).
//   - Phase C fsyncs the blob, THEN advances the rollup fence.
//
// If the durable local-index entry (the one HasChunk dedups on) is committed in
// Phase B — before the blob is fsynced — a crash in that window leaves a durable
// index entry pointing past the durable blob length. On replay the rollup re-runs
// from the un-advanced fence, HasChunk hits the surviving entry, StoreChunk
// becomes a no-op, and the bytes are never re-appended: once the append-log
// record is compacted away the acknowledged write is permanently lost.
//
// Invariant under test: no durable index entry may exist for a chunk whose blob
// fsync did not succeed. The test fails the blob Sync mid-pass, then asserts the
// index still misses (Part A). It then restarts the store — rebuilding the log
// index from disk and dropping the unsynced blob tail — re-runs a clean rollup,
// and asserts the chunk is re-appended and reads back byte-identical (Part B).
//
// It FAILS against code that commits the index in Phase B (the entry survives
// the failed sync) and PASSES once the index write is fenced behind the Phase C
// blob fsync.
func TestRollup_LocalIndex_FencedBehindBlobSync(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// A memory store stands in for the (durable) production backend that
	// serves both the LocalChunkIndex and RollupStore facets; sharing the SAME
	// instance across the reopen models its durability surviving a crash.
	idx := memmeta.NewMemoryMetadataStoreWithDefaults()
	opts := FSStoreOptions{
		MaxLogBytes:     1 << 30,
		StabilizationMS: 1,
		RollupWorkers:   1,
	}

	bc, err := NewWithOptions(dir, 0, idx, opts)
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}

	const pid = "logblob/durability/index-fence"
	// < MinChunkSize (1 MiB) so the write rolls up as exactly one chunk whose
	// content hash equals blake3 of the whole payload.
	payload := bytes.Repeat([]byte{0xC1}, 4096)
	h := blake3ContentHash(payload)

	if err := bc.AppendWrite(ctx, pid, payload, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	if ok, err := bc.HasChunk(ctx, h); err != nil || ok {
		t.Fatalf("HasChunk before rollup = (%v, %v); want (false, nil)", ok, err)
	}

	waitStableForTest(t, bc, pid)

	// Durable size of the active blob before the pass, so recovery can drop the
	// unsynced tail the crash left behind.
	blobID, preSize := activeBlobForTest(t, bc)

	// Crash window: fail the blob fsync exactly once, mid-pass. The pass appends
	// the chunk to the blob, then the injected fault aborts before the fsync +
	// fence advance.
	var failNext atomic.Bool
	failNext.Store(true)
	rollupPreSyncFailHook = func() error {
		if failNext.CompareAndSwap(true, false) {
			return errors.New("injected: simulated power loss before blob fsync")
		}
		return nil
	}
	defer func() { rollupPreSyncFailHook = nil }()

	if err := bc.ForceRollupForTest(ctx, pid); err == nil {
		t.Fatal("ForceRollupForTest: want injected sync failure, got nil")
	}

	// Part A — invariant: the blob was never fsynced this pass, so no durable
	// index entry may exist. Pre-fix, Phase B committed it, so the entry survives
	// and a replay would dedup-skip the re-append and lose the write.
	if ok, err := bc.HasChunk(ctx, h); err != nil {
		t.Fatalf("HasChunk after failed-sync pass: %v", err)
	} else if ok {
		t.Fatal("durable local-index entry exists for a chunk whose blob fsync did " +
			"not succeed: a crash here dedup-skips the re-append and loses the write")
	}

	// --- Simulate crash + restart ---
	rollupPreSyncFailHook = nil // recovery pass must fsync for real
	if err := bc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	bc2, err := NewWithOptions(dir, 0, idx, opts)
	if err != nil {
		t.Fatalf("NewWithOptions (reopen): %v", err)
	}
	t.Cleanup(func() { _ = bc2.Close() })

	// Power loss discards the unsynced blob tail: truncate the reopened blob back
	// to its pre-append durable size (the crash-recovery reconcile of a torn tail).
	if err := bc2.logBlob.Recover(ctx, blobID, preSize); err != nil {
		t.Fatalf("logblob Recover: %v", err)
	}
	// Rebuild the in-memory log index + interval trees from the on-disk
	// append-log (the record survived; the fence never advanced).
	if err := bc2.Recover(ctx); err != nil {
		t.Fatalf("FSStore Recover: %v", err)
	}

	waitStableForTest(t, bc2, pid)

	// Clean replay: HasChunk misses (Part A) so the bytes are re-appended,
	// fsynced, then indexed.
	if err := bc2.ForceRollupForTest(ctx, pid); err != nil {
		t.Fatalf("ForceRollupForTest (recovery pass): %v", err)
	}

	// Part B — the chunk is durably stored and reads back byte-identical.
	if ok, err := bc2.HasChunk(ctx, h); err != nil || !ok {
		t.Fatalf("HasChunk after recovery pass = (%v, %v); want (true, nil)", ok, err)
	}
	got, err := bc2.ReadChunk(ctx, h)
	if err != nil {
		t.Fatalf("ReadChunk after recovery: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("ReadChunk mismatch after recovery: got %d bytes, want %d", len(got), len(payload))
	}
}

// TestRollup_TransientSyncFailure_ReadableWithoutRestart guards the read
// availability of an acknowledged write across an IN-PROCESS transient blob
// fsync failure — no crash, no restart.
//
// The rollup Phase C commit sequence must flush + commit the log-blob index
// BEFORE it destructively trims the in-memory log index (the fence advance).
// If the trim runs first and the fsync then fails, the pass returns having
// already dropped the interval's index entries while the FileChunk manifest
// rows were written earlier in Phase B. The rollup worker keeps running and
// retries the pass, but with the entries gone the retry sees no log-index
// coverage for the still-dirty interval and drops it without re-appending —
// so the chunk is never committed to the local index. A read then misses BOTH
// ReadPayloadAt paths: step 1 (append-log replay) finds no index entries and
// step 2 (FileChunk manifest -> local index Get) finds no committed location,
// yielding ErrFileChunkNotFound and a remote-fallback zero-fill of data the
// caller already acknowledged — a read hole that only heals on the next
// process restart.
//
// This test injects a one-shot blob fsync failure, asserts the payload is
// STILL readable in-window (before any retry), then lets the same store retry
// the pass and asserts the payload reads back byte-identical. It FAILS against
// a trim-before-sync ordering (both reads miss) and PASSES once sync + commit
// are fenced ahead of the trim.
func TestRollup_TransientSyncFailure_ReadableWithoutRestart(t *testing.T) {
	ctx := context.Background()

	// Real in-memory FileChunk store + persister so ReadPayloadAt's CAS-manifest
	// path (step 2) can resolve rolled-up bytes after the fence trims the log
	// index entries — the only read path left once a pass succeeds. fbs also
	// serves as the mandatory LocalChunkIndex and RollupStore.
	fbs := newRollupMemFileChunkStore()
	persister := func(pctx context.Context, payloadID string, blocks []block.ChunkRef, _ block.ObjectID) error {
		return fbs.persist(pctx, payloadID, blocks)
	}
	opts := FSStoreOptions{
		ObjectIDPersister: persister,
		MaxLogBytes:       1 << 30,
		StabilizationMS:   1,
		RollupWorkers:     1,
	}

	bc := newFSStoreForTestWithFBS(t, fbs, opts)

	const pid = "logblob/durability/transient-sync"
	// < MinChunkSize (1 MiB) so the write rolls up as exactly one chunk.
	payload := bytes.Repeat([]byte{0xD7}, 4096)

	if err := bc.AppendWrite(ctx, pid, payload, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	waitStableForTest(t, bc, pid)

	// Fail the blob fsync exactly once, mid-pass. The pass appends the chunk to
	// the blob and writes the manifest rows in Phase B, then the injected fault
	// aborts Phase C before the fence advance.
	var failNext atomic.Bool
	failNext.Store(true)
	rollupPreSyncFailHook = func() error {
		if failNext.CompareAndSwap(true, false) {
			return errors.New("injected: transient blob fsync failure")
		}
		return nil
	}
	defer func() { rollupPreSyncFailHook = nil }()

	if err := bc.ForceRollupForTest(ctx, pid); err == nil {
		t.Fatal("ForceRollupForTest: want injected sync failure, got nil")
	}

	// In-window read: the failed pass must NOT have trimmed the log index, so
	// step 1 (append-log replay) still resolves the payload. Under a
	// trim-before-sync ordering the entries are gone and this read holes out.
	readback := func(stage string) {
		t.Helper()
		dest := make([]byte, len(payload))
		n, rerr := bc.ReadPayloadAt(ctx, pid, dest, 0)
		if rerr != nil {
			t.Fatalf("ReadPayloadAt (%s) after transient sync failure: %v "+
				"(acknowledged data holed out to remote zero-fill)", stage, rerr)
		}
		if n != len(payload) || !bytes.Equal(dest, payload) {
			t.Fatalf("ReadPayloadAt (%s) mismatch: got %d bytes, want %d", stage, n, len(payload))
		}
	}
	readback("in-window")

	// Clear the fault and let the SAME store retry the pass, exactly as the
	// rollup worker would. The interval is still dirty (Phase C never consumed
	// it), so the retry re-rolls it cleanly and commits the index.
	rollupPreSyncFailHook = nil
	waitStableForTest(t, bc, pid)
	if err := bc.ForceRollupForTest(ctx, pid); err != nil {
		t.Fatalf("ForceRollupForTest (in-process retry): %v", err)
	}
	readback("after-retry")
}
