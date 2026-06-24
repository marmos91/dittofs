package shares

import (
	"context"
	"errors"
	"strings"
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

	job := r.start("/share", 0, run)
	if job.ID != "warm-1" {
		t.Fatalf("job ID = %q, want warm-1", job.ID)
	}
	if strings.Contains(job.ID, "/") {
		t.Fatalf("job ID %q must not contain a slash (breaks poll route)", job.ID)
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
	job2 := r.start("/share", 0, func(_ context.Context, _ func(done, total int64)) (warmAllResult, error) {
		return warmAllResult{}, nil
	})
	if job2.ID != "warm-2" {
		t.Fatalf("second job ID = %q, want warm-2", job2.ID)
	}

	// The job is resolvable via the status lookup by id alone (the round-trip
	// that the 404 bug broke for slash-bearing shares).
	if _, ok := r.get(job2.ID); !ok {
		t.Fatalf("started job %q not resolvable by id", job2.ID)
	}
}

func TestWarmRegistry_SlashShareYieldsSlashFreeID(t *testing.T) {
	r := newWarmRegistry()
	job := r.start("/export", 0, func(_ context.Context, _ func(done, total int64)) (warmAllResult, error) {
		return warmAllResult{}, nil
	})
	if strings.Contains(job.ID, "/") {
		t.Fatalf("job ID %q for share /export must not contain a slash", job.ID)
	}
	if _, ok := r.get(job.ID); !ok {
		t.Fatalf("job %q not resolvable by id", job.ID)
	}
}

func TestWarmRegistry_WarnsOnEmptyEnumerationNonEmptyShare(t *testing.T) {
	r := newWarmRegistry()

	// Zero enumerable blocks (nothing fetched, nothing already-local) on a
	// share that reports nonzero stored bytes must set the Warning (#1374).
	job := r.start("/export", 4096, func(_ context.Context, _ func(done, total int64)) (warmAllResult, error) {
		return warmAllResult{}, nil
	})
	waitFor(t, func() bool {
		j, _ := r.get(job.ID)
		return j.State == WarmStateDone
	})
	j, _ := r.get(job.ID)
	if j.Warning == "" {
		t.Fatalf("expected warning on 0-block warm of non-empty share, got none: %+v", j)
	}
	if !strings.Contains(j.Warning, "local disk tier") {
		t.Fatalf("warning should reference the local disk tier, got %q", j.Warning)
	}

	// A share that genuinely has no stored bytes must NOT warn.
	jobEmpty := r.start("/empty", 0, func(_ context.Context, _ func(done, total int64)) (warmAllResult, error) {
		return warmAllResult{}, nil
	})
	waitFor(t, func() bool {
		j, _ := r.get(jobEmpty.ID)
		return j.State == WarmStateDone
	})
	je, _ := r.get(jobEmpty.ID)
	if je.Warning != "" {
		t.Fatalf("empty share should not warn, got %q", je.Warning)
	}

	// A run that DID enumerate blocks must not warn even if used bytes > 0.
	jobOK := r.start("/full", 4096, func(_ context.Context, _ func(done, total int64)) (warmAllResult, error) {
		return warmAllResult{BlocksAlreadyLocal: 3}, nil
	})
	waitFor(t, func() bool {
		j, _ := r.get(jobOK.ID)
		return j.State == WarmStateDone
	})
	jo, _ := r.get(jobOK.ID)
	if jo.Warning != "" {
		t.Fatalf("share with enumerable blocks should not warn, got %q", jo.Warning)
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

	job1 := r.start("/share", 0, run)
	job2 := r.start("/share", 0, run) // should return the running job, not start a new one
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

	job := r.start("/share", 0, run)
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
	job := r.start("/share", 0, func(_ context.Context, _ func(done, total int64)) (warmAllResult, error) {
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
