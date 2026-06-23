package shares

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// waitFor polls cond up to 2s; fails the test if it never becomes true.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

func TestWarmRegistry_DeterministicIDsAndSuccess(t *testing.T) {
	r := newWarmRegistry()

	run := func(ctx context.Context, progress func(done, total int64)) (warmAllResult, error) {
		progress(0, 2)
		progress(1, 2)
		progress(2, 2)
		return warmAllResult{BlocksFetched: 2, BytesFetched: 42}, nil
	}

	job := r.start("/share", run)
	if job.ID != "warm-/share-1" {
		t.Fatalf("job ID = %q, want warm-/share-1", job.ID)
	}
	if job.State != WarmStateRunning {
		t.Fatalf("initial state = %q, want running", job.State)
	}

	waitFor(t, func() bool {
		j, _ := r.get(job.ID)
		return j.State == WarmStateDone
	})

	j, ok := r.get(job.ID)
	if !ok {
		t.Fatal("job not found after completion")
	}
	if j.BlocksDone != 2 || j.BlocksTotal != 2 || j.BytesDone != 42 {
		t.Fatalf("final job = %+v", j)
	}
	if j.FinishedAt.IsZero() {
		t.Error("FinishedAt not set")
	}

	// A second start for the same share gets a fresh, monotonic ID.
	job2 := r.start("/share", func(_ context.Context, _ func(done, total int64)) (warmAllResult, error) {
		return warmAllResult{}, nil
	})
	if job2.ID != "warm-/share-2" {
		t.Fatalf("second job ID = %q, want warm-/share-2", job2.ID)
	}
}

func TestWarmRegistry_OneActivePerShare(t *testing.T) {
	r := newWarmRegistry()

	release := make(chan struct{})
	var calls int
	var mu sync.Mutex
	run := func(_ context.Context, _ func(done, total int64)) (warmAllResult, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
		return warmAllResult{}, nil
	}

	job1 := r.start("/share", run)
	job2 := r.start("/share", run) // should return the running job, not start a new one
	if job1.ID != job2.ID {
		t.Fatalf("second start got a different job: %q vs %q", job1.ID, job2.ID)
	}

	close(release)
	waitFor(t, func() bool {
		j, _ := r.get(job1.ID)
		return j.State == WarmStateDone
	})

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("run invoked %d times, want 1", calls)
	}
}

func TestWarmRegistry_CancelForShare(t *testing.T) {
	r := newWarmRegistry()

	started := make(chan struct{})
	run := func(ctx context.Context, _ func(done, total int64)) (warmAllResult, error) {
		close(started)
		<-ctx.Done()
		return warmAllResult{}, ctx.Err()
	}

	job := r.start("/share", run)
	<-started
	r.cancelForShare("/share")

	waitFor(t, func() bool {
		j, _ := r.get(job.ID)
		return j.State == WarmStateCanceled
	})

	j, _ := r.get(job.ID)
	if j.Err == "" {
		t.Fatalf("canceled job missing error: %+v", j)
	}
}

func TestWarmRegistry_Failure(t *testing.T) {
	r := newWarmRegistry()
	boom := errors.New("boom")
	job := r.start("/share", func(_ context.Context, _ func(done, total int64)) (warmAllResult, error) {
		return warmAllResult{}, boom
	})
	waitFor(t, func() bool {
		j, _ := r.get(job.ID)
		return j.State == WarmStateFailed
	})
	j, _ := r.get(job.ID)
	if j.Err != "boom" {
		t.Fatalf("failed job error = %q, want boom", j.Err)
	}
}
