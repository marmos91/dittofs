package blockstoretest

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
)

// AppendFactory creates a fresh BlockStoreAppend for a single subtest
// along with a cleanup closure. Same shape as Factory; the separation
// exists because only the fs backend implements BlockStoreAppend, so
// s3 / memory backends supply only Factory.
//
// AppendFactory is a defined type (not a type alias) to mirror Factory.
type AppendFactory func(t *testing.T) (block.BlockStoreAppend, func())

// BlockStoreAppendConformance runs the random-write absorber suite
// against any BlockStoreAppend implementation.
//
//   - AppendLogRoundTrip — AppendWrite payload, wait for the rollup
//     to surface chunks via Walk, then DeleteAppendLog tombstones the log.
//   - PressureChannel_INV05 — backpressure when the in-memory log
//     budget is exceeded. SKIPPED on the interface-only surface:
//     requires fs-internal SetMaxLogBytesForTest /
//     LogBytesTotalForTest probes.
//   - TornWriteRecovery_LSL06 — recovery from a torn append-log
//     write. SKIPPED on the interface-only surface: requires direct
//     access to the on-disk log file and the fs-internal
//     ReopenForTest / IntervalsLenForTest probes.
//   - ConcurrentStorm — many goroutines AppendWrite to many files;
//     no deadlock and at least one chunk per file surfaces via Walk.
//   - RollupOffsetMonotone_INV03 — header reconciliation across a
//     simulated crash window. SKIPPED on the interface-only surface:
//     requires header CRC injection on the on-disk log.
//
// The three SKIPPED scenarios are exercised by the fs backend via
// fs-internal `_test.go` files
// (pkg/block/local/fs/appendlog_internals_test.go) that hold the
// scenarios verbatim and call the fs-internal *ForTest probes.
func BlockStoreAppendConformance(t *testing.T, factory AppendFactory) {
	t.Helper()
	t.Run("AppendLogRoundTrip", func(t *testing.T) { testAppendLogRoundTrip(t, factory) })
	t.Run("RecreateAfterDeleteAppendLog", func(t *testing.T) { testRecreateAfterDeleteAppendLog(t, factory) })
	t.Run("PressureChannel_INV05", func(t *testing.T) { testPressureChannelINV05(t, factory) })
	t.Run("TornWriteRecovery_LSL06", func(t *testing.T) { testTornWriteRecoveryLSL06(t, factory) })
	t.Run("ConcurrentStorm", func(t *testing.T) { testConcurrentStorm(t, factory) })
	t.Run("RollupOffsetMonotone_INV03", func(t *testing.T) { testRollupOffsetMonotoneINV03(t, factory) })
	t.Run("DataExtents", func(t *testing.T) { testDataExtents(t, factory) })
}

// dataExtenter is the local-tier DataExtents surface (local.LocalStore). It is
// declared inline here rather than imported to keep blockstoretest free of a
// dependency on pkg/block/local.
type dataExtenter interface {
	DataExtents(ctx context.Context, payloadID string, fileSize uint64) ([][2]uint64, error)
}

// testDataExtents asserts the coverage invariant that backs NFSv4.2 SEEK /
// READ_PLUS (#1481): DataExtents must report a data extent over every
// written-but-not-yet-rolled-up byte (never a false hole, which RFC 7862
// forbids), and must never report data outside [0, fileSize). It does NOT
// assert exact extents — the precise fs backend returns one extent per written
// region while the conservative memory backend collapses the whole written span
// into a single extent; both satisfy coverage.
//
// DataExtents is queried IMMEDIATELY after the writes, before the rollup's
// stabilization window can elapse: on the fs backend a rolled-up region leaves
// the append-log logIndex (the engine re-adds it from the CAS manifest, but the
// bare local store under test does not), so querying pre-rollup keeps the
// local-tier coverage assertion deterministic.
func testDataExtents(t *testing.T, factory AppendFactory) {
	bs, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	de, ok := bs.(dataExtenter)
	if !ok {
		t.Skip("backend does not implement DataExtents")
	}

	const payloadID = "data-extents"
	// Two disjoint writes separated by a large gap (a hole).
	type wr struct {
		off uint64
		n   uint64
	}
	writes := []wr{{0, 4096}, {1 << 20, 4096}}
	for _, w := range writes {
		if err := bs.AppendWrite(ctx, payloadID, bytes.Repeat([]byte{0xCD}, int(w.n)), w.off); err != nil {
			t.Fatalf("AppendWrite(off=%d): %v", w.off, err)
		}
	}
	fileSize := writes[len(writes)-1].off + writes[len(writes)-1].n

	ext, err := de.DataExtents(ctx, payloadID, fileSize)
	if err != nil {
		t.Fatalf("DataExtents: %v", err)
	}

	// Bounds + ordering invariant: sorted, non-overlapping, within [0,fileSize).
	var prevEnd uint64
	for i, e := range ext {
		if e[0] >= e[1] {
			t.Errorf("extent %d = [%d,%d): empty/inverted", i, e[0], e[1])
		}
		if e[1] > fileSize {
			t.Errorf("extent %d = [%d,%d): exceeds fileSize %d", i, e[0], e[1], fileSize)
		}
		if e[0] < prevEnd {
			t.Errorf("extent %d = [%d,%d): overlaps/precedes previous end %d", i, e[0], e[1], prevEnd)
		}
		prevEnd = e[1]
	}

	// Coverage invariant: every written byte falls inside some returned extent.
	covered := func(off uint64) bool {
		for _, e := range ext {
			if off >= e[0] && off < e[1] {
				return true
			}
		}
		return false
	}
	for _, w := range writes {
		for _, b := range []uint64{w.off, w.off + w.n - 1} {
			if !covered(b) {
				t.Errorf("written byte %d not covered by any DataExtents range %v", b, ext)
			}
		}
	}
}

// testAppendLogRoundTrip asserts the end-to-end behavior on the
// public BlockStoreAppend surface: an AppendWrite eventually surfaces
// content-addressed chunks via Walk (the rollup loop emits them via
// Put), and DeleteAppendLog tombstones the per-file append log. The
// scenario does NOT pin the chunk count (FastCDC boundaries are
// payload-dependent) nor the timing — backends with background
// rollup pools may need to poll Walk for up to a few seconds before
// chunks appear.
func testAppendLogRoundTrip(t *testing.T, factory AppendFactory) {
	bs, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	// 8 MiB payload: reliably crosses the FastCDC min chunk size so
	// the chunker emits at least one chunk on the first rollup pass.
	const payloadID = "round-trip"
	payload := bytes.Repeat([]byte{0xAB}, 8*1024*1024)
	if err := bs.AppendWrite(ctx, payloadID, payload, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	// Poll Walk for the first emitted chunk. The interface surface
	// does not expose a synchronous-rollup hook (that stays internal
	// to the fs backend), so a deadline-driven poll is the portable
	// way to assert the rollup advances.
	deadline := time.Now().Add(10 * time.Second)
	var chunkCount int
	for time.Now().Before(deadline) {
		chunkCount = 0
		err := bs.Walk(ctx, func(_ block.ContentHash, _ block.Meta) error {
			chunkCount++
			return nil
		})
		if err != nil {
			t.Fatalf("Walk while waiting for rollup: %v", err)
		}
		if chunkCount > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if chunkCount == 0 {
		t.Fatal("rollup did not emit any chunks within 10s — Walk surfaced 0 objects")
	}

	// DeleteAppendLog resets the per-file append log. After it returns, a
	// subsequent AppendWrite for the same payloadID must succeed (per
	// BlockStoreAppend.DeleteAppendLog godoc — this resurrect path runs
	// on delete to reclaim the deleted file's log). The already-rolled-up
	// chunks remain (orphan-chunk sweep is GC's job, not DeleteAppendLog's).
	if err := bs.DeleteAppendLog(ctx, payloadID); err != nil {
		t.Fatalf("DeleteAppendLog: %v", err)
	}

	postCount := 0
	err := bs.Walk(ctx, func(_ block.ContentHash, _ block.Meta) error {
		postCount++
		return nil
	})
	if err != nil {
		t.Fatalf("Walk after DeleteAppendLog: %v", err)
	}
	if postCount == 0 {
		t.Fatal("DeleteAppendLog removed previously rolled-up chunks; GC sweep is responsible for those, not DeleteAppendLog")
	}
}

// testRecreateAfterDeleteAppendLog asserts that DeleteAppendLog does NOT
// permanently tombstone a payloadID: a subsequent AppendWrite for the
// same payloadID must succeed and start a fresh log. This is required
// on delete: DeleteAppendLog must reclaim the deleted file's log and
// leave the store ready for a fresh one. Files created after #1166
// PR-3 get UUID-based PayloadIDs, so recreate-at-same-path uses a
// fresh content_id (historically a path-derived recreate reused the
// id — POSIX recreate via NFSv3, pjdfstest chmod/12.t, unlink/14.t,
// open/00.t).
//
// The scenario writes once, deletes the log, then writes again at the
// same payloadID and asserts the second write returns nil.
func testRecreateAfterDeleteAppendLog(t *testing.T, factory AppendFactory) {
	bs, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	const payloadID = "recreate-after-delete"
	payload := bytes.Repeat([]byte{0xCD}, 64)

	if err := bs.AppendWrite(ctx, payloadID, payload, 0); err != nil {
		t.Fatalf("first AppendWrite: %v", err)
	}
	if err := bs.DeleteAppendLog(ctx, payloadID); err != nil {
		t.Fatalf("DeleteAppendLog: %v", err)
	}
	if err := bs.AppendWrite(ctx, payloadID, payload, 0); err != nil {
		t.Fatalf("AppendWrite after DeleteAppendLog (store must accept a fresh log after delete): %v", err)
	}
}

// testPressureChannelINV05 asserts that once the in-memory
// append-log budget is exceeded, subsequent AppendWrites block until
// the rollup drains the budget.
//
// SKIPPED on the BlockStoreAppend interface surface: the assertion
// requires fs-internal probes (SetMaxLogBytesForTest to shrink the
// budget and LogBytesTotalForTest to verify post-drain accounting).
// Neither probe is on the interface, and the suite cannot legally
// observe internal byte accounting through the public surface. The
// fs backend continues to exercise this scenario via the legacy
// fs-internal appendlog_internals_test.go scenarios.
func testPressureChannelINV05(t *testing.T, factory AppendFactory) {
	t.Skip("PressureChannel_INV05 is not portable to BlockStoreAppend: requires fs-internal SetMaxLogBytesForTest + LogBytesTotalForTest probes. The fs backend exercises this via pkg/block/local/fs/appendlog_internals_test.go (the scenarios were moved out of the deleted localtest package by Plan 17-06).")
}

// testTornWriteRecoveryLSL06 asserts that appending random garbage
// past the clean tail does not corrupt surviving records after
// reopen: the interval tree carries exactly the prior record count
// and the log is truncated at the first bad CRC.
//
// SKIPPED on the BlockStoreAppend interface surface: the assertion
// requires direct write access to the on-disk log file
// (<base>/logs/<payloadID>.log) plus fs-internal probes
// (BaseDirForTest, RollupStoreForTest, ReopenForTest,
// IntervalsLenForTest). The interface intentionally does not expose
// on-disk paths or recovery hooks — recovery is a backend-internal
// concern. The fs backend continues to exercise this scenario via
// the fs-internal appendlog_internals_test.go scenarios in
// pkg/block/local/fs/.
func testTornWriteRecoveryLSL06(t *testing.T, factory AppendFactory) {
	t.Skip("TornWriteRecovery_LSL06 is not portable to BlockStoreAppend: requires direct on-disk log access + ReopenForTest / IntervalsLenForTest fs-internal probes. The fs backend exercises this via pkg/block/local/fs/appendlog_internals_test.go (the scenarios were moved out of the deleted localtest package by Plan 17-06).")
}

// testConcurrentStorm asserts no deadlock and no silent data loss
// when many goroutines AppendWrite to many different files under
// concurrent rollup pressure. The legacy fs-suite version also
// asserted a byte-accounting invariant via LogBytesTotalForTest /
// RollupOffsetForTest, neither of which is on the BlockStoreAppend
// interface. The interface-portable assertion instead waits for the
// rollup to surface at least one chunk per file via Walk.
//
// SKIPPED on the BlockStoreAppend interface surface: the rollup
// pipeline is asynchronous and the only portable observation hook is
// polling Walk, which is timing-dependent on the backend's rollup
// loop. Local runs complete in under 2 s; CI runners under shared-IO
// contention have been observed to take >3 minutes (and sometimes
// never produce a chunk within any reasonable timeout). The
// interface-portable "at least one chunk surfaces" assertion is too
// flaky on CI to keep in the conformance suite.
//
// The fs backend continues to exercise this scenario via the legacy
// fs-internal appendlog_internals_test.go scenarios, which use the
// LogBytesTotalForTest / RollupOffsetForTest internal probes for
// deterministic assertions instead of polling Walk.
func testConcurrentStorm(t *testing.T, factory AppendFactory) {
	t.Skip("ConcurrentStorm is not portable to BlockStoreAppend: the only portable rollup-progress hook is polling Walk, which is timing-dependent and flakes on CI runners with slow shared IO. The fs backend exercises this via pkg/block/local/fs/appendlog_internals_test.go using fs-internal probes for deterministic assertions.")
}

// testRollupOffsetMonotoneINV03 asserts that if metadata has
// advanced past the on-disk header's rollup_offset (simulating a
// crash between SetRollupOffset and advanceRollupOffset), recovery
// writes the header forward to match metadata and never regresses
// the offset.
//
// SKIPPED on the BlockStoreAppend interface surface: the assertion
// requires direct header-CRC corruption on the on-disk log
// (RecomputeHeaderCRCForTest, byte-edit of <base>/logs/<id>.log,
// ReopenForTest, HeaderRollupOffsetForTest). The interface does not
// expose any on-disk path or recovery probe. The fs backend
// continues to exercise this scenario via the legacy fs-internal
// appendlog_internals_test.go scenarios.
func testRollupOffsetMonotoneINV03(t *testing.T, factory AppendFactory) {
	t.Skip("RollupOffsetMonotone_INV03 is not portable to BlockStoreAppend: requires header-CRC corruption + ReopenForTest / HeaderRollupOffsetForTest fs-internal probes. The fs backend exercises this via pkg/block/local/fs/appendlog_internals_test.go (the scenarios were moved out of the deleted localtest package by Plan 17-06).")
}
