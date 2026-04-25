package fs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestDelete_FreshPayload_UnlinksLog is the baseline happy-path check:
// AppendWrite lands a record, DeleteAppendLog runs, the on-disk log file
// disappears and FSStore per-file state is cleared.
func TestDelete_FreshPayload_UnlinksLog(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreWithRS(t, rs)
	ctx := context.Background()

	if err := bc.AppendWrite(ctx, "file1", []byte("hello"), 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	logPath := filepath.Join(bc.baseDir, "logs", "file1.log")
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("log file not created before delete: %v", err)
	}

	if err := bc.DeleteAppendLog(ctx, "file1"); err != nil {
		t.Fatalf("DeleteAppendLog: %v", err)
	}

	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("log file not unlinked after DeleteAppendLog: stat err=%v", err)
	}

	// Per-file state cleared. FIX-8: the tombstone is intentionally
	// PRESERVED (lifetime of the FSStore) so a deleted payloadID can
	// never be silently resurrected; assert it is set, not cleared.
	bc.logsMu.RLock()
	_, hasFD := bc.logFDs["file1"]
	_, hasLock := bc.logLocks["file1"]
	_, hasTree := bc.dirtyIntervals["file1"]
	_, hasTomb := bc.tombstones["file1"]
	_, hasTrunc := bc.truncations["file1"]
	bc.logsMu.RUnlock()
	if hasFD || hasLock || hasTree || hasTrunc {
		t.Fatalf("per-file state not cleared: fd=%v lock=%v tree=%v trunc=%v",
			hasFD, hasLock, hasTree, hasTrunc)
	}
	if !hasTomb {
		t.Fatalf("tombstone unexpectedly cleared after DeleteAppendLog (FIX-8 requires permanence)")
	}
}

// TestDelete_Idempotent_OnMissingFile verifies DeleteAppendLog on a
// payload that never had a log returns nil.
func TestDelete_Idempotent_OnMissingFile(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreWithRS(t, rs)
	if err := bc.DeleteAppendLog(context.Background(), "never-written"); err != nil {
		t.Fatalf("DeleteAppendLog on missing payload: got %v want nil", err)
	}
	// Second call is also a no-op.
	if err := bc.DeleteAppendLog(context.Background(), "never-written"); err != nil {
		t.Fatalf("DeleteAppendLog idempotent second call: got %v want nil", err)
	}
}

// TestDelete_OnMissingFile_NoError exercises FIX-17's ENOENT branch
// specifically: a payload that DID have a log on disk, was deleted once
// (clearing in-memory state), then has its log file deleted out-of-band
// before a SECOND DeleteAppendLog call. The second call's os.Remove
// returns ENOENT — FIX-17 must treat that as benign and return nil
// (idempotency preserved).
//
// Because step-5 cleanup wipes lf from logFDs after the first delete,
// the second delete's snapshot returns nil lf and the os.Remove path is
// not actually re-entered — but the user-facing nil-return contract is
// what callers depend on. This test guards that contract.
func TestDelete_OnMissingFile_NoError(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreWithRS(t, rs)
	ctx := context.Background()

	// Create a real log on disk.
	if err := bc.AppendWrite(ctx, "twice-deleted", []byte("payload"), 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	logPath := filepath.Join(bc.baseDir, "logs", "twice-deleted.log")
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("log not created: %v", err)
	}

	// First delete: real unlink.
	if err := bc.DeleteAppendLog(ctx, "twice-deleted"); err != nil {
		t.Fatalf("first DeleteAppendLog: %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("log not unlinked: %v", err)
	}

	// Second delete: file already gone. Even if some future change
	// re-snapshots a stale lf and reaches os.Remove, FIX-17 must still
	// surface nil for ENOENT.
	if err := bc.DeleteAppendLog(ctx, "twice-deleted"); err != nil {
		t.Fatalf("second DeleteAppendLog (file already gone): got %v, want nil — FIX-17 idempotency violation", err)
	}
}

// TestDelete_DisabledFlag_NoOp: when useAppendLog is false, DeleteAppendLog
// returns nil without touching any state — the legacy path owns delete in
// that mode.
func TestDelete_DisabledFlag_NoOp(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{UseAppendLog: false})
	if err := bc.DeleteAppendLog(context.Background(), "any"); err != nil {
		t.Fatalf("DeleteAppendLog flag-off: got %v want nil", err)
	}
}

// TestDelete_ClosedStore_ReturnsErrStoreClosed verifies the close guard
// consistent with AppendWrite's contract.
func TestDelete_ClosedStore_ReturnsErrStoreClosed(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreWithRS(t, rs)
	_ = bc.Close()
	if err := bc.DeleteAppendLog(context.Background(), "any"); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("DeleteAppendLog on closed store: got %v want ErrStoreClosed", err)
	}
}

// TestDelete_DuringActiveWriters_SomeReturnErrDeleted kicks off a burst
// of AppendWrites while DeleteAppendLog runs mid-stream. At least one
// write that observes the tombstone must surface ErrDeleted; no panic
// or data race.
//
// FIX-8 update: the tombstone is now PERMANENT (see
// TestDelete_Then_Write_RejectsReuse). Writers that start AFTER
// DeleteAppendLog returns get ErrDeleted too. This test still asserts
// the original "at least one mid-stream writer sees ErrDeleted" property
// — the post-delete writers reinforce it rather than violate it.
func TestDelete_DuringActiveWriters_SomeReturnErrDeleted(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreWithRS(t, rs)
	ctx := context.Background()

	// Seed the log so the delete has something to tear down and so
	// subsequent writers hit an existing per-file mutex.
	if err := bc.AppendWrite(ctx, "file1", []byte("seed"), 0); err != nil {
		t.Fatalf("seed AppendWrite: %v", err)
	}

	const goroutines = 20
	var wg sync.WaitGroup
	var deletedCount atomic.Int64
	var otherErrs atomic.Int64
	stop := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(gi int) {
			defer wg.Done()
			payload := bytes.Repeat([]byte{byte(gi)}, 512)
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				err := bc.AppendWrite(ctx, "file1", payload, uint64(gi*4096+j)*1024)
				if err == nil {
					continue
				}
				if errors.Is(err, ErrDeleted) {
					deletedCount.Add(1)
					return
				}
				if errors.Is(err, ErrStoreClosed) {
					return
				}
				otherErrs.Add(1)
				return
			}
		}(i)
	}

	// Let writers accumulate, then run the delete, then stop writers.
	time.Sleep(5 * time.Millisecond)
	if err := bc.DeleteAppendLog(ctx, "file1"); err != nil {
		t.Fatalf("DeleteAppendLog: %v", err)
	}
	close(stop)
	wg.Wait()

	if otherErrs.Load() > 0 {
		t.Fatalf("unexpected non-ErrDeleted errors: %d", otherErrs.Load())
	}
	if deletedCount.Load() == 0 {
		t.Fatalf("expected at least one AppendWrite to observe ErrDeleted; got 0")
	}
}

// TestDelete_DuringActiveRollup_NoMetadataZombie — plan-checker Blocker 3
// verification. A rollup is started for payloadID "target"; the rollup
// worker picks it up while DeleteAppendLog runs concurrently. After
// Delete returns, assert:
//
//	(a) rollup_offset in RollupStore is 0 — no zombie metadata row.
//	(b) The log file is unlinked.
//	(c) Rollup did not error the test (benign abort only).
//
// Race determinism: rollupFile holds the per-file mutex through the
// entire StoreChunk → SetRollupOffset path (plan 06), so
// DeleteAppendLog's mutex.Lock() either (a) runs first and the rollup
// worker immediately bails on the entry tombstone check, or (b) runs
// after the rollup pre-commit tombstone re-check which also bails
// before SetRollupOffset. Both paths leave GetRollupOffset == 0.
func TestDelete_DuringActiveRollup_NoMetadataZombie(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	// Tiny stabilization so records become eligible for rollup within
	// a few ms.
	bc := newFSStoreForTest(t, FSStoreOptions{
		UseAppendLog:    true,
		MaxLogBytes:     1 << 30,
		RollupWorkers:   2,
		StabilizationMS: 2,
		RollupStore:     rs,
	})
	ctx := context.Background()

	// Seed several records so the rollup has meaningful work.
	payload := bytes.Repeat([]byte{0xAA}, 64*1024)
	for i := 0; i < 16; i++ {
		if err := bc.AppendWrite(ctx, "target", payload, uint64(i)*64*1024); err != nil {
			t.Fatalf("AppendWrite seed %d: %v", i, err)
		}
	}

	// Wait past the stabilization window so EarliestStable has entries.
	time.Sleep(10 * time.Millisecond)

	// Launch a rollup attempt and a delete in parallel. rollupFile is
	// invoked directly (not via StartRollup) so the test has control
	// over exactly one rollup goroutine's lifecycle.
	var wg sync.WaitGroup
	var rollupErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		rollupErr = bc.rollupFile(ctx, "target")
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := bc.DeleteAppendLog(ctx, "target"); err != nil {
			t.Errorf("DeleteAppendLog: %v", err)
		}
	}()
	wg.Wait()

	if rollupErr != nil {
		t.Fatalf("rollupFile returned unexpected error (expected benign nil abort): %v", rollupErr)
	}

	// (a) no zombie metadata row.
	off, err := rs.GetRollupOffset(ctx, "target")
	if err != nil {
		t.Fatalf("GetRollupOffset: %v", err)
	}
	if off != 0 {
		t.Fatalf("zombie rollup_offset row for deleted payload: got %d want 0", off)
	}

	// (b) log file unlinked.
	logPath := filepath.Join(bc.baseDir, "logs", "target.log")
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("log still present after delete during rollup: stat err=%v", err)
	}
}

// TestDelete_CrashBetweenMetadataAndUnlink_OrphanSwept simulates the
// crash window between clearing metadata and unlinking the log:
// manually create a log file with no matching metadata, age its mtime
// past the orphan sweep threshold, then call Recover and assert the
// orphan was swept.
func TestDelete_CrashBetweenMetadataAndUnlink_OrphanSwept(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	dir := t.TempDir()

	// First, create an FSStore with append-log on, write one record so
	// a log file exists, then close.
	bc1, err := NewWithOptions(dir, 1<<30, 1<<30, nopFBS{}, FSStoreOptions{
		UseAppendLog:           true,
		MaxLogBytes:            1 << 30,
		RollupWorkers:          2,
		StabilizationMS:        10,
		RollupStore:            rs,
		OrphanLogMinAgeSeconds: 1, // short window so the test can age past it
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	if werr := bc1.AppendWrite(context.Background(), "crashed", []byte("data"), 0); werr != nil {
		t.Fatalf("AppendWrite: %v", werr)
	}
	logPath := filepath.Join(dir, "logs", "crashed.log")
	if _, serr := os.Stat(logPath); serr != nil {
		t.Fatalf("log not created: %v", serr)
	}
	_ = bc1.Close()

	// Age the file mtime past the orphan sweep threshold.
	past := time.Now().Add(-10 * time.Second)
	if err := os.Chtimes(logPath, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Crash-recovery pass on a fresh FSStore with the same RollupStore.
	// rs has no entry for "crashed" (metadata step never committed) and
	// nopFBS has no block-0 entry — so it qualifies as orphan.
	bc2, err := NewWithOptions(dir, 1<<30, 1<<30, nopFBS{}, FSStoreOptions{
		UseAppendLog:           true,
		MaxLogBytes:            1 << 30,
		RollupWorkers:          2,
		StabilizationMS:        10,
		RollupStore:            rs,
		OrphanLogMinAgeSeconds: 1,
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = bc2.Close() }()
	if err := bc2.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("orphan log not swept by recovery: stat err=%v", err)
	}
}

// TestTruncate_DropsIntervalsAbove: records at offsets 0, 4096, 8192,
// 16384; truncate to 8192; tree has entries only for offsets < 8192.
func TestTruncate_DropsIntervalsAbove(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreWithRS(t, rs)
	ctx := context.Background()

	payload := []byte("data")
	for _, off := range []uint64{0, 4096, 8192, 16384} {
		if err := bc.AppendWrite(ctx, "f1", payload, off); err != nil {
			t.Fatalf("AppendWrite off=%d: %v", off, err)
		}
	}

	if err := bc.TruncateAppendLog(ctx, "f1", 8192); err != nil {
		t.Fatalf("TruncateAppendLog: %v", err)
	}

	bc.logsMu.RLock()
	tree := bc.dirtyIntervals["f1"]
	bc.logsMu.RUnlock()
	if tree == nil {
		t.Fatal("interval tree missing after truncate")
	}

	var offsets []uint64
	tree.t.Ascend(func(iv *interval) bool {
		offsets = append(offsets, iv.Offset)
		return true
	})
	// Expect only offsets strictly less than 8192 to survive.
	for _, off := range offsets {
		if off >= 8192 {
			t.Fatalf("offset %d survived DropAbove(8192); tree=%v", off, offsets)
		}
	}
	if len(offsets) != 2 {
		t.Fatalf("expected 2 intervals (offsets 0, 4096) after truncate; got %v", offsets)
	}
}

// TestTruncate_ClipsStraddling: single record at offset 100 length 200
// (covers [100, 300)); truncate to 150 produces a clipped tree entry of
// length 50.
func TestTruncate_ClipsStraddling(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreWithRS(t, rs)
	ctx := context.Background()

	payload := bytes.Repeat([]byte{0x5A}, 200)
	if err := bc.AppendWrite(ctx, "f1", payload, 100); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	if err := bc.TruncateAppendLog(ctx, "f1", 150); err != nil {
		t.Fatalf("TruncateAppendLog: %v", err)
	}

	bc.logsMu.RLock()
	tree := bc.dirtyIntervals["f1"]
	bc.logsMu.RUnlock()

	var entries []interval
	tree.t.Ascend(func(iv *interval) bool {
		entries = append(entries, *iv)
		return true
	})
	if len(entries) != 1 {
		t.Fatalf("expected 1 clipped entry, got %d: %v", len(entries), entries)
	}
	got := entries[0]
	if got.Offset != 100 || got.Length != 50 {
		t.Fatalf("clip wrong: got offset=%d length=%d, want offset=100 length=50", got.Offset, got.Length)
	}
}

// TestTruncate_DisabledFlag_NoOp: TruncateAppendLog is a no-op when
// useAppendLog is false.
func TestTruncate_DisabledFlag_NoOp(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{UseAppendLog: false})
	if err := bc.TruncateAppendLog(context.Background(), "any", 100); err != nil {
		t.Fatalf("TruncateAppendLog flag-off: got %v want nil", err)
	}
}

// TestTruncate_ClosedStore_ReturnsErrStoreClosed mirrors the close
// guard test on DeleteAppendLog.
func TestTruncate_ClosedStore_ReturnsErrStoreClosed(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreWithRS(t, rs)
	_ = bc.Close()
	if err := bc.TruncateAppendLog(context.Background(), "any", 100); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("TruncateAppendLog on closed store: got %v want ErrStoreClosed", err)
	}
}

// TestTruncate_Rollup_SkipsBeyondBoundary: run rollup after truncate;
// verify chunks emitted only contain data up to newSize. We do this by
// counting chunks in blocks/ after rollup and by asserting the rollup
// did not error.
func TestTruncate_Rollup_SkipsBeyondBoundary(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreForTest(t, FSStoreOptions{
		UseAppendLog:    true,
		MaxLogBytes:     1 << 30,
		RollupWorkers:   2,
		StabilizationMS: 2,
		RollupStore:     rs,
	})
	ctx := context.Background()

	// Three records: two below the boundary, one entirely past it.
	belowA := bytes.Repeat([]byte{0x11}, 4096)
	belowB := bytes.Repeat([]byte{0x22}, 4096)
	above := bytes.Repeat([]byte{0x33}, 4096)

	if err := bc.AppendWrite(ctx, "t1", belowA, 0); err != nil {
		t.Fatalf("AppendWrite below A: %v", err)
	}
	if err := bc.AppendWrite(ctx, "t1", belowB, 4096); err != nil {
		t.Fatalf("AppendWrite below B: %v", err)
	}
	if err := bc.AppendWrite(ctx, "t1", above, 16384); err != nil {
		t.Fatalf("AppendWrite above: %v", err)
	}

	// Truncate to 8192 — the record at 16384 must not contribute to
	// emitted chunks.
	if err := bc.TruncateAppendLog(ctx, "t1", 8192); err != nil {
		t.Fatalf("TruncateAppendLog: %v", err)
	}

	// Let stabilization elapse.
	time.Sleep(20 * time.Millisecond)

	if err := bc.rollupFile(ctx, "t1"); err != nil {
		t.Fatalf("rollupFile after truncate: %v", err)
	}

	// Read back every chunk on disk; their concatenation must NOT
	// contain any 0x33 bytes (the above-boundary payload).
	blocksDir := filepath.Join(bc.baseDir, "blocks")
	var chunkBytes []byte
	_ = filepath.WalkDir(blocksDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		chunkBytes = append(chunkBytes, data...)
		return nil
	})
	if bytes.Contains(chunkBytes, []byte{0x33, 0x33, 0x33, 0x33}) {
		t.Fatalf("emitted chunks contain above-boundary bytes (0x33); truncation filter failed")
	}
	// And the below-boundary content IS emitted.
	if !bytes.Contains(chunkBytes, []byte{0x11, 0x11, 0x11, 0x11}) {
		t.Fatalf("emitted chunks missing below-boundary content (0x11)")
	}
}

// TestDelete_Then_Write_RejectsReuse verifies the FIX-8 invariant:
// tombstones are PERMANENT for the lifetime of the FSStore. After
// DeleteAppendLog returns, any subsequent AppendWrite on the same
// payloadID returns ErrDeleted. Callers that need a "delete + recreate"
// flow must allocate a fresh payloadID — the metadata layer's opaque
// file-handle abstraction already does this transparently.
//
// Previously this test asserted the opposite (that re-use succeeded);
// the change reflects the FIX-8 semantic shift, which closes a
// re-creation race where a stale wakeup could resurrect a deleted
// payloadID with on-disk state divergent from metadata's view.
func TestDelete_Then_Write_RejectsReuse(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreWithRS(t, rs)
	ctx := context.Background()

	if err := bc.AppendWrite(ctx, "f1", []byte("v1"), 0); err != nil {
		t.Fatalf("AppendWrite pre-delete: %v", err)
	}
	if err := bc.DeleteAppendLog(ctx, "f1"); err != nil {
		t.Fatalf("DeleteAppendLog: %v", err)
	}

	// Re-append on the SAME payloadID after delete — must be rejected.
	if err := bc.AppendWrite(ctx, "f1", []byte("v2"), 0); !errors.Is(err, ErrDeleted) {
		t.Fatalf("AppendWrite post-delete on reused payloadID: got %v want ErrDeleted", err)
	}
	logPath := filepath.Join(bc.baseDir, "logs", "f1.log")
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("log unexpectedly re-created after rejected reuse: stat err=%v", err)
	}

	// A FRESH payloadID after delete — must succeed (the abstraction the
	// metadata layer relies on for file recreation).
	if err := bc.AppendWrite(ctx, "f1-gen2", []byte("v2"), 0); err != nil {
		t.Fatalf("AppendWrite on fresh payloadID after delete: got %v want nil", err)
	}
	freshPath := filepath.Join(bc.baseDir, "logs", "f1-gen2.log")
	if _, err := os.Stat(freshPath); err != nil {
		t.Fatalf("log not created for fresh payloadID: %v", err)
	}
}
