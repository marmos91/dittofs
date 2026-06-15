package engine

import (
	"context"
	"testing"
	"time"

	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// TestUnsyncedBytes_TracksDrain drives the real write path (AppendWrite →
// rollup → chunkstore → onChunkComplete) and asserts that the syncer's
// unsynced-byte counter — the backpressure signal the local store consults —
// rises as chunks land locally and returns to zero once SyncNow mirrors them
// all to the remote. This is the integration backstop for the fs-internal
// ensureSpace backpressure tests: it proves the counter the stall path reads
// is actually fed and drained by the production wiring.
//
// The periodic uploader is deliberately NOT started (no env.syncer.Start):
// it would drain chunks on its own 50ms tick and could empty the counter
// before this test samples it, making the "rises" assertion racy (it flaked
// on slower Windows runners). Draining here is driven synchronously via
// SyncNow so both transitions are deterministic.
func TestUnsyncedBytes_TracksDrain(t *testing.T) {
	env := newHealthTestEnv(t)
	ctx := context.Background()
	env.local.Start(ctx) // FileBlock metadata persistence; rollup pool already started by newHealthTestEnv.

	if got := env.syncer.UnsyncedBytes(); got != 0 {
		t.Fatalf("UnsyncedBytes at start = %d, want 0", got)
	}

	// Write enough bytes to produce at least one CAS chunk.
	payloadID := "export/unsynced-bytes-test.bin"
	data := make([]byte, 1<<20) // 1 MiB
	for i := range data {
		data[i] = byte(i % 251)
	}
	if err := env.local.AppendWrite(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	// Let the stabilization window (StabilizationMS=50) elapse so the dirty
	// interval is eligible for rollup, then force rollup synchronously until
	// no dirty intervals remain. Each rolled-up chunk fires onChunkComplete →
	// addPendingHash with its byte size.
	time.Sleep(80 * time.Millisecond)
	for i := 0; i < 32; i++ {
		if err := env.local.ForceRollupForTest(ctx, payloadID); err != nil {
			t.Fatalf("ForceRollupForTest: %v", err)
		}
		if env.local.IntervalsLenForTest(payloadID) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	env.local.SyncFileBlocks(ctx)

	// The counter must reflect the unsynced cache bytes now present locally.
	// With the periodic uploader stopped, nothing drains it behind our back.
	if got := env.syncer.UnsyncedBytes(); got <= 0 {
		t.Fatalf("UnsyncedBytes after rollup = %d, want > 0", got)
	}

	// Drain to remote synchronously: every chunk is mirrored + marked
	// synced, so the counter must return to zero.
	if err := env.syncer.SyncNow(ctx); err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	if got := env.syncer.UnsyncedBytes(); got != 0 {
		t.Fatalf("UnsyncedBytes after SyncNow drain = %d, want 0", got)
	}

	// Sanity: the bytes really did reach the remote.
	if mem, ok := env.remote.RemoteStore.(*remotememory.Store); ok {
		if mem.BlockCount() == 0 {
			t.Fatalf("remote has 0 blocks after drain; expected the mirrored chunks")
		}
	}
}
