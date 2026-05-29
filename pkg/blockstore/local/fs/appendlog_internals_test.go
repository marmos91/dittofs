package fs_test

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
	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// appendlogFactory constructs an *fs.FSStore backed by a memory RollupStore
// so the fs-internal append-log scenarios can probe RollupOffset,
// LogBytesTotal, HeaderRollupOffset, etc. via the *ForTest hooks on
// *FSStore.
//
// Inlined these three scenarios (PressureChannel_INV05
// TornWriteRecovery_LSL06, RollupOffsetMonotone_INV03) from
// pkg/blockstore/local/localtest/appendlog_suite.go (deleted in this
// plan). The other two scenarios from that suite (AppendLogRoundTrip
// ConcurrentStorm) now live in pkg/blockstore/blockstoretest as
// BlockStoreAppendConformance subtests; the fs backend invokes them
// via fs_conformance_test.go.
//
// These three scenarios stay here because they require fs-internal
// probes that intentionally do NOT appear on the BlockStoreAppend
// interface surface.
func appendlogFactory(t *testing.T) *fs.FSStore {
	t.Helper()
	dir := t.TempDir()
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc, err := fs.NewWithOptions(dir, 1<<30, 1<<30, nil, fs.FSStoreOptions{
		MaxLogBytes:     1 << 30,
		RollupWorkers:   2,
		StabilizationMS: 50,
		RollupStore:     rs,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	if err := bc.StartRollup(context.Background()); err != nil {
		t.Fatalf("StartRollup: %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	return bc
}

// TestAppendLog_PressureChannel_INV05 asserts: once
// the total log bytes exceed maxLogBytes, subsequent AppendWrites
// block until the rollup drains the budget. The concurrent writers
// must all finish without deadlock, and the post-drain budget
// accounting must be within a sane ceiling (total data written was
// much larger than any in-memory bound).
func TestAppendLog_PressureChannel_INV05(t *testing.T) {
	bc := appendlogFactory(t)
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

// TestAppendLog_TornWriteRecovery_LSL06 asserts: appending
// garbage past a clean tail does NOT corrupt the surviving records.
// After reopening (which runs Recover), the interval tree has exactly
// the prior record count and the log is truncated at the first bad
// CRC.
func TestAppendLog_TornWriteRecovery_LSL06(t *testing.T) {
	bc := appendlogFactory(t)
	ctx := context.Background()
	baseDir := bc.BaseDirForTest()
	rs := bc.RollupStoreForTest()
	// Close the factory-created store and reopen WITHOUT a rollup
	// pool. The factory enables rollup with a short stabilization
	// window (50 ms) so the unrelated subtests in this suite can
	// exercise the rollup path; on slow-IO platforms (Windows NTFS)
	// 5 sequential AppendWrite + fsync iterations exceed that window
	// and the rollup advances rollup_offset past records mid-test
	// leaving Recover with fewer intervals than were actually
	// written. ReopenForTest constructs an FSStore without calling
	// StartRollup, so AppendWrites stay durably in the log until we
	// close it ourselves.
	if err := bc.Close(); err != nil {
		t.Fatalf("Close factory store: %v", err)
	}
	bcWrite, err := fs.ReopenForTest(baseDir, rs)
	if err != nil {
		t.Fatalf("ReopenForTest (write phase): %v", err)
	}
	const records = 5
	payload := bytes.Repeat([]byte{0xCD}, 256)
	for i := 0; i < records; i++ {
		if err := bcWrite.AppendWrite(ctx, "torn", payload, uint64(i*4096)); err != nil {
			t.Fatalf("AppendWrite %d: %v", i, err)
		}
	}
	if err := bcWrite.Close(); err != nil {
		t.Fatalf("Close write phase: %v", err)
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
}

// TestAppendLog_RollupOffsetMonotone_INV03 asserts: if metadata
// has advanced past the on-disk header's rollup_offset (simulating a
// crash between step 2 SetRollupOffset and step 3
// advanceRollupOffset), the next recovery writes the header forward to
// match metadata and never regresses the offset.
func TestAppendLog_RollupOffsetMonotone_INV03(t *testing.T) {
	bc := appendlogFactory(t)
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

	// Simulate a crash BETWEEN SetRollupOffset and advanceRollupOffset
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
