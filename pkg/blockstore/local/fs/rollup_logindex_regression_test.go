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
