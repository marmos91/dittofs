package blockstoretest

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// AppendFactory creates a fresh BlockStoreAppend for a single subtest
// along with a cleanup closure. Same shape as Factory; the separation
// exists because only the fs backend implements BlockStoreAppend, so
// s3 / memory backends supply only Factory.
//
// AppendFactory is a defined type (not a type alias) to mirror Factory.
type AppendFactory func(t *testing.T) (blockstore.BlockStoreAppend, func())

// BlockStoreAppendConformance runs the random-write absorber suite
// against any BlockStoreAppend implementation. Mirrors the structure
// of the original D-22 5-scenario suite but is re-expressed against
// the interface surface declared in Phase 17:
//
//   - AppendLogRoundTrip — AppendWrite payload, wait for the rollup
//     to surface chunks via Walk, then DeleteLog tombstones the log.
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
// (pkg/blockstore/local/fs/appendlog_internals_test.go) that hold the
// scenarios verbatim and call the fs-internal *ForTest probes. They
// were lifted out of the deleted pkg/blockstore/local/localtest/
// package by Plan 17-06.
func BlockStoreAppendConformance(t *testing.T, factory AppendFactory) {
	t.Helper()
	t.Run("AppendLogRoundTrip", func(t *testing.T) { testAppendLogRoundTrip(t, factory) })
	t.Run("PressureChannel_INV05", func(t *testing.T) { testPressureChannelINV05(t, factory) })
	t.Run("TornWriteRecovery_LSL06", func(t *testing.T) { testTornWriteRecoveryLSL06(t, factory) })
	t.Run("ConcurrentStorm", func(t *testing.T) { testConcurrentStorm(t, factory) })
	t.Run("RollupOffsetMonotone_INV03", func(t *testing.T) { testRollupOffsetMonotoneINV03(t, factory) })
}

// testAppendLogRoundTrip asserts the LSL-01/02/03/05 end-to-end
// behavior on the public BlockStoreAppend surface: an AppendWrite
// eventually surfaces content-addressed chunks via Walk (the rollup
// loop emits them via Put), and DeleteLog tombstones the per-file
// append log. The scenario does NOT pin the chunk count (FastCDC
// boundaries are payload-dependent) nor the timing — backends with
// background rollup pools may need to poll Walk for up to a few
// seconds before chunks appear.
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
	// does not expose a synchronous-rollup hook (per D-09 — that
	// stays internal to the fs backend), so a deadline-driven poll
	// is the portable way to assert the rollup advances.
	deadline := time.Now().Add(10 * time.Second)
	var chunkCount int
	for time.Now().Before(deadline) {
		chunkCount = 0
		err := bs.Walk(ctx, func(_ blockstore.ContentHash, _ blockstore.Meta) error {
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

	// DeleteLog tombstones the per-file append log. After it
	// returns, subsequent AppendWrites for the same payloadID are
	// expected to fail (per BlockStoreAppend.DeleteLog godoc). The
	// suite does not pin which error code surfaces — only that the
	// call itself succeeds and that the already-rolled-up chunks
	// remain in the store (orphan-chunk sweep is GC's job, not
	// DeleteLog's).
	if err := bs.DeleteLog(ctx, payloadID); err != nil {
		t.Fatalf("DeleteLog: %v", err)
	}

	postCount := 0
	err := bs.Walk(ctx, func(_ blockstore.ContentHash, _ blockstore.Meta) error {
		postCount++
		return nil
	})
	if err != nil {
		t.Fatalf("Walk after DeleteLog: %v", err)
	}
	if postCount == 0 {
		t.Fatal("DeleteLog removed previously rolled-up chunks; GC sweep is responsible for those, not DeleteLog")
	}
}

// testPressureChannelINV05 asserts INV-05 — once the in-memory
// append-log budget is exceeded, subsequent AppendWrites block until
// the rollup drains the budget.
//
// SKIPPED on the BlockStoreAppend interface surface: the assertion
// requires fs-internal probes (SetMaxLogBytesForTest to shrink the
// budget and LogBytesTotalForTest to verify post-drain accounting).
// Neither probe is on the interface, and the suite cannot legally
// observe internal byte accounting through the public surface. The
// fs backend continues to exercise this scenario via the legacy
// fs-internal appendlog_internals_test.go scenarios (moved out of the deleted localtest package by Plan 17-06).
func testPressureChannelINV05(t *testing.T, factory AppendFactory) {
	t.Skip("PressureChannel_INV05 is not portable to BlockStoreAppend: requires fs-internal SetMaxLogBytesForTest + LogBytesTotalForTest probes. The fs backend exercises this via pkg/blockstore/local/fs/appendlog_internals_test.go (the scenarios were moved out of the deleted localtest package by Plan 17-06).")
}

// testTornWriteRecoveryLSL06 asserts LSL-06 — appending random
// garbage past the clean tail does not corrupt surviving records;
// after reopen, the interval tree carries exactly the prior record
// count and the log is truncated at the first bad CRC.
//
// SKIPPED on the BlockStoreAppend interface surface: the assertion
// requires direct write access to the on-disk log file
// (<base>/logs/<payloadID>.log) plus fs-internal probes
// (BaseDirForTest, RollupStoreForTest, ReopenForTest,
// IntervalsLenForTest). The interface intentionally does not expose
// on-disk paths or recovery hooks — recovery is a backend-internal
// concern. The fs backend continues to exercise this scenario via
// the fs-internal appendlog_internals_test.go scenarios in
// pkg/blockstore/local/fs/ (moved out of the deleted localtest
// package by Plan 17-06).
func testTornWriteRecoveryLSL06(t *testing.T, factory AppendFactory) {
	t.Skip("TornWriteRecovery_LSL06 is not portable to BlockStoreAppend: requires direct on-disk log access + ReopenForTest / IntervalsLenForTest fs-internal probes. The fs backend exercises this via pkg/blockstore/local/fs/appendlog_internals_test.go (the scenarios were moved out of the deleted localtest package by Plan 17-06).")
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
// fs-internal appendlog_internals_test.go scenarios (moved out of the
// deleted localtest package by Plan 17-06), which use the
// LogBytesTotalForTest / RollupOffsetForTest internal probes for
// deterministic assertions instead of polling Walk.
func testConcurrentStorm(t *testing.T, factory AppendFactory) {
	t.Skip("ConcurrentStorm is not portable to BlockStoreAppend: the only portable rollup-progress hook is polling Walk, which is timing-dependent and flakes on CI runners with slow shared IO. The fs backend exercises this via pkg/blockstore/local/fs/appendlog_internals_test.go using fs-internal probes for deterministic assertions.")
}

// testRollupOffsetMonotoneINV03 asserts INV-03 — if metadata has
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
// continues to exercise this scenario via the legacy
// fs-internal appendlog_internals_test.go scenarios (moved out of the deleted localtest package by Plan 17-06).
func testRollupOffsetMonotoneINV03(t *testing.T, factory AppendFactory) {
	t.Skip("RollupOffsetMonotone_INV03 is not portable to BlockStoreAppend: requires header-CRC corruption + ReopenForTest / HeaderRollupOffsetForTest fs-internal probes. The fs backend exercises this via pkg/blockstore/local/fs/appendlog_internals_test.go (the scenarios were moved out of the deleted localtest package by Plan 17-06).")
}
