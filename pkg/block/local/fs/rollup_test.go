package fs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
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
// short stabilization window, let the rollup pool fire, and assert
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
// value unchanged.
//
// We deliberately do NOT call StartRollup so the test has exclusive control
// of rollupFile invocation. Use a tiny stabilization so the single record
// becomes stable within a few ms.
func TestRollup_CommitChunks_MonotoneEnforced(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreForTest(t, FSStoreOptions{
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

	if err := bc.rollupFile(ctx, "file1", false); err != nil {
		t.Fatalf("rollupFile on regression: want nil (benign), got %v", err)
	}

	got, _ := rs.GetRollupOffset(ctx, "file1")
	if got != preSeed {
		t.Fatalf("stored offset regressed despite INV-03 rejection: got %d want %d", got, preSeed)
	}
}

// TestRollup_StartRollup_NilStore: StartRollup with a nil RollupStore must
// return a descriptive error so misconfiguration fails loudly.
func TestRollup_StartRollup_NilStore(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{
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

// TestRollup_ReconstructStream_OverwriteLaterWins: unit test for order
// semantics — later records overwrite earlier at the same offset.
//
// The buffer is anchored at baseOff (the smallest record offset): buf[0]
// holds file byte baseOff, so the dead [0,baseOff) prefix is not allocated.
func TestRollup_ReconstructStream_OverwriteLaterWins(t *testing.T) {
	recs := []rec{
		{off: 100, payload: []byte("AAAA")},
		{off: 100, payload: []byte("BBBB")}, // same offset, later wins
		{off: 104, payload: []byte("CCCC")},
	}
	got, baseOff, err := reconstructStream(recs)
	if err != nil {
		t.Fatalf("reconstructStream: %v", err)
	}
	if baseOff != 100 {
		t.Fatalf("reconstructStream baseOff: got %d want 100", baseOff)
	}
	// span = maxEnd(108) - baseOff(100) = 8; no zero-padded prefix.
	if len(got) != 8 {
		t.Fatalf("reconstructStream length: got %d want 8 (span, baseOff-anchored)", len(got))
	}
	want := []byte("BBBBCCCC")
	if !bytes.Equal(got, want) {
		t.Fatalf("reconstructStream payload: got %q want %q", got, want)
	}
}

// TestRollup_ReconstructStream_SparseGapZeroed: records with a gap inside
// the window leave the gap zero-filled, and the buffer starts at the lowest
// record offset (no dead prefix below it).
func TestRollup_ReconstructStream_SparseGapZeroed(t *testing.T) {
	recs := []rec{
		{off: 200, payload: []byte("AB")},
		{off: 210, payload: []byte("CD")}, // 8-byte gap [202,210)
	}
	got, baseOff, err := reconstructStream(recs)
	if err != nil {
		t.Fatalf("reconstructStream: %v", err)
	}
	if baseOff != 200 {
		t.Fatalf("baseOff: got %d want 200", baseOff)
	}
	want := []byte("AB\x00\x00\x00\x00\x00\x00\x00\x00CD") // span = 212-200 = 12
	if !bytes.Equal(got, want) {
		t.Fatalf("reconstructStream sparse: got %q want %q", got, want)
	}
}

// TestRollup_ReconstructStream_ContentMapping asserts the file-offset →
// content invariant dedup relies on: the same payload bytes land at the
// same buffer-relative position (absOff - baseOff) regardless of what else
// is dirty in the pass. FastCDC is content-defined, so the chunker — fed
// stream[i:] — emits identical boundaries for identical suffix bytes; the
// absolute backing-array anchor is irrelevant.
func TestRollup_ReconstructStream_ContentMapping(t *testing.T) {
	high := []byte("PAYLOAD-AT-OFFSET-1024")
	passA := []rec{{off: 1024, payload: high}}
	passB := []rec{
		{off: 16, payload: []byte("LOW")},
		{off: 1024, payload: high},
	}
	bufA, baseA, errA := reconstructStream(passA)
	if errA != nil {
		t.Fatalf("reconstructStream passA: %v", errA)
	}
	bufB, baseB, errB := reconstructStream(passB)
	if errB != nil {
		t.Fatalf("reconstructStream passB: %v", errB)
	}
	if got := bufA[1024-baseA : 1024-baseA+uint64(len(high))]; !bytes.Equal(got, high) {
		t.Fatalf("passA high region: got %q want %q", got, high)
	}
	if got := bufB[1024-baseB : 1024-baseB+uint64(len(high))]; !bytes.Equal(got, high) {
		t.Fatalf("passB high region: got %q want %q", got, high)
	}
}

// TestRollup_ReconstructStream_Empty: empty input returns nil.
func TestRollup_ReconstructStream_Empty(t *testing.T) {
	got, _, err := reconstructStream(nil)
	if err != nil {
		t.Fatalf("reconstructStream nil err: %v", err)
	}
	if got != nil {
		t.Fatalf("reconstructStream nil: got %v want nil", got)
	}
	got, _, err = reconstructStream([]rec{})
	if err != nil {
		t.Fatalf("reconstructStream empty err: %v", err)
	}
	if got != nil {
		t.Fatalf("reconstructStream empty: got %v want nil", got)
	}
}

// TestRollup_ReconstructStream_DoSCeiling_FIX5 asserts reconstructStream
// refuses to allocate when the SPAN (maxEnd-baseOff) exceeds the ceiling.
// Lower the ceiling so two close records force refusal without allocating.
func TestRollup_ReconstructStream_DoSCeiling_FIX5(t *testing.T) {
	defer func(old uint64) { maxReconstructBytes = old }(maxReconstructBytes)
	maxReconstructBytes = 16
	recs := []rec{
		{off: 0, payload: []byte("x")},
		{off: 64, payload: []byte("x")}, // span 65 > 16-byte test ceiling
	}
	if _, _, err := reconstructStream(recs); err == nil {
		t.Fatal("reconstructStream above ceiling: want error, got nil")
	}
}

// TestRollup_TruncateMidWindow_DoesNotAdvancePastUncommittedTail is the
// regression guard for FIX-19. Repro
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
		MaxLogBytes:     1 << 30,
		RollupWorkers:   2,
		StabilizationMS: 1,
		RollupStore:     rs,
	})
	ctx := context.Background()

	// Three small records at distinct offsets. Use len 50 so
	// - rec at off=0 covers [0,50) — survives truncation
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

	if err := bc.rollupFile(ctx, "tfile", false); err != nil {
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

// capturedPersist is a minimal struct used by TestRollup_CommitChunks_
// PersistsObjectID subtests to record arguments passed to an
// ObjectIDPersister closure.
type capturedPersist struct {
	payloadID string
	blocks    []block.BlockRef
	objectID  block.ObjectID
}

// runRollupOnce triggers exactly one rollupFile pass for payloadID after
// AppendWrite-ing the supplied payload and giving the dirty interval time
// to stabilize. Mirrors the test pattern used by TestRollup_CommitChunks_
// MonotoneEnforced. NOT safe to call from a goroutine other than the
// test's own — uses t.Fatal*. Use runRollupOnceErr for concurrent tests.
func runRollupOnce(t *testing.T, bc *FSStore, payloadID string, payload []byte) {
	t.Helper()
	if err := runRollupOnceErr(bc, payloadID, payload); err != nil {
		t.Fatal(err)
	}
}

// runRollupOnceErr is the goroutine-safe variant: it returns errors
// instead of calling t.Fatal*. Concurrent tests must use this and
// fan errors back to the test goroutine via a channel; t.FailNow
// (which t.Fatal* call) is undefined when invoked from a non-test
// goroutine.
func runRollupOnceErr(bc *FSStore, payloadID string, payload []byte) error {
	ctx := context.Background()
	if err := bc.AppendWrite(ctx, payloadID, payload, 0); err != nil {
		return fmt.Errorf("AppendWrite: %w", err)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if bc.EarliestStableForTest(payloadID) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !bc.EarliestStableForTest(payloadID) {
		return fmt.Errorf("dirty interval did not stabilize within 500 ms")
	}
	if err := bc.rollupFile(ctx, payloadID, false); err != nil {
		return fmt.Errorf("rollupFile: %w", err)
	}
	return nil
}

// TestRollup_CommitChunks_PersistsObjectID covers the rollup-time
// ObjectID compute path
//
//  1. PersistsObjectIDOnCommit — happy path: persister invoked exactly
//
// once with a non-empty BlockRef manifest, BlockRefs sorted-by-Offset
//
//	   and an ObjectID equal to ComputeObjectID(blocks).
//	2. NilPersisterIsBenign — rollup proceeds without panic or error when
//	   FSStoreOptions.ObjectIDPersister is left nil (local-only fixture).
//	3. PersisterErrorPropagates — persister error surfaces wrapped through
//	   errors.Is so callers can distinguish it from neighbour errors.
func TestRollup_CommitChunks_PersistsObjectID(t *testing.T) {
	t.Run("PersistsObjectIDOnCommit", func(t *testing.T) {
		rs := memmeta.NewMemoryMetadataStoreWithDefaults()

		var mu sync.Mutex
		var captures []capturedPersist
		persister := func(_ context.Context, pid string, blocks []block.BlockRef, oid block.ObjectID) error {
			mu.Lock()
			defer mu.Unlock()
			// Defensive copy so subsequent rollup passes (none expected
			// in this subtest) cannot mutate the captured slice.
			cp := make([]block.BlockRef, len(blocks))
			copy(cp, blocks)
			captures = append(captures, capturedPersist{
				payloadID: pid,
				blocks:    cp,
				objectID:  oid,
			})
			return nil
		}

		bc := newFSStoreForTest(t, FSStoreOptions{
			MaxLogBytes:       1 << 30,
			RollupWorkers:     2,
			StabilizationMS:   1,
			RollupStore:       rs,
			ObjectIDPersister: persister,
		})

		// 8 MiB payload is large enough that the chunker emits at least one
		// chunk; FastCDC may emit several. Either way the BlockRef
		// manifest is non-empty.
		payload := bytes.Repeat([]byte{0xCD}, 8*1024*1024)
		runRollupOnce(t, bc, "happyfile", payload)

		mu.Lock()
		defer mu.Unlock()

		if got := len(captures); got != 1 {
			t.Fatalf("persister invocation count: got %d want 1", got)
		}
		cap := captures[0]
		if cap.payloadID != "happyfile" {
			t.Fatalf("captured payloadID: got %q want %q", cap.payloadID, "happyfile")
		}
		if len(cap.blocks) == 0 {
			t.Fatal("captured BlockRefs: got empty slice; chunker should have emitted ≥1 chunk")
		}
		if !sort.SliceIsSorted(cap.blocks, func(i, j int) bool {
			return cap.blocks[i].Offset < cap.blocks[j].Offset
		}) {
			t.Fatalf("captured BlockRefs not sorted by Offset: %+v", cap.blocks)
		}
		expectedOID := block.ComputeObjectID(cap.blocks)
		if cap.objectID != expectedOID {
			t.Fatalf("captured ObjectID mismatch: got %s want %s",
				cap.objectID.String(), expectedOID.String())
		}
		// Sanity: local-only path materialized a non-zero ObjectID without
		// any remote upload.
		var zero block.ObjectID
		if cap.objectID == zero {
			t.Fatal("captured ObjectID is all-zero; local-only rollup should produce a real identity")
		}
	})

	t.Run("NilPersisterIsBenign", func(t *testing.T) {
		rs := memmeta.NewMemoryMetadataStoreWithDefaults()
		bc := newFSStoreForTest(t, FSStoreOptions{
			MaxLogBytes:     1 << 30,
			RollupWorkers:   2,
			StabilizationMS: 1,
			RollupStore:     rs,
			// ObjectIDPersister left nil — local-only fixture.
		})

		payload := bytes.Repeat([]byte{0xEF}, 8*1024*1024)
		runRollupOnce(t, bc, "nilfile", payload)

		// rollup_offset should still have advanced — the persister being
		// nil must not block the rollup from quiescing.
		off, err := rs.GetRollupOffset(context.Background(), "nilfile")
		if err != nil {
			t.Fatalf("GetRollupOffset: %v", err)
		}
		if off <= uint64(logHeaderSize) {
			t.Fatalf("rollup_offset did not advance past header: got %d", off)
		}
	})

	t.Run("PersisterErrorPropagates", func(t *testing.T) {
		rs := memmeta.NewMemoryMetadataStoreWithDefaults()

		simulated := errors.New("simulated persister failure")
		persister := func(_ context.Context, _ string, _ []block.BlockRef, _ block.ObjectID) error {
			return simulated
		}

		bc := newFSStoreForTest(t, FSStoreOptions{
			MaxLogBytes:       1 << 30,
			RollupWorkers:     2,
			StabilizationMS:   1,
			RollupStore:       rs,
			ObjectIDPersister: persister,
		})

		ctx := context.Background()
		payload := bytes.Repeat([]byte{0x99}, 1*1024*1024)
		if err := bc.AppendWrite(ctx, "errfile", payload, 0); err != nil {
			t.Fatalf("AppendWrite: %v", err)
		}
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			if bc.EarliestStableForTest("errfile") {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if !bc.EarliestStableForTest("errfile") {
			t.Fatal("dirty interval did not stabilize within 500 ms")
		}

		err := bc.rollupFile(ctx, "errfile", false)
		if err == nil {
			t.Fatal("rollupFile: want persister error, got nil")
		}
		if !errors.Is(err, simulated) {
			t.Fatalf("rollupFile error: want errors.Is(simulated)=true, got %v", err)
		}
	})
}

// TestRollup_ComputeObjectID_StableAcrossPayloads rolls up two distinct
// payloads carrying identical content and asserts the rollup-time
// ObjectID + BlockRef manifest are identical. CAS StoreChunk is
// content-addressed and idempotent, so the second payload re-stores the
// same chunk harmlessly; the manifest must not drift.
func TestRollup_ComputeObjectID_StableAcrossPayloads(t *testing.T) {
	mem := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreForTest(t, FSStoreOptions{
		MaxLogBytes:     1 << 30,
		RollupWorkers:   2,
		StabilizationMS: 1,
		RollupStore:     mem,
	})

	payload := bytes.Repeat([]byte{0xF1}, 256*1024)

	var mu sync.Mutex
	var captured []capturedPersist
	bc.SetObjectIDPersister(func(_ context.Context, pid string, blocks []block.BlockRef, oid block.ObjectID) error {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]block.BlockRef, len(blocks))
		copy(cp, blocks)
		captured = append(captured, capturedPersist{payloadID: pid, blocks: cp, objectID: oid})
		return nil
	})

	runRollupOnce(t, bc, "oid-A", payload)
	runRollupOnce(t, bc, "oid-B", payload)

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("persister captures: got %d want 2", len(captured))
	}
	if captured[0].objectID != captured[1].objectID {
		t.Fatalf("ObjectID drift across payloads: %s vs %s",
			captured[0].objectID.String(), captured[1].objectID.String())
	}
	if len(captured[0].blocks) != len(captured[1].blocks) {
		t.Fatalf("BlockRef manifest length drift: A=%d B=%d", len(captured[0].blocks), len(captured[1].blocks))
	}
	for i := range captured[0].blocks {
		a, b := captured[0].blocks[i], captured[1].blocks[i]
		if a.Hash != b.Hash || a.Offset != b.Offset || a.Size != b.Size {
			t.Fatalf("BlockRef[%d] drift between payloads: A=%+v B=%+v", i, a, b)
		}
	}
}
