package fs

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestRollup_OutOfOrderArrivals_AllRecordsRolledUp is the load-bearing
// regression case from the Direction-1 redesign proposal. Four AppendWrites
// at file offsets {32768, 458752, 0, 1540096} arrive in that order — the
// per-file mu serializes them so on-disk frame order matches arrival order,
// which is exactly the parallel-write shape that broke the legacy rollup.
//
// Pre-fix: the linear scan from rollup_offset read frame 0 (file_off=32768),
// then frame 1 (file_off=458752) — past the chosen stable interval — and
// broke out. Frame 2 (file_off=0) was never read for the [0, …) stable
// interval. Net effect: recs=0, no chunks for that region, rollup_offset
// stalled or skipped past it.
//
// Post-fix: every stable interval's EntriesForInterval lookup finds the
// matching arrival-order entries regardless of where they sit in the log,
// so all four regions produce chunks and rollup_offset advances to EOF.
func TestRollup_OutOfOrderArrivals_AllRecordsRolledUp(t *testing.T) {
	bc, rs := newRollupFSStore(t, 1<<30, 10)
	ctx := context.Background()

	// Distinct fill bytes per region so content-addressing produces
	// distinguishable chunks (no inadvertent dedup collapse). Each
	// payload is 32 KiB — large enough to clear the FastCDC minimum
	// chunk size and small enough to keep the test fast.
	const payloadLen = 32 * 1024
	type write struct {
		fileOff uint64
		fill    byte
	}
	writes := []write{
		{fileOff: 32768, fill: 0xA1},
		{fileOff: 458752, fill: 0xA2},
		{fileOff: 0, fill: 0xA3},
		{fileOff: 1540096, fill: 0xA4},
	}
	for _, w := range writes {
		payload := bytes.Repeat([]byte{w.fill}, payloadLen)
		if err := bc.AppendWrite(ctx, "file-ooo", payload, w.fileOff); err != nil {
			t.Fatalf("AppendWrite at %d: %v", w.fileOff, err)
		}
	}

	// All four intervals must eventually roll up. Wait for the log
	// budget to decrement (the rollup pool fires the ConsumeUpTo +
	// logBytesTotal.Add(-reclaimed) sequence once chunks are durable).
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		off, err := rs.GetRollupOffset(ctx, "file-ooo")
		if err != nil {
			t.Fatalf("GetRollupOffset: %v", err)
		}
		// Expected end-state rollup_offset: header + 4 * (frame + payload).
		wantOff := uint64(logHeaderSize) + 4*(uint64(recordFrameOverhead)+uint64(payloadLen))
		if off >= wantOff {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	off, err := rs.GetRollupOffset(ctx, "file-ooo")
	if err != nil {
		t.Fatalf("GetRollupOffset: %v", err)
	}
	wantOff := uint64(logHeaderSize) + 4*(uint64(recordFrameOverhead)+uint64(payloadLen))
	if off < wantOff {
		t.Fatalf("rollup_offset did not advance to EOF: got %d want >= %d (pre-fix bug shape)", off, wantOff)
	}

	// At least one chunk must exist in blocks/ for each write — since
	// each write filled a distinct region with a distinct byte, dedup
	// cannot collapse them across writes. With FastCDC the chunk count
	// may exceed 4 (large payloads split), but it must NOT be < 4. The
	// pre-fix bug would have produced 1 (rec#0 only — and even that one
	// might have been lost depending on stabilization ordering).
	if n := countChunksInBlocks(t, bc.baseDir); n < 4 {
		t.Fatalf("expected at least 4 chunks in blocks/, found %d — Direction-1 bug-shape regression", n)
	}

	// logBytesTotal must have dropped to zero (every dirty interval
	// consumed). A non-zero residue would indicate the log-budget
	// release missed an interval.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bc.logBytesTotal.Load() == 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if got := bc.logBytesTotal.Load(); got != 0 {
		t.Fatalf("logBytesTotal nonzero after full out-of-order rollup: %d", got)
	}
}

// TestRollup_StalledFence_ChunksStillCommitted asserts that even when the
// compaction fence cannot advance because of a record at the head of the
// log waiting on stabilization, later stable intervals still produce
// chunks. This is the R-7 path in the proposal — chunks are durable
// content-addressed bytes, and tree.ConsumeUpTo runs regardless of fence
// stall in the typical case.
//
// The simulation uses a small stabilization window (50 ms) and arranges
// for the FIRST written record's interval to remain in the dirty tree
// (via a later overlapping write to the same offset that refreshes
// Touched on the same interval, pushing its stabilization). The LATER
// records at distinct, non-overlapping file regions stabilize and roll
// up — producing chunks even while the head record waits. Verifies the
// chunk count and a non-empty blocks/ tree before the head-write
// stabilizes.
func TestRollup_StalledFence_ChunksStillCommitted(t *testing.T) {
	bc, rs := newRollupFSStore(t, 1<<30, 50)
	ctx := context.Background()

	const payloadLen = 16 * 1024
	// Head write at file_off=0 (lowest logPos). The test will refresh
	// Touched on this interval just before the stabilization deadline
	// so its stabilization keeps slipping; meanwhile the other writes
	// stabilize and roll up.
	head := bytes.Repeat([]byte{0xB1}, payloadLen)
	if err := bc.AppendWrite(ctx, "file-stall", head, 0); err != nil {
		t.Fatalf("AppendWrite head: %v", err)
	}
	// Later writes at distinct, non-overlapping regions.
	for i, fill := range []byte{0xB2, 0xB3, 0xB4} {
		payload := bytes.Repeat([]byte{fill}, payloadLen)
		off := uint64((i + 1) * 524288)
		if err := bc.AppendWrite(ctx, "file-stall", payload, off); err != nil {
			t.Fatalf("AppendWrite[%d]: %v", i, err)
		}
	}

	// Refresh the head interval a few times to keep it un-stable
	// while the others stabilize. Each refresh writes the same content
	// to file_off=0; per-file mu serializes the appends, and the tree
	// merges the overlapping insert, bumping Touched.
	for i := 0; i < 3; i++ {
		time.Sleep(20 * time.Millisecond)
		if err := bc.AppendWrite(ctx, "file-stall", head, 0); err != nil {
			t.Fatalf("AppendWrite head refresh[%d]: %v", i, err)
		}
	}

	// Wait long enough for the later regions to stabilize and roll
	// up. The head region keeps getting refreshed during this loop
	// (via the refresh writes above already issued); after the refresh
	// loop completes, wait one stabilization window for the later
	// regions.
	time.Sleep(200 * time.Millisecond)

	if n := countChunksInBlocks(t, bc.baseDir); n < 3 {
		t.Fatalf("expected ≥3 chunks for later regions while head stalls, got %d", n)
	}

	// Now stop refreshing head and let it stabilize too. Then the
	// full file should roll up and rollup_offset advance to (or past)
	// the end of all records we appended.
	time.Sleep(300 * time.Millisecond)
	off, err := rs.GetRollupOffset(ctx, "file-stall")
	if err != nil {
		t.Fatalf("GetRollupOffset: %v", err)
	}
	if off <= uint64(logHeaderSize) {
		t.Fatalf("rollup_offset did not advance after head stabilization: got %d", off)
	}
}

// TestRollup_R7_OverwriteReclaimsHeadBytes is the R-7 (#580) acceptance
// scenario: a head-of-log record whose file region is FULLY OVERWRITTEN
// by a later record must have its on-disk frame bytes reclaimed as soon
// as the overwrite chunks — even if the head record's interval is still
// held in the dirty interval tree (e.g. because it was touched again
// later and its own stabilization window keeps slipping).
//
// Pre-R-7 (#566's design): consumption was keyed by logPos. The head
// record's logPos pinned the fence at the log header even when every
// byte of its file region had been chunked by a subsequent overwrite,
// because the head was not itself "in the consumed set" yet.
//
// Post-R-7: consumption is keyed by FILE-OFFSET interval. The overwrite's
// MarkConsumed adds [headOff, headOff+len) to coverageSet, which is the
// same extent the head record covers, so AdvanceFence walks past both
// frames once the overwrite is chunked. rollup_offset advances even
// though the head's dirty interval is still considered unstable.
//
// We exercise this end-to-end by:
//  1. Writing a head record at file_off=0.
//  2. Writing an overwrite at the SAME file_off=0 (same length).
//  3. Waiting several stabilization windows so the interval-tree entry
//     for [0, payloadLen) becomes stable and the rollup pool drains it.
//     R-7's guarantee is that both frames' bytes get reclaimed in a
//     single rollup pass — the head's frame becomes dead via the
//     overwrite's coverage, not via the head being individually picked
//     up — so the on-disk rollup_offset must advance past both frames.
//
// Acceptance: within a few stabilization windows the fence has advanced
// past BOTH frames (head + overwrite), not just past the overwrite.
func TestRollup_R7_OverwriteReclaimsHeadBytes(t *testing.T) {
	const stabilizationMS = 50
	bc, rs := newRollupFSStore(t, 1<<30, stabilizationMS)
	ctx := context.Background()

	const payloadLen = 16 * 1024
	head := bytes.Repeat([]byte{0xC1}, payloadLen)
	overwrite := bytes.Repeat([]byte{0xC2}, payloadLen)

	// 1. Head write.
	if err := bc.AppendWrite(ctx, "file-r7", head, 0); err != nil {
		t.Fatalf("AppendWrite head: %v", err)
	}
	// 2. Overwrite at the same file region.
	if err := bc.AppendWrite(ctx, "file-r7", overwrite, 0); err != nil {
		t.Fatalf("AppendWrite overwrite: %v", err)
	}

	// 3. Wait several stabilization windows so both intervals stabilize
	// together and the rollup pool fires. R-7's guarantee is that both
	// frames' bytes get reclaimed in a single rollup pass — the head's
	// frame becomes dead via the overwrite's coverage, not via the head
	// being individually picked up.
	deadline := time.Now().Add(5 * time.Second)
	wantFence := uint64(logHeaderSize) + 2*(uint64(recordFrameOverhead)+uint64(payloadLen))
	var lastOff uint64
	for time.Now().Before(deadline) {
		off, err := rs.GetRollupOffset(ctx, "file-r7")
		if err != nil {
			t.Fatalf("GetRollupOffset: %v", err)
		}
		lastOff = off
		if off >= wantFence {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if lastOff < wantFence {
		t.Fatalf("R-7 stalled-fence reclamation: rollup_offset=%d want >= %d", lastOff, wantFence)
	}

	// Belt-and-braces: logBytesTotal must drop to zero — every frame's
	// bytes accounted in budget release.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bc.logBytesTotal.Load() == 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if got := bc.logBytesTotal.Load(); got != 0 {
		t.Fatalf("logBytesTotal nonzero after overwrite reclamation: %d", got)
	}
}

// TestRollup_R7_LogIndexLevel_StalledFence is a focused unit-level
// acceptance for R-7. It bypasses the rollup ticker and exercises the
// stalled-fence scenario directly against the logIndex: head + overwrite
// at the same file extent, the head is NEVER marked consumed, the
// overwrite IS marked consumed → AdvanceFence walks past both frames.
//
// This is the "synthetic stalled-fence scenario" from the issue
// acceptance criterion. Sibling to the end-to-end test above; this one
// is deterministic and fast.
func TestRollup_R7_LogIndexLevel_StalledFence(t *testing.T) {
	idx := newLogIndex()
	const payload = uint32(4096)
	step := uint64(recordFrameOverhead) + uint64(payload)
	headLogPos := uint64(logHeaderSize)
	overLogPos := headLogPos + step
	tailLogPos := overLogPos + step

	// Head at [0, 4K), overwrite at [0, 4K), tail at [4K, 8K).
	idx.Append(headLogPos, 0, payload)
	idx.Append(overLogPos, 0, payload)
	idx.Append(tailLogPos, 4096, payload)

	// Pre-fix sanity: with zero consumption, fence stays at the header.
	if got := idx.AdvanceFence(); got != uint64(logHeaderSize) {
		t.Fatalf("pre-consume fence: got %d want %d", got, logHeaderSize)
	}

	// Mark the overwrite and the tail consumed; explicitly do NOT mark
	// the head. Under the old logPos-keyed scheme, the head's logPos sat
	// in the consumed map's complement and pinned the fence. Under R-7,
	// the overwrite's coverage [0, 4K) subsumes the head's extent —
	// the head's frame is dead and the fence walks straight through.
	idx.MarkConsumed(0, payload)    // covers head AND overwrite
	idx.MarkConsumed(4096, payload) // covers tail

	wantFence := tailLogPos + step
	if got := idx.AdvanceFence(); got != wantFence {
		t.Fatalf("R-7 stalled-fence regression: fence got %d want %d (head should have been released via overwrite coverage)",
			got, wantFence)
	}
}
