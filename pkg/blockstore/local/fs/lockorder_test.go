package fs

import (
	"context"
	"sync"
	"testing"
	"time"

	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestAppendWrite_DeleteRace_NoDeadlock is a regression guard for FIX-2:
// the AppendWrite error-recovery path used to acquire bc.logsMu.Lock()
// while still holding the per-file mutex. Combined with any path that
// holds logsMu and then waits on the per-file mutex, the result was an
// AB/BA deadlock. The fix releases the per-file mutex BEFORE acquiring
// logsMu in the error path, so the global discipline "always mu before
// logsMu" is preserved.
//
// This test exercises the AppendWrite ↔ DeleteAppendLog hot path under a
// hard wall-clock deadline: if the lock order regresses, the test hangs
// past the deadline and the select-on-time.After ceiling at the bottom of
// this function trips with a clear message.
//
// FIX-27 cleanup: the previous "if d, ok := t.Deadline(); ..." block at
// the top was a no-op (registered an empty t.Cleanup) and only documented
// intent. The actual ceiling lives in the select { case <-time.After(...)
// } at the bottom — that is the genuine wall-clock guard.
func TestAppendWrite_DeleteRace_NoDeadlock(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreForTest(t, FSStoreOptions{
		UseAppendLog:    true,
		MaxLogBytes:     1 << 30,
		RollupWorkers:   2,
		StabilizationMS: 10,
		RollupStore:     rs,
	})
	ctx := context.Background()

	const goroutines = 8
	const itersPerGoroutine = 200

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Half the goroutines hammer AppendWrite on a small set of payload IDs.
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			payloadID := "race-payload"
			for i := 0; i < itersPerGoroutine; i++ {
				_ = bc.AppendWrite(ctx, payloadID, []byte{byte(i)}, uint64(i))
			}
		}(g)
	}

	// The other half hammer DeleteAppendLog on the same payload IDs.
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			payloadID := "race-payload"
			for i := 0; i < itersPerGoroutine; i++ {
				_ = bc.DeleteAppendLog(ctx, payloadID)
			}
		}(g)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	deadline := 10 * time.Second
	if dl, ok := t.Deadline(); ok {
		// Leave a 2 s buffer so we fail with a clear message rather than
		// truncating arbitrary post-test cleanup.
		if budget := time.Until(dl) - 2*time.Second; budget > 0 && budget < deadline {
			deadline = budget
		}
	}
	select {
	case <-done:
	case <-time.After(deadline):
		t.Fatalf("AppendWrite/DeleteAppendLog deadlocked within %s — FIX-2 lock-order regression", deadline)
	}
}
