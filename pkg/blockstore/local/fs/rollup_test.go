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
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
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

	if err := bc.rollupFile(ctx, "file1"); err != nil {
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
// across rollup passes.
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

// capturedPersist is a minimal struct used by TestRollup_CommitChunks_
// PersistsObjectID subtests to record arguments passed to an
// ObjectIDPersister closure.
type capturedPersist struct {
	payloadID string
	blocks    []blockstore.BlockRef
	objectID  blockstore.ObjectID
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
	if err := bc.rollupFile(ctx, payloadID); err != nil {
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
		persister := func(_ context.Context, pid string, blocks []blockstore.BlockRef, oid blockstore.ObjectID) error {
			mu.Lock()
			defer mu.Unlock()
			// Defensive copy so subsequent rollup passes (none expected
			// in this subtest) cannot mutate the captured slice.
			cp := make([]blockstore.BlockRef, len(blocks))
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
		expectedOID := blockstore.ComputeObjectID(cap.blocks)
		if cap.objectID != expectedOID {
			t.Fatalf("captured ObjectID mismatch: got %s want %s",
				cap.objectID.String(), expectedOID.String())
		}
		// Sanity: local-only path materialized a non-zero ObjectID without
		// any remote upload.
		var zero blockstore.ObjectID
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
		persister := func(_ context.Context, _ string, _ []blockstore.BlockRef, _ blockstore.ObjectID) error {
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

		err := bc.rollupFile(ctx, "errfile")
		if err == nil {
			t.Fatal("rollupFile: want persister error, got nil")
		}
		if !errors.Is(err, simulated) {
			t.Fatalf("rollupFile error: want errors.Is(simulated)=true, got %v", err)
		}
	})
}

// --- rollup LRU-hit fast-path tests ---
//
// These tests cover the dedupLRU consult inserted between FastCDC.Next()
// and StoreChunk in rollup.go's chunker emit loop: hit-path / fallback
// flow, manifest invariant, and BlockState preservation.
//
// Test payload sizing: 256 KiB is well under MinChunkSize (1 MiB), so
// FastCDC.Next emits exactly ONE chunk per rollup pass with final=true.
// This gives the tests a deterministic single hash to reason about.

// programmableFBS wraps a real EngineFileBlockStore and lets a test
// override AddRef's behavior. Used by the LRU-fallback (Test 3) and
// other-error-propagates (Test 4) cases where the inner memory store
// alone cannot synthesize the required failure modes.
type programmableFBS struct {
	inner blockstore.EngineFileBlockStore

	addRefCalls atomic.Int64
	// addRefOverride, if non-nil, returns its value instead of delegating
	// to the inner store. Set by the test for ErrUnknownHash / arbitrary-
	// error scenarios.
	addRefOverride func(ctx context.Context, h blockstore.ContentHash, payloadID string, ref blockstore.BlockRef) error
}

func newProgrammableFBS(inner blockstore.EngineFileBlockStore) *programmableFBS {
	return &programmableFBS{inner: inner}
}

func (p *programmableFBS) GetByHash(ctx context.Context, h blockstore.ContentHash) (*blockstore.FileBlock, error) {
	return p.inner.GetByHash(ctx, h)
}
func (p *programmableFBS) Put(ctx context.Context, b *blockstore.FileBlock) error {
	return p.inner.Put(ctx, b)
}
func (p *programmableFBS) Delete(ctx context.Context, id string) error {
	return p.inner.Delete(ctx, id)
}
func (p *programmableFBS) IncrementRefCount(ctx context.Context, id string) error {
	return p.inner.IncrementRefCount(ctx, id)
}
func (p *programmableFBS) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	return p.inner.DecrementRefCount(ctx, id)
}
func (p *programmableFBS) AddRef(ctx context.Context, h blockstore.ContentHash, payloadID string, ref blockstore.BlockRef) error {
	p.addRefCalls.Add(1)
	if p.addRefOverride != nil {
		return p.addRefOverride(ctx, h, payloadID, ref)
	}
	return p.inner.AddRef(ctx, h, payloadID, ref)
}
func (p *programmableFBS) ListPending(ctx context.Context, olderThan time.Duration, limit int) ([]*blockstore.FileBlock, error) {
	return p.inner.ListPending(ctx, olderThan, limit)
}
func (p *programmableFBS) GetFileBlock(ctx context.Context, id string) (*blockstore.FileBlock, error) {
	return p.inner.GetFileBlock(ctx, id)
}
func (p *programmableFBS) ListFileBlocks(ctx context.Context, payloadID string) ([]*blockstore.FileBlock, error) {
	return p.inner.ListFileBlocks(ctx, payloadID)
}

// newFSStoreForRollupLRUTest constructs an FSStore backed by a
// programmableFBS wrapping a memory metadata store. The same store
// instance is used for both the EngineFileBlockStore and the RollupStore
// surfaces so seeded FileBlocks are visible to both. Returns the store
// the FBS wrapper (for AddRef assertions), and the raw memory store (for
// seeding FileBlock rows and reading them back post-rollup).
func newFSStoreForRollupLRUTest(t *testing.T) (*FSStore, *programmableFBS, *memmeta.MemoryMetadataStore) {
	t.Helper()
	mem := memmeta.NewMemoryMetadataStoreWithDefaults()
	wrapped := newProgrammableFBS(mem)
	dir := t.TempDir()
	bc, err := NewWithOptions(dir, 1<<30, 1<<30, wrapped, FSStoreOptions{
		MaxLogBytes:     1 << 30,
		RollupWorkers:   2,
		StabilizationMS: 1,
		RollupStore:     mem,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	return bc, wrapped, mem
}

// hashOfSingleChunk returns the BLAKE3 ContentHash of payload after a
// rollup pass — i.e. the hash that FastCDC emits when reconstructStream
// produces a buffer starting at byte 0 (zero-padded gap) with payload at
// the AppendWrite offset. For payload sized <= MinChunkSize (1 MiB) and
// off=0 the reconstructed buffer is exactly the payload bytes, so the
// chunk hash matches BLAKE3(payload).
func hashOfSingleChunk(payload []byte) blockstore.ContentHash {
	return blake3ContentHash(payload)
}

// TestRollup_FirstChunk_PopulatesLRU: empty LRU; rollup a payload that
// produces hash H; afterwards bc.dedupLRU.Has(H, payloadID) is true
// (first-write seeding, scoped to the writing payload — #669).
func TestRollup_FirstChunk_PopulatesLRU(t *testing.T) {
	bc, _, _ := newFSStoreForRollupLRUTest(t)

	const pid = "first-pop"
	payload := bytes.Repeat([]byte{0xAA}, 256*1024)
	expectedHash := hashOfSingleChunk(payload)

	if bc.dedupLRU.Has(expectedHash, pid) {
		t.Fatal("precondition: dedupLRU should not contain hash before rollup")
	}

	runRollupOnce(t, bc, pid, payload)

	if !bc.dedupLRU.Has(expectedHash, pid) {
		t.Fatal("post-rollup: dedupLRU.Has(H, pid) is false — first-write LRU seeding did not run")
	}
}

// TestRollup_LRUHit_SkipsStoreChunk: seed dedupLRU with (hash H,
// payloadID="hitfile") (the payload that will run rollup);
// pre-populate FBS with a FileBlock row for H so AddRef succeeds;
// rollup the payload. Assert
//   - AddRef invoked exactly once with hash=H (counter +1)
//   - StoreChunk skipped: pre-deleted CAS file is NOT recreated on disk
//   - blocks slice still contains a BlockRef{Hash:H,Offset:..,Size:..}
//     (verified via the ObjectIDPersister capture)
//
// #669: the LRU is keyed by (hash, payloadID); the seed payloadID
// MUST match the rollup payloadID for the hit path to fire. Cross-payload
// short-circuit is intentionally not supported (see
// TestRollup_CrossPayload_LRUMisses below).
func TestRollup_LRUHit_SkipsStoreChunk(t *testing.T) {
	bc, wrapped, mem := newFSStoreForRollupLRUTest(t)
	ctx := context.Background()

	const pid = "hitfile"
	payload := bytes.Repeat([]byte{0xBE}, 256*1024)
	h := hashOfSingleChunk(payload)

	// Seed a FileBlock row in the memory store so AddRef can find it.
	// State=Remote, RefCount=1 — represents a previously rolled-up block.
	if err := mem.Put(ctx, &blockstore.FileBlock{
		ID:       "seed-payload/block-0",
		Hash:     h,
		State:    blockstore.BlockStateRemote,
		RefCount: 1,
		DataSize: uint32(len(payload)),
	}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	// Seed the LRU with (h, pid) — the PRECONDITION the hit path consults.
	bc.dedupLRU.Put(h, pid)
	if !bc.dedupLRU.Has(h, pid) {
		t.Fatal("precondition: LRU.Has(h, pid) is false after Put")
	}

	// Install a persister to capture the BlockRef manifest — proves the
	// blocks slice still received the BlockRef append unconditionally
	// (manifest invariant).
	var mu sync.Mutex
	var captured []capturedPersist
	bc.SetObjectIDPersister(func(_ context.Context, pid string, blocks []blockstore.BlockRef, oid blockstore.ObjectID) error {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]blockstore.BlockRef, len(blocks))
		copy(cp, blocks)
		captured = append(captured, capturedPersist{payloadID: pid, blocks: cp, objectID: oid})
		return nil
	})

	// Pre-delete any CAS file for h so we can detect a re-create
	// (would indicate StoreChunk was NOT skipped).
	casPath := bc.chunkPath(h)
	_ = os.Remove(casPath)

	baseAddRef := wrapped.addRefCalls.Load()

	runRollupOnce(t, bc, pid, payload)

	gotAddRef := wrapped.addRefCalls.Load() - baseAddRef
	if gotAddRef != 1 {
		t.Fatalf("AddRef call count delta: got %d want 1", gotAddRef)
	}

	// CAS file must NOT exist — StoreChunk skipped.
	if _, err := os.Stat(casPath); err == nil {
		t.Fatalf("CAS chunk %s exists post-rollup; StoreChunk was NOT skipped on LRU hit", casPath)
	}

	// Manifest must contain a BlockRef with hash h.
	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("persister captures: got %d want 1", len(captured))
	}
	foundH := false
	for _, br := range captured[0].blocks {
		if br.Hash == h {
			foundH = true
		}
	}
	if !foundH {
		t.Fatalf("D-02 manifest invariant violated: BlockRef for hash %s missing from manifest", h)
	}
}

// TestRollup_AddRefReturnsErrUnknownHash_FallsBackToStoreChunk: LRU has
// (h, pid); programmable FBS returns ErrUnknownHash on every AddRef
// (simulating a TOCTOU sweep by engine.Delete cascade between
// LRU populate and the next rollup pass); rollup must proceed to
// StoreChunk normally.
func TestRollup_AddRefReturnsErrUnknownHash_FallsBackToStoreChunk(t *testing.T) {
	bc, wrapped, _ := newFSStoreForRollupLRUTest(t)

	const pid = "fallback"
	payload := bytes.Repeat([]byte{0xCA}, 256*1024)
	h := hashOfSingleChunk(payload)

	bc.dedupLRU.Put(h, pid)
	wrapped.addRefOverride = func(_ context.Context, _ blockstore.ContentHash, _ string, _ blockstore.BlockRef) error {
		return blockstore.ErrUnknownHash
	}

	casPath := bc.chunkPath(h)
	if _, err := os.Stat(casPath); err == nil {
		_ = os.Remove(casPath)
	}

	runRollupOnce(t, bc, pid, payload)

	if wrapped.addRefCalls.Load() != 1 {
		t.Fatalf("AddRef calls: got %d want 1 (LRU hit triggered AddRef)", wrapped.addRefCalls.Load())
	}
	if _, err := os.Stat(casPath); err != nil {
		t.Fatalf("CAS chunk %s missing post-rollup; ErrUnknownHash fallback did NOT call StoreChunk: %v", casPath, err)
	}
	if !bc.dedupLRU.Has(h, pid) {
		t.Fatal("LRU not (re-)populated after StoreChunk fallback path — post-persister PutMany missing")
	}
}

// TestRollup_AddRefError_OtherThan_ErrUnknownHash_Propagates: AddRef
// returns errors.New(...); rollupFile must return the wrapped error —
// NO silent fallback (error-surfacing contract).
func TestRollup_AddRefError_OtherThan_ErrUnknownHash_Propagates(t *testing.T) {
	bc, wrapped, _ := newFSStoreForRollupLRUTest(t)
	ctx := context.Background()

	payload := bytes.Repeat([]byte{0xDE}, 256*1024)
	h := hashOfSingleChunk(payload)
	bc.dedupLRU.Put(h, "errpath")

	simulated := errors.New("metadata: postgres down")
	wrapped.addRefOverride = func(_ context.Context, _ blockstore.ContentHash, _ string, _ blockstore.BlockRef) error {
		return simulated
	}

	if err := bc.AppendWrite(ctx, "errpath", payload, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if bc.EarliestStableForTest("errpath") {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !bc.EarliestStableForTest("errpath") {
		t.Fatal("dirty interval did not stabilize")
	}

	err := bc.rollupFile(ctx, "errpath")
	if err == nil {
		t.Fatal("rollupFile: want wrapped AddRef error, got nil")
	}
	if !errors.Is(err, simulated) {
		t.Fatalf("rollupFile error: want errors.Is(simulated)=true, got %v", err)
	}
}

// TestRollup_ComputeObjectID_StableAcrossPayloads: write two payloads
// with identical content; ObjectID and BlockRefs of each match.
// Verifies the manifest invariant: the chunker and ComputeObjectID
// produce identical output across payloads regardless of LRU outcome.
//
// #669: under compound-key LRU scoping both passes MISS the LRU
// (each payload only ever hits LRU entries IT populated). The test
// therefore exercises the StoreChunk path twice for the same content;
// content-addressed CAS makes the second Put idempotent. The pre-#669
// version of this test conflated cross-payload dedup with LRU
// short-circuit — the LRU short-circuit is now intentionally scoped to
// same-payload idempotent rewrites; cross-payload dedup happens via
// the regular CAS + FileBlockStore.GetByHash path.
func TestRollup_ComputeObjectID_StableAcrossPayloads(t *testing.T) {
	bc, _, _ := newFSStoreForRollupLRUTest(t)

	payload := bytes.Repeat([]byte{0xF1}, 256*1024)

	var mu sync.Mutex
	var captured []capturedPersist
	bc.SetObjectIDPersister(func(_ context.Context, pid string, blocks []blockstore.BlockRef, oid blockstore.ObjectID) error {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]blockstore.BlockRef, len(blocks))
		copy(cp, blocks)
		captured = append(captured, capturedPersist{payloadID: pid, blocks: cp, objectID: oid})
		return nil
	})

	// First payload — LRU miss → StoreChunk + post-persister LRU seed.
	runRollupOnce(t, bc, "oid-A", payload)
	// Second payload — different payloadID, same content. LRU miss
	// under compound-key scoping → StoreChunk again (idempotent under
	// CAS). Manifest must still be identical.
	runRollupOnce(t, bc, "oid-B", payload)

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("persister captures: got %d want 2", len(captured))
	}
	if captured[0].objectID != captured[1].objectID {
		t.Fatalf("ObjectID drift across LRU miss vs hit: %s vs %s",
			captured[0].objectID.String(), captured[1].objectID.String())
	}
	if len(captured[0].blocks) != len(captured[1].blocks) {
		t.Fatalf("BlockRef manifest length drift: A=%d B=%d", len(captured[0].blocks), len(captured[1].blocks))
	}
	for i := range captured[0].blocks {
		a, b := captured[0].blocks[i], captured[1].blocks[i]
		if a.Hash != b.Hash || a.Offset != b.Offset || a.Size != b.Size {
			t.Fatalf("BlockRef[%d] drift between miss and hit passes: A=%+v B=%+v", i, a, b)
		}
	}
}

// TestRollup_LRUHit_NoBlockStateMutation: pre-seed a FileBlock row at
// State=Remote, RefCount=5; run a rollup that triggers the LRU hit
// AddRef path; assert the row is preserved at State=Remote with
// RefCount=6 post-rollup.
func TestRollup_LRUHit_NoBlockStateMutation(t *testing.T) {
	bc, _, mem := newFSStoreForRollupLRUTest(t)
	ctx := context.Background()

	payload := bytes.Repeat([]byte{0x07}, 256*1024)
	h := hashOfSingleChunk(payload)

	const seedID = "seedfile/block-0"
	const seedRefCount uint32 = 5
	const seedState = blockstore.BlockStateRemote
	if err := mem.Put(ctx, &blockstore.FileBlock{
		ID:       seedID,
		Hash:     h,
		State:    seedState,
		RefCount: seedRefCount,
		DataSize: uint32(len(payload)),
	}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	// #669: LRU keyed by (hash, payloadID). Seed under the SAME
	// payloadID the rollup will run on so the hit path fires.
	const pid = "no-state-mutation"
	bc.dedupLRU.Put(h, pid)

	runRollupOnce(t, bc, pid, payload)

	// Read the row back via GetByHash (the row might be looked up via
	// any matching ID; GetByHash is the public path).
	fb, err := mem.GetByHash(ctx, h)
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if fb == nil {
		t.Fatal("post-rollup: FileBlock for h missing — AddRef somehow dropped the row")
	}
	if fb.State != seedState {
		t.Fatalf("BlockState mutated by AddRef: got %v want %v (D-27 STATE invariant violated)", fb.State, seedState)
	}
	if fb.RefCount != seedRefCount+1 {
		t.Fatalf("RefCount: got %d want %d (seed=%d + 1 AddRef)", fb.RefCount, seedRefCount+1, seedRefCount)
	}
}
