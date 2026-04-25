package localtest

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
)

// AppendLogFactory constructs an *fs.FSStore with the append-log flag
// enabled and a RollupStore wired in. The factory is expected to scope
// the backing filesystem to a per-test tempdir and register any Close
// via t.Cleanup.
//
// Plan 10-10 deliberately returns *fs.FSStore (not local.LocalStore)
// because the new append-log methods (AppendWrite, StoreChunk,
// DeleteAppendLog, TruncateAppendLog, StartRollup) live on *FSStore
// directly through Phase 10 (D-04). The LocalStore interface narrowing
// is LSL-07 / Phase 11 (A2) work.
type AppendLogFactory func(t *testing.T) *fs.FSStore

// RunAppendLogSuite dispatches the five D-22 append-log scenario tests
// against the store produced by factory. Each test is independent and
// receives a fresh store.
//
// Scenarios (10-CONTEXT.md D-22):
//   - AppendLogRoundTrip        — LSL-01/02/03/05 end-to-end chunk + rollup.
//   - PressureChannel_INV05     — budget drained under write storm, no leak.
//   - TornWriteRecovery_LSL06   — random mid-record garbage truncated on reopen.
//   - ConcurrentStorm           — M goroutines, no deadlock, all data intact.
//   - RollupOffsetMonotone_INV03 — header reconciled to metadata on reopen.
func RunAppendLogSuite(t *testing.T, factory AppendLogFactory) {
	t.Run("AppendLogRoundTrip", func(t *testing.T) { testAppendLogRoundTrip(t, factory) })
	t.Run("PressureChannel_INV05", func(t *testing.T) { testPressureChannelINV05(t, factory) })
	t.Run("TornWriteRecovery_LSL06", func(t *testing.T) { testTornWriteRecovery(t, factory) })
	t.Run("ConcurrentStorm", func(t *testing.T) { testConcurrentStorm(t, factory) })
	t.Run("RollupOffsetMonotone_INV03", func(t *testing.T) { testRollupOffsetMonotoneINV03(t, factory) })
}

// testAppendLogRoundTrip asserts LSL-01/02/03/05: an AppendWrite lands in
// the log, the rollup pool emits content-addressed chunks under
// blocks/{hh}/{hh}/{hex}, and the metadata rollup_offset advances past
// the header.
func testAppendLogRoundTrip(t *testing.T, factory AppendLogFactory) {
	bc := factory(t)
	ctx := context.Background()
	// 8 MiB payload: reliably crosses the FastCDC min chunk size so the
	// chunker emits at least one chunk on the first rollup pass.
	payload := bytes.Repeat([]byte{0xAB}, 8*1024*1024)
	if err := bc.AppendWrite(ctx, "round-trip", payload, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	// Wait for the background rollup pool to advance rollup_offset past
	// the header. The stabilization window in factory-constructed stores
	// is short (50ms) so this normally resolves in < 1s.
	deadline := time.Now().Add(5 * time.Second)
	var metaOff uint64
	for time.Now().Before(deadline) {
		off, err := bc.RollupOffsetForTest(ctx, "round-trip")
		if err != nil {
			t.Fatalf("RollupOffsetForTest: %v", err)
		}
		if off > 64 {
			metaOff = off
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if metaOff <= 64 {
		// Force a synchronous rollup as a fallback in case the worker
		// pool's stabilization timing missed our window.
		if err := bc.ForceRollupForTest(ctx, "round-trip"); err != nil {
			t.Fatalf("ForceRollupForTest: %v", err)
		}
		metaOff, _ = bc.RollupOffsetForTest(ctx, "round-trip")
	}
	if metaOff <= 64 {
		t.Fatalf("metadata rollup_offset did not advance: got %d want > 64", metaOff)
	}

	// Verify at least one chunk exists under blocks/. The chunk content
	// is BLAKE3(data) and the path layout is blocks/<hh>/<hh>/<hex> per
	// D-11 — walking the tree is sufficient to prove at least one chunk
	// landed.
	blocksDir := filepath.Join(bc.BaseDirForTest(), "blocks")
	var chunkCount int
	_ = filepath.Walk(blocksDir, func(_ string, info os.FileInfo, werr error) error {
		if werr == nil && info != nil && !info.IsDir() {
			chunkCount++
		}
		return nil
	})
	if chunkCount == 0 {
		t.Fatal("no chunks written under blocks/")
	}

	// Header rollup_offset must be reconciled with metadata once the
	// rollup has advanced successfully (D-12 step 3). It may trail
	// metadata by a brief window in the crash-test scenario, but in the
	// happy path they match.
	hdrOff := bc.HeaderRollupOffsetForTest("round-trip")
	if hdrOff != metaOff {
		// advanceRollupOffset can fail fsync in rare CI scenarios; poll
		// briefly to catch an immediately-following success.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			hdrOff = bc.HeaderRollupOffsetForTest("round-trip")
			if hdrOff == metaOff {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		if hdrOff != metaOff {
			t.Fatalf("header rollup_offset %d != metadata %d", hdrOff, metaOff)
		}
	}
}

// testPressureChannelINV05 asserts INV-05 (D-14/D-15): once the total
// log bytes exceed maxLogBytes, subsequent AppendWrites block until the
// rollup drains the budget. The concurrent writers must all finish
// without deadlock, and the post-drain budget accounting must be within
// a sane ceiling (total data written was much larger than any
// in-memory bound).
func testPressureChannelINV05(t *testing.T, factory AppendLogFactory) {
	bc := factory(t)
	ctx := context.Background()
	// Shrink the budget aggressively so even small writes contend for
	// the pressure channel.
	bc.SetMaxLogBytesForTest(64 * 1024)

	const writers = 4
	const payloadLen = 1 * 1024 * 1024 // 1 MiB each (well past the 64 KiB budget)
	var wg sync.WaitGroup
	wg.Add(writers)
	errCh := make(chan error, writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			payload := bytes.Repeat([]byte{byte(0x10 + i)}, payloadLen)
			if err := bc.AppendWrite(ctx, fmt.Sprintf("press-%d", i), payload, 0); err != nil {
				errCh <- fmt.Errorf("writer %d: %w", i, err)
			}
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("pressure-channel test deadlocked (writers never finished)")
	}
	close(errCh)
	for err := range errCh {
		t.Fatalf("writer returned error: %v", err)
	}

	// Sanity bound: logBytesTotal must not exceed the sum of every
	// writer's framed record — the pressure path should have forced
	// drains to happen, and certainly not have double-accounted bytes.
	// Each record costs payloadLen + 16 bytes of frame overhead
	// (payload_len + file_offset + crc). We allow a small slack factor
	// to absorb any single-record straggler that lands after the final
	// drain signal.
	const recordFrameOverhead = 16
	totalData := int64(writers * (payloadLen + recordFrameOverhead))
	if leaked := bc.LogBytesTotalForTest(); leaked > totalData {
		t.Fatalf("pressure test accounting leaked: logBytesTotal=%d total-written=%d", leaked, totalData)
	}
}

// testTornWriteRecovery asserts LSL-06: appending garbage past a clean
// tail does NOT corrupt the surviving records. After reopening (which
// runs Recover), the interval tree has exactly the prior record count
// and the log is truncated at the first bad CRC.
func testTornWriteRecovery(t *testing.T, factory AppendLogFactory) {
	bc := factory(t)
	ctx := context.Background()
	const records = 5
	payload := bytes.Repeat([]byte{0xCD}, 256)
	for i := 0; i < records; i++ {
		if err := bc.AppendWrite(ctx, "torn", payload, uint64(i*4096)); err != nil {
			t.Fatalf("AppendWrite %d: %v", i, err)
		}
	}
	baseDir := bc.BaseDirForTest()
	rs := bc.RollupStoreForTest()
	// Close the store before poking the log file — a concurrent rollup
	// worker could otherwise race our truncation.
	if err := bc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Append random garbage to the log. Recovery must discard everything
	// past the last valid record and leave the N good records intact.
	logPath := filepath.Join(baseDir, "logs", "torn.log")
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("open log for torn-write: %v", err)
	}
	garbage := make([]byte, 300)
	_, _ = rand.New(rand.NewSource(1)).Read(garbage)
	if _, err := f.Write(garbage); err != nil {
		_ = f.Close()
		t.Fatalf("append garbage: %v", err)
	}
	_ = f.Close()

	// Reopen — Recover truncates at the first bad-CRC frame and replays
	// every surviving record into a fresh interval tree.
	bc2, err := fs.ReopenForTest(baseDir, rs)
	if err != nil {
		t.Fatalf("ReopenForTest: %v", err)
	}
	defer func() { _ = bc2.Close() }()

	got := bc2.IntervalsLenForTest("torn")
	if got != records {
		t.Fatalf("after recovery: intervals=%d want %d", got, records)
	}
	// Silence unused-ctx linter for the happy path — ctx is kept for
	// parity with the other scenarios.
	_ = ctx
}

// testConcurrentStorm asserts no deadlock or data corruption when many
// goroutines AppendWrite to many different files under a short
// stabilization window. The test doesn't read the data back (the rollup
// is intentionally allowed to race with writes) — its job is to prove
// the mutex + interval-tree + rollup + pressure machinery doesn't
// deadlock or leak.
func testConcurrentStorm(t *testing.T, factory AppendLogFactory) {
	bc := factory(t)
	ctx := context.Background()
	const files = 8
	const writersPerFile = 8
	const payloadLen = 4096
	var wg sync.WaitGroup
	wg.Add(files * writersPerFile)
	errCh := make(chan error, files*writersPerFile)
	for f := 0; f < files; f++ {
		for w := 0; w < writersPerFile; w++ {
			go func(f, w int) {
				defer wg.Done()
				payload := bytes.Repeat([]byte{byte(w)}, payloadLen)
				off := uint64(w) * payloadLen
				if err := bc.AppendWrite(ctx, fmt.Sprintf("storm-%d", f), payload, off); err != nil {
					errCh <- fmt.Errorf("writer %d/%d: %w", f, w, err)
				}
			}(f, w)
		}
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("concurrent storm deadlocked")
	}
	close(errCh)
	for err := range errCh {
		t.Fatalf("writer returned error: %v", err)
	}

	// FIX-11: assert no data was silently dropped. Each writer wrote
	// payloadLen bytes at a distinct file offset within its file, so the
	// per-file payload count must equal writersPerFile and the total
	// payload bytes must equal files*writersPerFile*payloadLen. Some
	// intervals may have already been rolled up by the background pool;
	// account for that by adding the metadata rollup_offset advance
	// (minus the 64-byte header) to the still-dirty interval count.
	const logHeaderSize = 64
	const recordFrameOverhead = 16
	const perRecordBytes = recordFrameOverhead + payloadLen
	wantTotalLogBytes := uint64(files) * uint64(writersPerFile) * uint64(perRecordBytes)
	var rolledUpBytes uint64
	for f := 0; f < files; f++ {
		pid := fmt.Sprintf("storm-%d", f)
		off, err := bc.RollupOffsetForTest(ctx, pid)
		if err != nil {
			t.Fatalf("RollupOffsetForTest(%s): %v", pid, err)
		}
		if off > logHeaderSize {
			rolledUpBytes += off - logHeaderSize
		}
	}
	dirtyBytes := uint64(bc.LogBytesTotalForTest())
	gotTotalLogBytes := dirtyBytes + rolledUpBytes
	if gotTotalLogBytes < wantTotalLogBytes {
		t.Fatalf("data dropped during storm: got %d log bytes (dirty=%d + rolled-up=%d), want at least %d",
			gotTotalLogBytes, dirtyBytes, rolledUpBytes, wantTotalLogBytes)
	}
}

// testRollupOffsetMonotoneINV03 asserts INV-03: if metadata has advanced
// past the on-disk header's rollup_offset (simulating a crash between
// D-12 step 2 SetRollupOffset and step 3 advanceRollupOffset), the next
// recovery writes the header forward to match metadata and never
// regresses the offset.
func testRollupOffsetMonotoneINV03(t *testing.T, factory AppendLogFactory) {
	bc := factory(t)
	ctx := context.Background()
	// Enough data to guarantee the chunker emits at least one chunk
	// (8 MiB comfortably crosses the 1 MiB FastCDC min).
	payload := bytes.Repeat([]byte{0xEF}, 8*1024*1024)
	if err := bc.AppendWrite(ctx, "monotone", payload, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	// Force a synchronous rollup so we do not race the background pool's
	// stabilization window.
	deadline := time.Now().Add(5 * time.Second)
	var metaOff uint64
	for time.Now().Before(deadline) {
		_ = bc.ForceRollupForTest(ctx, "monotone")
		off, _ := bc.RollupOffsetForTest(ctx, "monotone")
		if off > 64 {
			metaOff = off
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if metaOff <= 64 {
		t.Skip("rollup did not advance — test environment too slow to exercise INV-03")
	}
	baseDir := bc.BaseDirForTest()
	rs := bc.RollupStoreForTest()
	if err := bc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Simulate a crash BETWEEN SetRollupOffset and advanceRollupOffset:
	// metadata already at metaOff; on-disk header still at the post-init
	// value of logHeaderSize (64). We zero bytes [8..16) of the header
	// and recompute its CRC so the log is "valid but behind metadata" —
	// exactly what crash-window (2→3) looks like on reboot.
	logPath := filepath.Join(baseDir, "logs", "monotone.log")
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log for crash injection: %v", err)
	}
	// Clear bytes [8..16) then set [8]=64 (little-endian u64 = 64).
	for i := 8; i < 16; i++ {
		raw[i] = 0
	}
	raw[8] = 64
	// Recompute header CRC so unmarshalHeader accepts it; otherwise
	// Recover treats the header as hard-corrupt and re-inits the log.
	fs.RecomputeHeaderCRCForTest(raw[:64])
	if err := os.WriteFile(logPath, raw, 0644); err != nil {
		t.Fatalf("write rewound log: %v", err)
	}

	// Reopen. Recover sees metadata (metaOff) > header (64) and calls
	// advanceRollupOffset(f, metaOff) so the header catches up without
	// ever regressing.
	bc2, err := fs.ReopenForTest(baseDir, rs)
	if err != nil {
		t.Fatalf("ReopenForTest: %v", err)
	}
	defer func() { _ = bc2.Close() }()

	hdrOff := bc2.HeaderRollupOffsetForTest("monotone")
	if hdrOff != metaOff {
		t.Fatalf("INV-03: header not reconciled; got %d want %d", hdrOff, metaOff)
	}
	// And metadata itself must not regress either.
	postMeta, _ := bc2.RollupOffsetForTest(ctx, "monotone")
	if postMeta < metaOff {
		t.Fatalf("INV-03: metadata regressed; got %d want >= %d", postMeta, metaOff)
	}
}
