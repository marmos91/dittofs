package runtime

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

// teardownRuntime releases a fixture's runtime in one line: drain the snapshot
// orchestration, stop the adapters, quiesce the per-share data plane, close the
// metadata stores. It is for tests that are finished with a runtime, not tests
// that are about shutting one down.
//
// It is deliberately NOT the sequence the server runs. That one belongs to
// lifecycle.Service and is reached through Serve — see serveUntilShutdown. A
// composition assembled here could stay correct while the server's own order
// was wrong, so anything asserting about shutdown drives Serve instead.
func teardownRuntime(ctx context.Context, rt *Runtime) {
	rt.StopBackgroundWorkers()
	rt.ShutdownSnapshots(ctx)
	_ = rt.StopAllAdapters()
	rt.sharesSvc.CloseBlockStores(ctx)
	rt.CloseMetadataStores()
}

// serveUntilShutdown runs the server's own lifecycle and then cancels it, so
// the shutdown sequence under test is the one production takes — no step of it
// is supplied by the test.
//
// The context must stay live until startup completes. Serve returns a startup
// error rather than running the shutdown sequence if the cancellation lands
// while a startup step is still using that context, and the assertions below
// then report a shutdown that never ran instead of the startup that failed —
// so the barrier is StartupDone, which closes only once every step that can
// fail is past. Serve's own return is watched alongside it: a failed startup
// leaves StartupDone open, and the error names the cause.
func serveUntilShutdown(t *testing.T, rt *Runtime) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- rt.Serve(ctx) }()

	select {
	case <-rt.StartupDone():
	case err := <-done:
		t.Fatalf("Serve returned before finishing startup: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("Serve did not finish startup")
	}
	cancel()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
}

// goroutineDump returns every goroutine's stack. The buffer grows until the
// dump fits: runtime.Stack truncates rather than reporting how much it needed,
// and a truncated dump silently drops the frames the assertions look for.
func goroutineDump() string {
	for size := 1 << 20; ; size *= 2 {
		buf := make([]byte, size)
		if n := runtime.Stack(buf, true); n < size {
			return string(buf[:n])
		}
	}
}

// countGoroutines counts the live goroutines whose stack names frame. Naming a
// frame rather than counting every goroutine keeps the answer about the loop
// under test and not about whatever else the process happens to be running.
func countGoroutines(frame string) int {
	return strings.Count(goroutineDump(), frame)
}
