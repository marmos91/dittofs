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
func TestUnsyncedBytes_TracksDrain(t *testing.T) {
	env := newHealthTestEnv(t)
	ctx := context.Background()
	env.local.Start(ctx)
	env.syncer.Start(ctx)

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
	if _, err := env.syncer.Flush(ctx, payloadID); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Drive rollup so the staged bytes land in the CAS chunk store, firing
	// onChunkComplete → addPendingHash with each chunk's byte size.
	time.Sleep(80 * time.Millisecond)
	for i := 0; i < 16; i++ {
		if err := env.local.ForceRollupForTest(ctx, payloadID); err != nil {
			t.Fatalf("ForceRollupForTest: %v", err)
		}
		if env.local.IntervalsLenForTest(payloadID) == 0 {
			break
		}
	}
	env.local.SyncFileBlocks(ctx)

	// The counter must reflect the unsynced cache bytes now present locally.
	waitFor(t, 5*time.Second, func() bool {
		return env.syncer.UnsyncedBytes() > 0
	}, "unsynced bytes to rise after rollup")

	// Drain to remote: every chunk is mirrored + marked synced, so the
	// counter must return to zero.
	if err := env.syncer.SyncNow(ctx); err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return env.syncer.UnsyncedBytes() == 0
	}, "unsynced bytes to drain to zero after SyncNow")

	// Sanity: the bytes really did reach the remote.
	if mem, ok := env.remote.RemoteStore.(*remotememory.Store); ok {
		if mem.BlockCount() == 0 {
			t.Fatalf("remote has 0 blocks after drain; expected the mirrored chunks")
		}
	}
}
