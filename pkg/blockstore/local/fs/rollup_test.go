package fs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newRollupFSStore builds an FSStore with append-log + rollup enabled and a
// memory-backed RollupStore, launches the rollup pool, and registers Close
// for cleanup.
func newRollupFSStore(t *testing.T, maxLogBytes int64, stabilizationMS int) (*FSStore, metadata.RollupStore) {
	t.Helper()
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	opts := FSStoreOptions{
		UseAppendLog:    true,
		MaxLogBytes:     maxLogBytes,
		RollupWorkers:   2,
		StabilizationMS: stabilizationMS,
		RollupStore:     rs,
	}
	bc := newFSStoreForTest(t, opts)
	if err := bc.StartRollup(context.Background()); err != nil {
		t.Fatalf("StartRollup: %v", err)
	}
	return bc, rs
}

// waitForRollup polls until rollup_offset for payloadID advances past
// logHeaderSize (i.e., at least one record has been rolled up), or fails the
// test after timeout.
func waitForRollup(t *testing.T, rs metadata.RollupStore, payloadID string, timeout time.Duration) uint64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		off, err := rs.GetRollupOffset(context.Background(), payloadID)
		if err != nil {
			t.Fatalf("GetRollupOffset: %v", err)
		}
		if off > uint64(logHeaderSize) {
			return off
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("rollup_offset did not advance for %s within %v", payloadID, timeout)
	return 0
}

// countChunksInBlocks walks baseDir/blocks/ and counts non-directory files.
func countChunksInBlocks(t *testing.T, baseDir string) int {
	t.Helper()
	blocksDir := filepath.Join(baseDir, "blocks")
	count := 0
	_ = filepath.WalkDir(blocksDir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			count++
		}
		return nil
	})
	return count
}

// TestRollup_CommitChunks_HappyPath: AppendWrite a large payload under a
// short stabilization window, let the rollup pool fire, and assert:
//   - at least one chunk landed in blocks/
//   - rollup_offset in metadata advanced past logHeaderSize
//   - logBytesTotal decreased (budget released)
func TestRollup_CommitChunks_HappyPath(t *testing.T) {
	bc, rs := newRollupFSStore(t, 1<<30, 10)
	ctx := context.Background()

	payload := bytes.Repeat([]byte{0xAB}, 8*1024*1024)
	beforeBytes := bc.logBytesTotal.Load()
	if err := bc.AppendWrite(ctx, "file1", payload, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	postWriteBytes := bc.logBytesTotal.Load()
	if postWriteBytes <= beforeBytes {
		t.Fatalf("logBytesTotal did not grow after AppendWrite: before=%d after=%d", beforeBytes, postWriteBytes)
	}

	off := waitForRollup(t, rs, "file1", 5*time.Second)
	if off <= uint64(logHeaderSize) {
		t.Fatalf("rollup_offset did not advance past header: got %d", off)
	}

	// Wait a bit more for logBytesTotal to decrement (it's set after
	// SetRollupOffset so may trail the GetRollupOffset check slightly).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bc.logBytesTotal.Load() < postWriteBytes {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if bc.logBytesTotal.Load() >= postWriteBytes {
		t.Fatalf("logBytesTotal did not drop after rollup: still %d", bc.logBytesTotal.Load())
	}

	if n := countChunksInBlocks(t, bc.baseDir); n < 1 {
		t.Fatalf("expected ≥1 chunk in blocks/, found %d", n)
	}
}

// TestRollup_PressureReleasesAppendWrite: tiny log budget forces the
// AppendWrite to block on pressureCh. Rollup worker must drain + signal so
// the writer unblocks within a deadline.
func TestRollup_PressureReleasesAppendWrite(t *testing.T) {
	// 64 KiB budget — a single 2 MiB AppendWrite will exceed it, forcing
	// the pressure loop on subsequent writes.
	bc, _ := newRollupFSStore(t, 64*1024, 10)
	ctx := context.Background()

	// First write — consumes budget.
	first := bytes.Repeat([]byte{0x11}, 2*1024*1024)
	if err := bc.AppendWrite(ctx, "file1", first, 0); err != nil {
		t.Fatalf("first AppendWrite: %v", err)
	}

	// Second write — must wait for rollup to drain. Run in goroutine with a
	// deadline so we can assert it unblocks.
	second := bytes.Repeat([]byte{0x22}, 1*1024*1024)
	done := make(chan error, 1)
	go func() {
		done <- bc.AppendWrite(ctx, "file1", second, 2*1024*1024)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second AppendWrite returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AppendWrite never unblocked (rollup not draining pressure)")
	}
}

// TestRollup_CommitChunks_MonotoneEnforced: if rollup_offset is pre-seeded
// to a value ABOVE what rollupFile would advance to, the call must treat
// ErrRollupOffsetRegression as benign — returning nil AND leaving the stored
// value unchanged (INV-03).
//
// We deliberately do NOT call StartRollup so the test has exclusive control
// of rollupFile invocation. Use a tiny stabilization so the single record
// becomes stable within a few ms.
func TestRollup_CommitChunks_MonotoneEnforced(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreForTest(t, FSStoreOptions{
		UseAppendLog:    true,
		MaxLogBytes:     1 << 30,
		RollupWorkers:   2,
		StabilizationMS: 1, // 1 ms — record stabilizes almost immediately
		RollupStore:     rs,
	})
	ctx := context.Background()

	// Pre-seed rollup_offset well above the first record's position.
	const preSeed = uint64(1 << 30)
	if _, err := rs.SetRollupOffset(ctx, "file1", preSeed); err != nil {
		t.Fatalf("pre-seed SetRollupOffset: %v", err)
	}

	// Write a small record so rollupFile has dirty data to process.
	payload := []byte("small-record-to-rollup")
	if err := bc.AppendWrite(ctx, "file1", payload, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	// FIX-10: poll EarliestStableForTest instead of sleeping. The previous
	// fixed time.Sleep(20ms) was flaky under heavy CI load — if the
	// scheduler delayed the AppendWrite goroutine past the sleep, the
	// rollupFile call would skip the regression-rejection branch and the
	// test would pass for the wrong reason. Polling actively waits until
	// the rollup engine WOULD observe a stable interval, then asserts.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if bc.EarliestStableForTest("file1") {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !bc.EarliestStableForTest("file1") {
		t.Fatal("dirty interval did not stabilize within 500 ms — test cannot exercise regression-rejection branch")
	}

	if err := bc.rollupFile(ctx, "file1"); err != nil {
		t.Fatalf("rollupFile on regression: want nil (benign), got %v", err)
	}

	got, _ := rs.GetRollupOffset(ctx, "file1")
	if got != preSeed {
		t.Fatalf("stored offset regressed despite INV-03 rejection: got %d want %d", got, preSeed)
	}
}

// TestRollup_StartRollup_DisabledFlag: StartRollup with use_append_log=false
// must return ErrAppendLogDisabled without launching workers.
func TestRollup_StartRollup_DisabledFlag(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{
		UseAppendLog: false,
		RollupStore:  memmeta.NewMemoryMetadataStoreWithDefaults(),
	})
	if err := bc.StartRollup(context.Background()); !errors.Is(err, ErrAppendLogDisabled) {
		t.Fatalf("StartRollup with flag off: got %v want ErrAppendLogDisabled", err)
	}
}

// TestRollup_StartRollup_NilStore: StartRollup with a nil RollupStore must
// return a descriptive error so misconfiguration fails loudly.
func TestRollup_StartRollup_NilStore(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{
		UseAppendLog: true,
		// RollupStore intentionally nil.
	})
	if err := bc.StartRollup(context.Background()); err == nil {
		t.Fatalf("StartRollup with nil RollupStore: want error, got nil")
	}
}

// TestRollup_StartRollup_Idempotent: calling StartRollup twice must not
// spawn a second pool (observable via rollupStarted CAS — second call
// returns nil without adding to rollupWg).
func TestRollup_StartRollup_Idempotent(t *testing.T) {
	bc, _ := newRollupFSStore(t, 1<<30, 250)
	// StartRollup already called by the helper; call it again.
	if err := bc.StartRollup(context.Background()); err != nil {
		t.Fatalf("second StartRollup: got %v want nil", err)
	}
}

// TestRollup_ReconstructStream_OverwriteLaterWins: unit test for D-35 order
// semantics — later records overwrite earlier at the same offset.
//
// FIX-3: the buffer is indexed by FILE OFFSET — bytes [0..minOff) are
// zero-padded so the chunker sees identical prefixes across rollup passes
// and chunk boundaries stay stable.
func TestRollup_ReconstructStream_OverwriteLaterWins(t *testing.T) {
	recs := []rec{
		{off: 100, payload: []byte("AAAA")},
		{off: 100, payload: []byte("BBBB")}, // same offset, later wins
		{off: 104, payload: []byte("CCCC")},
	}
	got, err := reconstructStream(recs)
	if err != nil {
		t.Fatalf("reconstructStream: %v", err)
	}
	if len(got) != 108 {
		t.Fatalf("reconstructStream length: got %d want 108 (file-offset-indexed)", len(got))
	}
	// First 100 bytes are the zero-padded gap below minOff.
	for i := 0; i < 100; i++ {
		if got[i] != 0 {
			t.Fatalf("reconstructStream[%d]: got %d want 0 (gap below minOff)", i, got[i])
		}
	}
	want := []byte("BBBBCCCC")
	if !bytes.Equal(got[100:], want) {
		t.Fatalf("reconstructStream payload: got %q want %q", got[100:], want)
	}
}

// TestRollup_ReconstructStream_BoundaryStability_FIX3 asserts the FIX-3
// invariant: two passes that both touch the same file region (same payload
// bytes at the same file offsets) reconstruct identical buffers regardless
// of whether other dirty regions sit at lower offsets in either pass. This
// is what guarantees the chunker emits the same boundaries → dedup holds
// across rollup passes (D-21).
func TestRollup_ReconstructStream_BoundaryStability_FIX3(t *testing.T) {
	// Pass A: only the high-offset region is dirty.
	passA := []rec{
		{off: 1024, payload: []byte("PAYLOAD-AT-OFFSET-1024")},
	}
	// Pass B: the high-offset region is dirty AND a small low-offset
	// region (which would shift minOff under the old behavior).
	passB := []rec{
		{off: 16, payload: []byte("LOW")},
		{off: 1024, payload: []byte("PAYLOAD-AT-OFFSET-1024")},
	}
	bufA, errA := reconstructStream(passA)
	if errA != nil {
		t.Fatalf("reconstructStream passA: %v", errA)
	}
	bufB, errB := reconstructStream(passB)
	if errB != nil {
		t.Fatalf("reconstructStream passB: %v", errB)
	}

	// The byte at offset 1024 in BOTH buffers must be the first byte of
	// the high-offset payload — i.e., the file-offset → buffer-index
	// mapping is invariant under changes to minOff.
	wantHigh := []byte("PAYLOAD-AT-OFFSET-1024")
	if got := bufA[1024 : 1024+len(wantHigh)]; !bytes.Equal(got, wantHigh) {
		t.Fatalf("passA[1024..]: got %q want %q", got, wantHigh)
	}
	if got := bufB[1024 : 1024+len(wantHigh)]; !bytes.Equal(got, wantHigh) {
		t.Fatalf("passB[1024..]: got %q want %q", got, wantHigh)
	}
	// Both buffers' [1024..end) range must be byte-identical so the
	// chunker (gear-hash position-keyed) emits the same boundaries.
	if !bytes.Equal(bufA[1024:], bufB[1024:]) {
		t.Fatal("FIX-3 violated: high-offset region differs between passes; chunker boundaries would drift")
	}
}

// TestRollup_ReconstructStream_Empty: empty input returns nil.
func TestRollup_ReconstructStream_Empty(t *testing.T) {
	got, err := reconstructStream(nil)
	if err != nil {
		t.Fatalf("reconstructStream nil err: %v", err)
	}
	if got != nil {
		t.Fatalf("reconstructStream nil: got %v want nil", got)
	}
	got, err = reconstructStream([]rec{})
	if err != nil {
		t.Fatalf("reconstructStream empty err: %v", err)
	}
	if got != nil {
		t.Fatalf("reconstructStream empty: got %v want nil", got)
	}
}

// TestRollup_ReconstructStream_DoSCeiling_FIX5 asserts reconstructStream
// refuses to allocate when maxEnd exceeds the 16 GiB ceiling. We forge a
// record with a far-future offset (no payload bytes are actually
// allocated by the test itself).
func TestRollup_ReconstructStream_DoSCeiling_FIX5(t *testing.T) {
	recs := []rec{
		{off: maxReconstructBytes + 1, payload: []byte("x")},
	}
	if _, err := reconstructStream(recs); err == nil {
		t.Fatal("reconstructStream above ceiling: want error, got nil")
	}
}

// TestRollup_TruncateMidWindow_DoesNotAdvancePastUncommittedTail is the
// regression guard for FIX-19. Repro:
//
//  1. AppendWrite three small records at offsets 0, 100, 200.
//  2. TruncateAppendLog(150) — the record at offset 200 is past the
//     truncation boundary and must NOT contribute to chunks.
//  3. Run rollupFile.
//
// Before FIX-19, targetPos was computed during the record-scan loop and
// already accounted for the offset-200 record's frame bytes by the time
// the truncation filter dropped it; SetRollupOffset would persist a
// position past the dropped record and the bytes between the last
// committed frame and the persisted offset would silently disappear on
// the next boot's recovery scan.
//
// Assertion: after rollup, the persisted rollup_offset is equal to the
// on-disk position immediately after the LAST RECORD that survived the
// truncation filter (offset 100's frame end), not the on-disk position
// past the dropped record.
func TestRollup_TruncateMidWindow_DoesNotAdvancePastUncommittedTail(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreForTest(t, FSStoreOptions{
		UseAppendLog:    true,
		MaxLogBytes:     1 << 30,
		RollupWorkers:   2,
		StabilizationMS: 1,
		RollupStore:     rs,
	})
	ctx := context.Background()

	// Three small records at distinct offsets. Use len 50 so:
	//   - rec at off=0   covers [0,50)   — survives truncation
	//   - rec at off=100 covers [100,150) — survives truncation (end == trunc)
	//   - rec at off=200 covers [200,250) — DROPPED by truncation
	payload := bytes.Repeat([]byte{0xAA}, 50)
	for _, off := range []uint64{0, 100, 200} {
		if err := bc.AppendWrite(ctx, "tfile", payload, off); err != nil {
			t.Fatalf("AppendWrite off=%d: %v", off, err)
		}
	}

	// Truncate at 150. The record at off=200 must be dropped.
	if err := bc.TruncateAppendLog(ctx, "tfile", 150); err != nil {
		t.Fatalf("TruncateAppendLog: %v", err)
	}

	// Wait for stabilization (1 ms).
	time.Sleep(20 * time.Millisecond)

	if err := bc.rollupFile(ctx, "tfile"); err != nil {
		t.Fatalf("rollupFile: %v", err)
	}

	// Pre-FIX-19 behavior: targetPos walks past all three records, so
	// rollup_offset == header + 3*(frame+50). FIX-19 caps it at the
	// last surviving record's endPos == header + 2*(frame+50).
	got, err := rs.GetRollupOffset(ctx, "tfile")
	if err != nil {
		t.Fatalf("GetRollupOffset: %v", err)
	}
	wantMax := uint64(logHeaderSize) + 2*uint64(recordFrameOverhead+len(payload))
	if got > wantMax {
		t.Fatalf("FIX-19 regression: rollup_offset advanced past the truncated record (got %d, max-allowed %d) — bytes between [%d,%d) would be lost on next boot",
			got, wantMax, wantMax, got)
	}
}
