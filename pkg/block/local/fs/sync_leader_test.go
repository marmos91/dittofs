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

// TestSyncLeader_SingleWriter_FiresInline exercises the adaptive bypass: a lone
// caller at an idle leader runs its fsync inline (one drain pass) and returns
// its result, with no goroutine hand-off.
func TestSyncLeader_SingleWriter_FiresInline(t *testing.T) {
	l := newSyncLeader()
	var calls atomic.Int32
	if err := l.Sync(context.Background(), func() error { calls.Add(1); return nil }); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fsync calls: got %d, want 1", got)
	}
	if p := l.drainPasses.Load(); p != 1 {
		t.Fatalf("drainPasses: got %d, want 1 (inline bypass)", p)
	}
}

// TestSyncLeader_ConcurrentDistinctFds_EachSyncedOnce is the core PR3 property:
// N concurrent callers each holding a DISTINCT fd all fsync exactly once (each
// fd is durable) and coalesce onto a single leader run (far fewer drain passes
// than submissions). Leader A's fsync blocks until B/C/D have enqueued, so they
// are drained together on the leader's next pass.
func TestSyncLeader_ConcurrentDistinctFds_EachSyncedOnce(t *testing.T) {
	l := newSyncLeader()
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})

	// A becomes the leader; its fsync blocks so B/C/D pile up behind it.
	aDone := make(chan error, 1)
	go func() {
		aDone <- l.Sync(context.Background(), func() error {
			calls.Add(1)
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	const followers = 3
	fDone := make(chan error, followers)
	for i := 0; i < followers; i++ {
		go func() {
			fDone <- l.Sync(context.Background(), func() error { calls.Add(1); return nil })
		}()
	}
	// Give the followers time to enqueue while A is blocked in its fsync.
	time.Sleep(20 * time.Millisecond)
	close(release)

	for i := 0; i < followers+1; i++ {
		var ch chan error
		if i == 0 {
			ch = aDone
		} else {
			ch = fDone
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

	if got := calls.Load(); got != followers+1 {
		t.Fatalf("fsync calls: got %d, want %d (every fd synced once)", got, followers+1)
	}
	// Coalescing: 4 submissions collapsed onto <= 2 leader passes.
	if p := l.drainPasses.Load(); p > 2 {
		t.Fatalf("drainPasses: got %d, want <= 2 (followers should coalesce onto one leader run)", p)
	}
}

// TestSyncLeader_PerFdErrorIsolation asserts each caller sees ONLY its own fd's
// fsync result — unlike the old shared-fsync coordinator, a failure on one fd
// does not poison a co-batched caller holding a different fd.
func TestSyncLeader_PerFdErrorIsolation(t *testing.T) {
	l := newSyncLeader()
	wantErr := errors.New("fd A on fire")
	release := make(chan struct{})
	started := make(chan struct{})

	aDone := make(chan error, 1)
	go func() {
		aDone <- l.Sync(context.Background(), func() error {
			close(started)
			<-release
			return wantErr
		})
	}()
	<-started

	bDone := make(chan error, 1)
	go func() {
		bDone <- l.Sync(context.Background(), func() error { return nil })
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)

	if err := <-aDone; !errors.Is(err, wantErr) {
		t.Fatalf("A: got %v, want %v", err, wantErr)
	}
	if err := <-bDone; err != nil {
		t.Fatalf("B: got %v, want nil (A's error must not leak to B)", err)
	}
}

// TestSyncLeader_CtxCancel_MidWait_ReturnsCtxErr_ButFsyncStillRuns verifies a
// canceled follower observes ctx.Err() but the leader still runs its fsync
// (durability trumps caller-side latency relief).
func TestSyncLeader_CtxCancel_MidWait_ReturnsCtxErr_ButFsyncStillRuns(t *testing.T) {
	l := newSyncLeader()
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})

	// A holds the leader open.
	aDone := make(chan error, 1)
	go func() {
		aDone <- l.Sync(context.Background(), func() error {
			calls.Add(1)
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	// B enqueues behind A, then is canceled before A releases.
	ctxB, cancelB := context.WithCancel(context.Background())
	bDone := make(chan error, 1)
	var bRan atomic.Int32
	go func() {
		bDone <- l.Sync(ctxB, func() error { bRan.Add(1); return nil })
	}()
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
	// B's fsync must still have run despite the cancel (buffered chan absorbs it).
	deadline := time.Now().Add(time.Second)
	for bRan.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if bRan.Load() == 0 {
		t.Fatal("B's fsync did not run after cancel (durability broken)")
	}
}

// TestSyncLeader_RaceFree hammers the leader from many goroutines under -race
// and asserts progress plus coalescing (fewer passes than submissions).
func TestSyncLeader_RaceFree(t *testing.T) {
	l := newSyncLeader()
	var calls atomic.Int32
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
				_ = l.Sync(context.Background(), func() error {
					calls.Add(1)
					time.Sleep(50 * time.Microsecond)
					return nil
				})
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

// TestSyncLeader_NoShardLockTouch pins the lock-order invariant: the leader
// coordinator must NEVER reference a shard lock (the per-store append-log
// state maps). It is a source-grep gate mirroring the pre-PR3
// TestGroupCommit_NoLogsMuTouch.
func TestSyncLeader_NoShardLockTouch(t *testing.T) {
	body, err := os.ReadFile("sync_leader.go")
	if err != nil {
		t.Fatalf("read sync_leader.go: %v", err)
	}
	for _, banned := range []string{"logsMu", "logShards", "shardFor"} {
		if bytes.Contains(body, []byte(banned)) {
			t.Fatalf("sync_leader.go must not reference %q (lock-order invariant)", banned)
		}
	}
}
