package fs

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestRollupFile_ShutdownCancellationIsBenign reproduces issue #1245 Bug C:
// when the rollup context is cancelled DURING a shutdown-time pass (in-flight
// StoreChunk / ObjectIDPersister see ctx.Err()), rollupFile must NOT propagate
// context.Canceled as a fatal error. Such an interruption is benign — CAS
// chunks are content-addressed and rollup_offset only advances after the
// manifest lands, so the next pass on a fresh context resumes safely.
//
// Before the fix, rollupFile returned context.Canceled, which the drain/stop
// path bubbled up to os.Exit (status=1/FAILURE). After the fix it returns nil
// (logged at info/debug) and a subsequent fresh-context pass completes the
// rollup.
func TestRollupFile_ShutdownCancellationIsBenign(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()

	var mu sync.Mutex
	var persisted []block.BlockRef
	persister := func(ctx context.Context, _ string, blocks []block.BlockRef, _ block.ObjectID) error {
		// Honour ctx cancellation the way the real engine persister does.
		if err := ctx.Err(); err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		persisted = append(persisted, blocks...)
		return nil
	}

	bc := newFSStoreForTest(t, FSStoreOptions{
		MaxLogBytes:       1 << 30,
		RollupWorkers:     2,
		StabilizationMS:   1, // tiny so force isn't required for the fresh pass
		RollupStore:       rs,
		ObjectIDPersister: persister,
	})
	// Intentionally do NOT StartRollup — we drive rollupFile directly.

	payload := bytes.Repeat([]byte{0x5A}, 8*1024*1024)
	if err := bc.AppendWrite(context.Background(), "file1", payload, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	// Pass 1: cancelled context, simulating a rollup pass interrupted by
	// shutdown. This MUST be treated as benign (return nil), NOT a fatal
	// context.Canceled.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := bc.rollupFile(cancelledCtx, "file1", true); err != nil {
		t.Fatalf("rollupFile with cancelled ctx returned fatal error %v; want nil (shutdown cancellation must be benign)", err)
	}

	// The rollup must NOT have advanced rollup_offset on the cancelled pass
	// (nothing was committed).
	off, err := rs.GetRollupOffset(context.Background(), "file1")
	if err != nil {
		t.Fatalf("GetRollupOffset: %v", err)
	}
	if off > uint64(logHeaderSize) {
		t.Fatalf("rollup_offset advanced on a cancelled pass: got %d (want <= header)", off)
	}

	// Pass 2: fresh context — the rollup must resume and complete. This is
	// the "resume on restart" property.
	if err := bc.rollupFile(context.Background(), "file1", true); err != nil {
		t.Fatalf("rollupFile resume pass: %v", err)
	}
	mu.Lock()
	post := len(persisted)
	mu.Unlock()
	if post == 0 {
		t.Fatal("resume pass did not complete the rollup: persister never fired")
	}
	off, err = rs.GetRollupOffset(context.Background(), "file1")
	if err != nil {
		t.Fatalf("GetRollupOffset (post-resume): %v", err)
	}
	if off <= uint64(logHeaderSize) {
		t.Fatalf("rollup_offset did not advance after resume pass: got %d", off)
	}
}

// TestDrainRollups_ShutdownCancellationIsBenign verifies that a drain pass
// whose context is cancelled mid-flight returns the benign ctx error (the
// drain aborts cleanly) rather than wrapping it as a fatal
// "DrainRollups: rollup payload ..." error that the shutdown path treats as a
// hard failure. A cancelled drain is a benign abort: the rollup resumes on a
// later (fresh-context) pass.
func TestDrainRollups_ShutdownCancellationIsBenign(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()

	// Persister gate: signal when the rollup pass has entered Phase B (so the
	// cancellation lands MID-FLIGHT, not before the drain starts), then block
	// until the test cancels the context and releases us. Returning ctx.Err()
	// models a real in-progress persist seeing the cancelled context.
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once
	persister := func(ctx context.Context, _ string, _ []block.BlockRef, _ block.ObjectID) error {
		enterOnce.Do(func() { close(entered) })
		<-release
		return ctx.Err()
	}

	bc := newFSStoreForTest(t, FSStoreOptions{
		MaxLogBytes:       1 << 30,
		RollupWorkers:     2,
		StabilizationMS:   3_600_000,
		RollupStore:       rs,
		ObjectIDPersister: persister,
	})

	payload := bytes.Repeat([]byte{0x5A}, 8*1024*1024)
	if err := bc.AppendWrite(context.Background(), "file1", payload, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- bc.DrainRollups(ctx) }()

	// Wait until the persist is actually in flight, THEN cancel — this exercises
	// cancellation during an in-progress rollup, not just the early ctx.Err()
	// guard at the top of DrainRollups.
	<-entered
	cancel()
	close(release)

	var err error
	select {
	case err = <-errCh:
	case <-time.After(10 * time.Second):
		t.Fatal("DrainRollups did not return after mid-flight cancellation")
	}
	// DrainRollups returns ctx.Err() directly on cancellation (benign abort),
	// never a wrapped "DrainRollups: rollup payload" fatal error.
	if err != nil && err != context.Canceled {
		t.Fatalf("DrainRollups(cancelled mid-flight) = %v; want nil or context.Canceled (benign), not a wrapped fatal error", err)
	}
}

// TestGracefulStopRollup_DrainsInFlight is the graceful-stop test: with a
// slow/blocked rollup in flight (the worker pool running on a context that is
// then cancelled), GracefulStopRollup must stop accepting new rollups and
// drain the in-flight + remaining dirty payloads to completion using a
// SEPARATE, non-cancelled context within the grace window, returning nil.
//
// After GracefulStopRollup the dirty intervals are drained and the manifest is
// populated — proving the in-flight work was completed on a fresh context
// rather than abandoned with context.Canceled.
func TestGracefulStopRollup_DrainsInFlight(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()

	var mu sync.Mutex
	var persistedBlocks int
	persister := func(ctx context.Context, _ string, blocks []block.BlockRef, _ block.ObjectID) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		persistedBlocks += len(blocks)
		return nil
	}

	// Large stabilization window so the steady-state worker/ticker never
	// consumes the interval on its own — only the graceful-stop FORCED drain
	// can flush it. This isolates the drain behaviour.
	bc := newFSStoreForTest(t, FSStoreOptions{
		MaxLogBytes:       1 << 30,
		RollupWorkers:     2,
		StabilizationMS:   3_600_000,
		RollupStore:       rs,
		ObjectIDPersister: persister,
	})

	// Start the worker pool on a context we then cancel, modelling the
	// shutdown signal cancelling the long-lived runtime ctx.
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	if err := bc.StartRollup(workerCtx); err != nil {
		t.Fatalf("StartRollup: %v", err)
	}

	payload := bytes.Repeat([]byte{0x42}, 8*1024*1024)
	if err := bc.AppendWrite(context.Background(), "file1", payload, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	// Cancel the worker context (shutdown signal). In-flight/steady-state
	// passes on workerCtx would now see context.Canceled.
	cancelWorkers()

	// Graceful stop must drain the remaining dirty payload on a fresh,
	// non-cancelled context within the grace window and return nil.
	if err := bc.GracefulStopRollup(5 * time.Second); err != nil {
		t.Fatalf("GracefulStopRollup: %v", err)
	}

	mu.Lock()
	post := persistedBlocks
	mu.Unlock()
	if post == 0 {
		t.Fatal("GracefulStopRollup did not drain the in-flight rollup: persister never fired")
	}

	if n := bc.IntervalsLenForTest("file1"); n != 0 {
		t.Fatalf("dirty intervals remain after graceful stop: %d", n)
	}

	off, err := rs.GetRollupOffset(context.Background(), "file1")
	if err != nil {
		t.Fatalf("GetRollupOffset: %v", err)
	}
	if off <= uint64(logHeaderSize) {
		t.Fatalf("rollup_offset did not advance after graceful stop: got %d", off)
	}
}
