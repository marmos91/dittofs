package badger

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestCommitLeader_SingleCaller_FiresInline exercises the adaptive bypass: a
// lone caller at an idle leader runs the barrier inline (one drain pass) with no
// goroutine hand-off.
func TestCommitLeader_SingleCaller_FiresInline(t *testing.T) {
	var calls atomic.Int32
	l := newCommitLeader(func() error { calls.Add(1); return nil })

	if err := l.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("barrier calls: got %d, want 1", got)
	}
	if p := l.drainPasses.Load(); p != 1 {
		t.Fatalf("drainPasses: got %d, want 1 (inline bypass)", p)
	}
}

// TestCommitLeader_Concurrent_Coalesces is the core property: N concurrent
// callers collapse onto far fewer barrier invocations than submissions. Leader A
// holds the barrier open until the followers have enqueued, so they are drained
// together on the leader's next pass — one shared barrier for the whole batch.
func TestCommitLeader_Concurrent_Coalesces(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	l := newCommitLeader(func() error {
		n := calls.Add(1)
		if n == 1 {
			// First (leader A) barrier blocks so followers pile up behind it.
			close(started)
			<-release
		}
		return nil
	})

	aDone := make(chan error, 1)
	go func() { aDone <- l.Sync(context.Background()) }()
	<-started

	const followers = 8
	fDone := make(chan error, followers)
	for i := 0; i < followers; i++ {
		go func() { fDone <- l.Sync(context.Background()) }()
	}
	// Give the followers time to enqueue while A is blocked in its barrier.
	time.Sleep(20 * time.Millisecond)
	close(release)

	for i := 0; i < followers+1; i++ {
		ch := fDone
		if i == 0 {
			ch = aDone
		}
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("Sync returned error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a Sync did not return")
		}
	}

	// 9 submissions must collapse onto <= 2 barrier calls (A's blocked one plus
	// one shared drain of all followers) — the coalescing win.
	if got := calls.Load(); got > 2 {
		t.Fatalf("barrier calls: got %d, want <= 2 (followers must coalesce onto one barrier)", got)
	}
	if p := l.drainPasses.Load(); p > 2 {
		t.Fatalf("drainPasses: got %d, want <= 2", p)
	}
}

// TestCommitLeader_SharedResult_Synchronous pins two contract points: (1) every
// waiter in a coalesced batch sees the SAME barrier result (unlike syncLeader's
// per-fd isolation — one metadata barrier makes all pending writes durable), and
// (2) Sync is synchronous: a barrier error surfaces to the caller (no async ack)
// and only after the barrier has run.
func TestCommitLeader_SharedResult_Synchronous(t *testing.T) {
	wantErr := errors.New("fsync on fire")
	var ran atomic.Bool
	started := make(chan struct{})
	release := make(chan struct{})
	l := newCommitLeader(func() error {
		// Signal + block only on the very first (leader) call so followers pile
		// up and share this batch's barrier.
		if ran.CompareAndSwap(false, true) {
			close(started)
			<-release
		}
		return wantErr
	})

	aDone := make(chan error, 1)
	go func() { aDone <- l.Sync(context.Background()) }()
	<-started

	const followers = 4
	fDone := make(chan error, followers)
	for i := 0; i < followers; i++ {
		go func() { fDone <- l.Sync(context.Background()) }()
	}
	time.Sleep(20 * time.Millisecond)

	// Barrier has not completed yet: no waiter may have returned (synchronous).
	select {
	case err := <-aDone:
		t.Fatalf("Sync returned %v before barrier completed (must be synchronous)", err)
	default:
	}

	close(release)
	if err := <-aDone; !errors.Is(err, wantErr) {
		t.Fatalf("leader: got %v, want %v", err, wantErr)
	}
	for i := 0; i < followers; i++ {
		if err := <-fDone; !errors.Is(err, wantErr) {
			t.Fatalf("follower %d: got %v, want %v (shared barrier result)", i, err, wantErr)
		}
	}
}

// TestCommitLeader_CtxCancel_ReturnsCtxErr_ButBarrierStillRuns verifies a
// canceled follower observes ctx.Err() but the barrier still runs (durability
// trumps caller-side latency relief).
func TestCommitLeader_CtxCancel_ReturnsCtxErr_ButBarrierStillRuns(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	l := newCommitLeader(func() error {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return nil
	})

	aDone := make(chan error, 1)
	go func() { aDone <- l.Sync(context.Background()) }()
	<-started

	ctxB, cancelB := context.WithCancel(context.Background())
	bDone := make(chan error, 1)
	go func() { bDone <- l.Sync(ctxB) }()
	time.Sleep(20 * time.Millisecond)
	cancelB()

	select {
	case err := <-bDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("B: got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("B did not return after cancel")
	}

	close(release)
	if err := <-aDone; err != nil {
		t.Fatalf("A: %v", err)
	}
	// The barrier covering B must still have run (>= 2 calls: A's + B's batch).
	deadline := time.Now().Add(time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() < 2 {
		t.Fatal("barrier for the canceled follower did not run (durability broken)")
	}
}

// TestCommitLeader_RaceFree hammers the leader from many goroutines under -race
// and asserts progress plus coalescing (fewer barrier calls than submissions).
func TestCommitLeader_RaceFree(t *testing.T) {
	var calls atomic.Int64
	l := newCommitLeader(func() error {
		calls.Add(1)
		time.Sleep(50 * time.Microsecond)
		return nil
	})

	stop := make(chan struct{})
	time.AfterFunc(50*time.Millisecond, func() { close(stop) })

	var subs atomic.Int64
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
				subs.Add(1)
				_ = l.Sync(context.Background())
			}
		}()
	}
	wg.Wait()

	if calls.Load() == 0 || subs.Load() == 0 {
		t.Fatalf("no progress: calls=%d subs=%d", calls.Load(), subs.Load())
	}
	if l.drainPasses.Load() >= subs.Load() {
		t.Fatalf("coalescing not observed: passes=%d >= submissions=%d", l.drainPasses.Load(), subs.Load())
	}
}
