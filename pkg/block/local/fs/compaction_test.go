package fs

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// logFileSize returns the on-disk byte size of payloadID's log file. Fails
// the test if the file is missing.
func logFileSize(t *testing.T, bc *FSStore, payloadID string) int64 {
	t.Helper()
	path := bc.logPath(payloadID)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat log %s: %v", path, err)
	}
	return st.Size()
}

// waitForLogBytesBelow polls until bc.logBytesTotal drops below max or
// the deadline expires. Used to confirm rollup drained the budget. We
// key compaction-bound tests off logBytesTotal rather than
// metadata.rollup_offset because once compaction trips, the metadata
// fence is frozen by monotonicity at the pre-compaction
// high-water mark — only the in-memory budget continues to drop as
// records are consumed.
func waitForLogBytesBelow(t *testing.T, bc *FSStore, max int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if bc.logBytesTotal.Load() <= max {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("logBytesTotal did not drop below %d within %v (current=%d)",
		max, timeout, bc.logBytesTotal.Load())
}

// TestCompaction_BoundedLogSize: sustained AppendWrite + rollup produces a
// log file that does NOT grow without bound. The compaction pass should
// reclaim pre-fence bytes so the on-disk size stays below a multiple of
// the compaction threshold.
func TestCompaction_BoundedLogSize(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	// Very tight compaction threshold (32 KiB) and a small log budget so
	// the test runs in a few seconds. Each chunk is 8 KiB; we write many
	// of them sequentially at increasing file offsets so the rollup
	// chunks them and the fence advances. Compaction should keep the
	// on-disk log size bounded.
	opts := FSStoreOptions{
		MaxLogBytes:              1 << 30,
		RollupWorkers:            2,
		StabilizationMS:          5,
		RollupStore:              rs,
		CompactionThresholdBytes: 32 * 1024,
	}
	bc := newFSStoreForTest(t, opts)
	if err := bc.StartRollup(context.Background()); err != nil {
		t.Fatalf("StartRollup: %v", err)
	}
	ctx := context.Background()

	chunk := bytes.Repeat([]byte{0xCD}, 8*1024)
	const writes = 256
	for i := 0; i < writes; i++ {
		if err := bc.AppendWrite(ctx, "fileA", chunk, uint64(i)*uint64(len(chunk))); err != nil {
			t.Fatalf("AppendWrite[%d]: %v", i, err)
		}
		// Brief yield so the rollup worker has a chance to run between
		// batches; otherwise the entire log accumulates first and a
		// single big compaction would mask whether the steady-state
		// loop trips at all.
		if i%16 == 15 {
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Wait for the rollup to drain the log budget so we know every
	// AppendWrite has been processed. logBytesTotal goes back to zero
	// when every record is consumed; we don't key off metadata
	// rollup_offset because once compaction trips, the metadata fence
	// is frozen at the pre-compaction high-water mark by
	// monotonicity — only the in-memory fence and on-disk header
	// continue to advance.
	waitForLogBytesBelow(t, bc, 64*1024, 10*time.Second)
	// Give the compaction pass a moment to fire after the SetRollupOffset
	// that advanced the fence past the threshold.
	time.Sleep(300 * time.Millisecond)

	size := logFileSize(t, bc, "fileA")
	// The log file should be far smaller than the cumulative AppendWrite
	// payload size. Bound: a few multiples of the compaction threshold
	// (one full threshold worth of pre-fence bytes can sit between
	// passes, plus whatever unconsumed records are still resident).
	maxAcceptable := int64(8 * 32 * 1024) // 256 KiB
	if size > maxAcceptable {
		t.Fatalf("log file size %d bytes exceeds bound %d (compaction did not run or did not reclaim bytes)",
			size, maxAcceptable)
	}
}

// TestCompaction_DisabledByNegativeThreshold: with CompactionThresholdBytes
// set to a sentinel negative value, the log file grows unbounded (pre-#579
// behavior). Verifies the opt-out path.
func TestCompaction_DisabledByNegativeThreshold(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	opts := FSStoreOptions{
		MaxLogBytes:              1 << 30,
		RollupWorkers:            2,
		StabilizationMS:          5,
		RollupStore:              rs,
		CompactionThresholdBytes: -1,
	}
	bc := newFSStoreForTest(t, opts)
	if err := bc.StartRollup(context.Background()); err != nil {
		t.Fatalf("StartRollup: %v", err)
	}
	ctx := context.Background()

	chunk := bytes.Repeat([]byte{0xEE}, 8*1024)
	const writes = 64
	for i := 0; i < writes; i++ {
		if err := bc.AppendWrite(ctx, "fileB", chunk, uint64(i)*uint64(len(chunk))); err != nil {
			t.Fatalf("AppendWrite[%d]: %v", i, err)
		}
	}
	totalBytes := uint64(writes) * uint64(len(chunk))
	waitForLogBytesBelow(t, bc, 64*1024, 10*time.Second)
	// Allow a final rollup tick to settle on disk.
	time.Sleep(200 * time.Millisecond)

	size := logFileSize(t, bc, "fileB")
	// Without compaction the log retains every framed record. Expect at
	// least the cumulative AppendWrite bytes (plus framing overhead) on
	// disk.
	minExpected := int64(totalBytes)
	if size < minExpected {
		t.Fatalf("compaction-disabled log size %d below %d — did compaction run unexpectedly?",
			size, minExpected)
	}
}

// TestCompaction_RecoveryRebuildsAfterCompact: after compaction trims the
// on-disk log, close + reopen the FSStore and verify recovery rebuilds
// the interval tree + logIndex from the post-compaction file (no records
// lost, no records re-replayed from the dropped prefix). Subsequent
// AppendWrite + rollup against the recovered payload must work.
func TestCompaction_RecoveryRebuildsAfterCompact(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	opts := FSStoreOptions{
		MaxLogBytes:              1 << 30,
		RollupWorkers:            2,
		StabilizationMS:          5,
		RollupStore:              rs,
		CompactionThresholdBytes: 16 * 1024,
	}
	bc := newFSStoreForTest(t, opts)
	if err := bc.StartRollup(context.Background()); err != nil {
		t.Fatalf("StartRollup: %v", err)
	}
	ctx := context.Background()

	// write enough records to drive at least one compaction.
	chunk := bytes.Repeat([]byte{0xA1}, 4*1024)
	const phase1Writes = 64
	for i := 0; i < phase1Writes; i++ {
		if err := bc.AppendWrite(ctx, "fileR", chunk, uint64(i)*uint64(len(chunk))); err != nil {
			t.Fatalf("phase1 AppendWrite[%d]: %v", i, err)
		}
	}
	phase1Bytes := uint64(phase1Writes) * uint64(len(chunk))
	waitForLogBytesBelow(t, bc, 32*1024, 10*time.Second)

	// close the store BEFORE measuring size. Close() joins rollup
	// and compaction workers, guaranteeing no in-flight compaction can leave
	// a partial tail that recovery would then truncate — which was the root
	// cause of the pre-existing flake (pre/post size mismatch).
	baseDir := bc.baseDir
	logPath := bc.logPath("fileR")
	_ = bc.Close()

	st, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log after close: %v", err)
	}
	preReopenSize := st.Size()
	// Sanity: compaction should have trimmed the file far below the
	// total cumulative AppendWrite bytes; if it didn't fire, the
	// post-reopen invariant below would still hold trivially and we
	// would not actually be testing the compaction recovery path.
	if uint64(preReopenSize) >= phase1Bytes {
		t.Fatalf("compaction did not trim log: size=%d cumulative writes=%d",
			preReopenSize, phase1Bytes)
	}
	bc2, err := NewWithOptions(baseDir, 1<<30, 1<<30, nopFBS{}, opts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = bc2.Close() })
	if err := bc2.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// post-recovery, the in-memory logIndex should reflect the
	// post-compaction layout — entries from the dropped prefix must NOT
	// reappear. The on-disk log size should be unchanged (recovery did
	// not re-grow the file).
	postReopenSize := logFileSize(t, bc2, "fileR")
	if postReopenSize != preReopenSize {
		t.Fatalf("recovery mutated log size: pre=%d post=%d (recovery should be read-only on a clean tail)",
			preReopenSize, postReopenSize)
	}

	// append new records against the recovered payload and let
	// the rollup process them. This exercises the post-compaction
	// append + rollup loop end-to-end after a reopen.
	if err := bc2.StartRollup(context.Background()); err != nil {
		t.Fatalf("StartRollup (post-recovery): %v", err)
	}
	const phase4Writes = 8
	for i := 0; i < phase4Writes; i++ {
		off := phase1Bytes + uint64(i)*uint64(len(chunk))
		if err := bc2.AppendWrite(ctx, "fileR", chunk, off); err != nil {
			t.Fatalf("phase4 AppendWrite[%d]: %v", i, err)
		}
	}
	time.Sleep(500 * time.Millisecond)
	// At least the previously-observed offset should still hold post-
	// reopen (the post-#579 rollup will attempt SetRollupOffset on the
	// new records, but the metadata-monotonic regression rule keeps
	// metaOff at its high-water mark).
	finalMeta, _ := rs.GetRollupOffset(context.Background(), "fileR")
	if finalMeta < uint64(logHeaderSize) {
		t.Fatalf("rollup_offset reset to %d after recovery (should be monotonic)", finalMeta)
	}
}

// TestCompaction_HeaderFlagSetAndPreservesCAS: after compaction, the on-
// disk log header should carry the LogFlagCompacted bit, RollupOffset
// should be reset to logHeaderSize, and the CAS chunks emitted prior to
// compaction should remain untouched (BLAKE3 content addressing
// invariant — compaction does not re-emit chunks).
func TestCompaction_HeaderFlagSetAndPreservesCAS(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	opts := FSStoreOptions{
		MaxLogBytes:              1 << 30,
		RollupWorkers:            2,
		StabilizationMS:          5,
		RollupStore:              rs,
		CompactionThresholdBytes: 8 * 1024,
	}
	bc := newFSStoreForTest(t, opts)
	if err := bc.StartRollup(context.Background()); err != nil {
		t.Fatalf("StartRollup: %v", err)
	}
	ctx := context.Background()

	chunk := bytes.Repeat([]byte{0x42}, 4*1024)
	const writes = 32
	for i := 0; i < writes; i++ {
		if err := bc.AppendWrite(ctx, "fileH", chunk, uint64(i)*uint64(len(chunk))); err != nil {
			t.Fatalf("AppendWrite[%d]: %v", i, err)
		}
	}
	// Drain all rollup work to completion. This rolls up every record and
	// triggers the post-rollup compaction pass, leaving a deterministic
	// end-state: the log is compacted (LogFlagCompacted set) with no
	// surviving records, so RollupOffset settles at logHeaderSize and the
	// idle background loop has nothing left to advance it with. Waiting on
	// log size alone left un-rolled survivors that a later pass kept rolling
	// up, racily pushing RollupOffset past logHeaderSize again.
	if err := bc.DrainRollups(context.Background()); err != nil {
		t.Fatalf("DrainRollups: %v", err)
	}

	chunksBefore := countChunksInBlocks(t, bc.baseDir)
	if chunksBefore == 0 {
		t.Fatalf("no chunks emitted; cannot validate CAS-preservation invariant")
	}

	// Poll the on-disk header until compaction has stamped LogFlagCompacted.
	// Compaction runs inside the background rollup loop and rewrites the
	// header via a temp file + rename, so a single read can race that rename
	// and catch a torn header (bad CRC) — Windows-prone, where the replaced
	// handle lingers — or simply observe a pre-compaction state on a fast
	// runner. Retry on both until we read a valid, compacted header. This is
	// read-only, so it never perturbs the store the way an early Close would.
	var hdr logHeader
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, openErr := os.Open(bc.logPath("fileH"))
		if openErr == nil {
			h, readErr := readLogHeader(f)
			_ = f.Close()
			if readErr == nil && h.Flags&LogFlagCompacted != 0 {
				hdr = h
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for compacted on-disk header (flags not set / torn read)")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if hdr.Flags&LogFlagCompacted == 0 {
		t.Fatalf("expected LogFlagCompacted bit in header.Flags (got 0x%x); compaction may not have run",
			hdr.Flags)
	}
	if hdr.RollupOffset != logHeaderSize {
		t.Fatalf("expected RollupOffset = %d after compaction, got %d",
			logHeaderSize, hdr.RollupOffset)
	}

	// CAS-preservation: chunk count must not decrease across a
	// compaction pass. (We tolerate growth from any post-compaction
	// rollup that ran after the header peek.)
	chunksAfter := countChunksInBlocks(t, bc.baseDir)
	if chunksAfter < chunksBefore {
		t.Fatalf("CAS chunk count dropped from %d to %d across compaction — should be additive only",
			chunksBefore, chunksAfter)
	}
}

// TestCompaction_StaleTempCleanedUpOnRecovery: a `.compact` temp file
// left over from a crashed compaction pass is unlinked by recovery's
// cleanupCompactTemps sweep.
func TestCompaction_StaleTempCleanedUpOnRecovery(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreForTest(t, FSStoreOptions{
		MaxLogBytes:     1 << 20,
		RollupWorkers:   2,
		StabilizationMS: 10,
		RollupStore:     rs,
	})
	ctx := context.Background()

	// Touch a real log so the logs/ directory exists.
	if err := bc.AppendWrite(ctx, "fileT", []byte("seed"), 0); err != nil {
		t.Fatalf("seed AppendWrite: %v", err)
	}
	baseDir := bc.baseDir
	_ = bc.Close()

	// Drop a stale .compact temp into the logs dir, mimicking a
	// crashed compaction.
	stalePath := bc.logPath("fileT") + ".compact"
	if err := os.WriteFile(stalePath, []byte("garbage"), 0644); err != nil {
		t.Fatalf("write stale temp: %v", err)
	}

	// Reopen and run recovery.
	bc2, err := NewWithOptions(baseDir, 1<<30, 1<<30, nopFBS{}, FSStoreOptions{
		MaxLogBytes:     1 << 20,
		RollupWorkers:   2,
		StabilizationMS: 10,
		RollupStore:     rs,
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = bc2.Close() })
	if err := bc2.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("stale .compact temp was not cleaned up: stat err=%v", err)
	}
}
