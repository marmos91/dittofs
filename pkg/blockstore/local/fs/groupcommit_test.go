package fs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestGroupCommit_SingleWriter_FiresImmediately exercises adaptive
// bypass: when the pending queue is empty and no timer is armed, Sync
// must call fsyncFn inline without waiting for the 1ms window. The
// budget is 500µs — well under the 1ms window — to defend against
// regressions where the bypass is dropped.
func TestGroupCommit_SingleWriter_FiresImmediately(t *testing.T) {
	var calls atomic.Int32
	gc := newGroupCommit(func() error {
		calls.Add(1)
		return nil
	})
	start := time.Now()
	if err := gc.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 500*time.Microsecond {
		t.Fatalf("depth-1 bypass too slow: %v (want < 500µs)", elapsed)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fsync calls: got %d, want 1", got)
	}
}

// TestGroupCommit_TwoConcurrentWriters_ShareOneFsync verifies that
// writers landing within the same window batch into a single fsync.
// Writer-A holds the fsync mid-flight (slow fsyncFn) so writer-B
// arrives while a batch is open; together they must share one fsync.
func TestGroupCommit_TwoConcurrentWriters_ShareOneFsync(t *testing.T) {
	var calls atomic.Int32
	released := make(chan struct{})
	gc := newGroupCommit(func() error {
		calls.Add(1)
		<-released // hold fsync long enough for B to join the batch
		return nil
	})

	errCh := make(chan error, 2)
	go func() { errCh <- gc.Sync(context.Background()) }()
	// Give A enough time to arm the timer / start fsync.
	time.Sleep(2 * time.Millisecond)
	go func() { errCh <- gc.Sync(context.Background()) }()
	// Give B time to enqueue.
	time.Sleep(2 * time.Millisecond)
	close(released)

	for i := 0; i < 2; i++ {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("Sync %d returned error: %v", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("Sync %d did not return", i)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fsync calls: got %d, want 1 (batch)", got)
	}
}

// TestGroupCommit_TimerFiresAt1ms_BatchesAllArrivals feeds five
// concurrent Sync calls within the 1ms window and asserts the coordinator
// collapses them into a single fsync.
func TestGroupCommit_TimerFiresAt1ms_BatchesAllArrivals(t *testing.T) {
	var calls atomic.Int32
	// Hold the fsync for ~3ms so all writers arrive while a batch is open.
	gc := newGroupCommit(func() error {
		calls.Add(1)
		time.Sleep(3 * time.Millisecond)
		return nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := gc.Sync(context.Background()); err != nil {
				t.Errorf("Sync: %v", err)
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("writers did not return in time")
	}

	if got := calls.Load(); got > 2 {
		// At most 2: one for the initial inline bypass (if A raced past
		// the empty-queue check) plus one for the batched window. Anything
		// above 2 means batching is broken.
		t.Fatalf("fsync calls: got %d, want <= 2 (5 writers should batch)", got)
	}
}

// TestGroupCommit_FsyncErrorPropagatesToAllWaiters confirms that a
// failed fsync delivers the same error to every batched waiter.
func TestGroupCommit_FsyncErrorPropagatesToAllWaiters(t *testing.T) {
	wantErr := errors.New("disk full")
	released := make(chan struct{})
	gc := newGroupCommit(func() error {
		<-released
		return wantErr
	})

	errCh := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() { errCh <- gc.Sync(context.Background()) }()
		// Stagger slightly so A starts, B and C enqueue.
		time.Sleep(500 * time.Microsecond)
	}
	close(released)

	for i := 0; i < 3; i++ {
		select {
		case err := <-errCh:
			if !errors.Is(err, wantErr) {
				t.Fatalf("Sync %d: got %v, want %v", i, err, wantErr)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("Sync %d did not return", i)
		}
	}
}

// TestGroupCommit_CtxCancel_MidWait_ReturnsCtxErr_ButFsyncStillRuns
// verifies: a canceled context returns ctx.Err() to the caller, but
// the in-flight fsync still completes for any other batched waiters.
func TestGroupCommit_CtxCancel_MidWait_ReturnsCtxErr_ButFsyncStillRuns(t *testing.T) {
	var calls atomic.Int32
	released := make(chan struct{})
	gc := newGroupCommit(func() error {
		calls.Add(1)
		<-released
		return nil
	})

	aErrCh := make(chan error, 1)
	go func() { aErrCh <- gc.Sync(context.Background()) }()
	// Let A arm the timer / start fsync.
	time.Sleep(2 * time.Millisecond)

	ctxB, cancelB := context.WithCancel(context.Background())
	bErrCh := make(chan error, 1)
	go func() { bErrCh <- gc.Sync(ctxB) }()
	// Let B enqueue.
	time.Sleep(2 * time.Millisecond)

	cancelB()
	select {
	case err := <-bErrCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("B: got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("B did not return after cancel")
	}

	// fsync MUST still complete for A.
	close(released)
	select {
	case err := <-aErrCh:
		if err != nil {
			t.Fatalf("A: got %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("A did not return after fsync release")
	}
	if got := calls.Load(); got < 1 {
		t.Fatalf("fsync should have run despite B's cancel; calls=%d", got)
	}
}

// TestGroupCommit_NoLogsMuTouch enforces the lock-order
// invariant by grepping the source file: the coordinator must never
// reference bc.logsMu.
func TestGroupCommit_NoLogsMuTouch(t *testing.T) {
	body, err := os.ReadFile("groupcommit.go")
	if err != nil {
		t.Fatalf("read groupcommit.go: %v", err)
	}
	if bytes.Contains(body, []byte("logsMu")) {
		t.Fatalf("groupcommit.go must not reference logsMu (D-09 lock-order invariant)")
	}
}

// TestGroupCommit_RaceFree spins 16 goroutines hammering Sync for 50ms
// and verifies (a) the race detector stays clean and (b) batching is
// observably reducing the fsync count.
func TestGroupCommit_RaceFree(t *testing.T) {
	var calls atomic.Int32
	var syncs atomic.Int32
	gc := newGroupCommit(func() error {
		calls.Add(1)
		// Tiny sleep ensures batching has a chance to coalesce arrivals.
		time.Sleep(100 * time.Microsecond)
		return nil
	})

	stop := make(chan struct{})
	time.AfterFunc(50*time.Millisecond, func() { close(stop) })

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := gc.Sync(context.Background()); err == nil {
					syncs.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	c := calls.Load()
	s := syncs.Load()
	if s == 0 || c == 0 {
		t.Fatalf("no progress: syncs=%d calls=%d", s, c)
	}
	if c >= s {
		t.Fatalf("batching not observed: fsync calls (%d) >= Sync calls (%d)", c, s)
	}
}
