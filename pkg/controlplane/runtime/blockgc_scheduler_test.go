package runtime

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/remote"
)

// TestStartScheduledGC_TicksAndStops verifies the auto-GC scheduler runs GC on
// its interval and exits cleanly when its context is cancelled.
func TestStartScheduledGC_TicksAndStops(t *testing.T) {
	var runs int32
	orig := collectGarbageFn
	collectGarbageFn = func(_ context.Context, _ remote.RemoteStore, _ engine.MetadataReconciler, _ *engine.Options) *engine.GCStats {
		atomic.AddInt32(&runs, 1)
		return &engine.GCStats{}
	}
	t.Cleanup(func() { collectGarbageFn = orig })

	rt := newRuntimeForGC(t, map[string]remote.RemoteStore{"/share-a": &fakeRemoteStore{name: "s3"}})

	ctx, cancel := context.WithCancel(context.Background())
	rt.StartScheduledGC(ctx, 10*time.Millisecond)

	// Wait for a few ticks.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&runs) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&runs); got < 2 {
		t.Fatalf("auto-GC ran %d times, want >= 2", got)
	}

	// Cancel and confirm the loop stops (run count stops climbing).
	cancel()
	time.Sleep(30 * time.Millisecond)
	stopped := atomic.LoadInt32(&runs)
	time.Sleep(60 * time.Millisecond)
	if after := atomic.LoadInt32(&runs); after != stopped {
		t.Errorf("auto-GC kept running after cancel: %d -> %d", stopped, after)
	}
}
