package fs

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestScanAllFiles_DispatchesConcurrently proves the ticker/backlog drain path
// rolls up independent payloads in PARALLEL (#1411). For many small files the
// per-file rollupCh nudges are spent once writing stops, so the backlog is
// drained solely by scanAllFiles. A serial dispatch there produces one chunk at
// a time, which pins the S3 uploader's pending snapshot — and thus upload
// inflight — to 1, no matter how wide the adaptive window is.
//
// The test latches the per-payload Phase-B hook as a barrier: every dispatched
// rollup announces itself then blocks. If scanAllFiles dispatches serially only
// ONE rollup ever reaches the barrier (the rest are never started because the
// single dispatch goroutine is parked inside the first rollup), so fewer than
// `workers` arrivals land within the timeout and the test fails.
func TestScanAllFiles_DispatchesConcurrently(t *testing.T) {
	const workers = 4
	bc := newFSStoreForTest(t, FSStoreOptions{
		MaxLogBytes:     1 << 30,
		RollupWorkers:   workers,
		StabilizationMS: 1,
	})
	// Deliberately do NOT StartRollup: this test drives scanAllFiles directly
	// so the only concurrency under observation is its own dispatch fan-out.
	ctx := context.Background()

	const n = workers // expect this many independent payloads to roll up at once
	pids := make([]string, n)
	for i := range pids {
		pids[i] = fmt.Sprintf("payload-%d", i)
		if err := bc.AppendWrite(ctx, pids[i], bytes.Repeat([]byte{byte(0xA0 + i)}, 4096), 0); err != nil {
			t.Fatalf("AppendWrite %s: %v", pids[i], err)
		}
	}
	// Wait until every payload is past its stabilization window (rollup-eligible).
	deadline := time.Now().Add(2 * time.Second)
	for _, pid := range pids {
		for !bc.EarliestStableForTest(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("payload %s never stabilized", pid)
			}
			time.Sleep(time.Millisecond)
		}
	}

	arrived := make(chan string, n)
	release := make(chan struct{})
	var once sync.Once
	rollupPhaseBHook = func(p string) {
		arrived <- p
		<-release
	}
	t.Cleanup(func() {
		rollupPhaseBHook = nil
		once.Do(func() { close(release) })
	})

	done := make(chan struct{})
	go func() { defer close(done); bc.scanAllFiles(ctx) }()

	timeout := time.After(3 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-arrived:
		case <-timeout:
			// Release the barrier and wait for scanAllFiles (and the rollups
			// it dispatched) to fully return BEFORE failing — otherwise the
			// t.Cleanup below would nil the package-global hook while those
			// goroutines still read it (a race under -race).
			once.Do(func() { close(release) })
			<-done
			t.Fatalf("only %d/%d rollups ran concurrently; scanAllFiles dispatch is serial", i, n)
		}
	}
	once.Do(func() { close(release) }) // let all barrier-held rollups complete
	<-done
}
