package fs

import (
	"bytes"
	"context"
	"testing"
)

// TestForceRollupForTest_FlushesUnstableInterval is the Windows-flake
// regression (a rollup test intermittently failed "file chunk not found" on
// CI). Root cause: ForceRollupForTest ran the pass with force=false, so it was
// gated by the stabilization window — a just-written interval is "not stable at
// this instant", the pass was a silent no-op that returned nil, and the
// following read found no chunk. On a coarse clock (Windows) or a loaded runner
// the race surfaced; on fast hardware the background pool usually hid it.
//
// The helper must flush deterministically regardless of how recently the data
// was written. A stabilization window far larger than the test's lifetime makes
// force=false provably a no-op, so this fails with the old behavior and passes
// once ForceRollupForTest bypasses the gate (force=true).
func TestForceRollupForTest_FlushesUnstableInterval(t *testing.T) {
	// 1h stabilization: the background pool cannot roll this up during the
	// test, so ForceRollupForTest is the only thing that can — isolating the
	// helper's determinism from the async pool.
	bc, rs := newRollupFSStore(t, 1<<30, 3_600_000)
	ctx := context.Background()
	const pid = "file-force-flush"

	payload := bytes.Repeat([]byte{0xC7}, 64*1024)
	if err := bc.AppendWrite(ctx, pid, payload, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}
	if err := bc.ForceRollupForTest(ctx, pid); err != nil {
		t.Fatalf("ForceRollupForTest: %v", err)
	}

	// The write must have reached CAS: at least one chunk exists and
	// rollup_offset advanced past the header.
	if n := countLocalChunks(t, bc); n < 1 {
		t.Fatalf("ForceRollupForTest did not flush the interval: 0 chunks (force=false silent no-op regression)")
	}
	off, err := rs.GetRollupOffset(ctx, pid)
	if err != nil {
		t.Fatalf("GetRollupOffset: %v", err)
	}
	if off <= uint64(logHeaderSize) {
		t.Fatalf("rollup_offset did not advance: got %d want > %d", off, logHeaderSize)
	}
}
