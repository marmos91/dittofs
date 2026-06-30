package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/engine"
)

// TestGCRegistry_SingleActiveJob asserts that while a run is in flight a second
// start returns the SAME job rather than launching a concurrent one (GC
// serializes server-wide), and that progress callbacks update the job.
func TestGCRegistry_SingleActiveJob(t *testing.T) {
	reg := newGCRegistry()

	release := make(chan struct{})
	started := make(chan struct{})
	run := func(ctx context.Context, progress func(engine.GCStats)) (*engine.GCStats, error) {
		close(started)
		progress(engine.GCStats{HashesMarked: 5})
		<-release // block so the job stays "running" for the second start
		return &engine.GCStats{HashesMarked: 5, ObjectsSwept: 2, BytesFreed: 1024}, nil
	}

	first := reg.start("/s", false, false, run)
	<-started
	if first.State != GCStateRunning {
		t.Fatalf("first job state = %q, want running", first.State)
	}

	// Second start while the first is in flight must return the same job and
	// must NOT invoke run again.
	second := reg.start("/s", false, false, func(context.Context, func(engine.GCStats)) (*engine.GCStats, error) {
		t.Fatal("second start must not launch a concurrent run")
		return nil, nil
	})
	if second.ID != first.ID {
		t.Fatalf("second start returned a different job: %q vs %q", second.ID, first.ID)
	}

	close(release)

	// Wait for completion by polling the registry.
	waitFor(t, func() bool {
		j, ok := reg.get(first.ID)
		return ok && j.State == GCStateDone
	})

	done, _ := reg.get(first.ID)
	if done.ObjectsSwept != 2 || done.BytesFreed != 1024 || done.Stats == nil {
		t.Fatalf("final job not populated from stats: %+v", done)
	}
}

// TestGCRegistry_FailedJob records the error and frees the active slot for a
// subsequent run.
func TestGCRegistry_FailedJob(t *testing.T) {
	reg := newGCRegistry()
	job := reg.start("/s", false, false, func(context.Context, func(engine.GCStats)) (*engine.GCStats, error) {
		return nil, context.DeadlineExceeded
	})
	waitFor(t, func() bool {
		j, ok := reg.get(job.ID)
		return ok && j.State == GCStateFailed
	})
	j, _ := reg.get(job.ID)
	if j.Err == "" {
		t.Fatal("failed job must record an error")
	}

	// Active slot is freed: a new run launches with a fresh id.
	next := reg.start("/s", false, false, func(context.Context, func(engine.GCStats)) (*engine.GCStats, error) {
		return &engine.GCStats{}, nil
	})
	if next.ID == job.ID {
		t.Fatal("a new run after completion must get a fresh job id")
	}
}

// TestGCRegistry_RetireBound caps retained terminal jobs at maxRetainedGCJobs.
func TestGCRegistry_RetireBound(t *testing.T) {
	reg := newGCRegistry()
	var ids []string
	for i := 0; i < maxRetainedGCJobs+5; i++ {
		job := reg.start("/s", false, false, func(context.Context, func(engine.GCStats)) (*engine.GCStats, error) {
			return &engine.GCStats{}, nil
		})
		ids = append(ids, job.ID)
		waitFor(t, func() bool {
			j, ok := reg.get(job.ID)
			return ok && j.State == GCStateDone
		})
	}
	reg.mu.Lock()
	retained := len(reg.jobs)
	reg.mu.Unlock()
	if retained > maxRetainedGCJobs {
		t.Fatalf("retained %d terminal jobs, want <= %d", retained, maxRetainedGCJobs)
	}
	// The oldest jobs must have been evicted.
	if _, ok := reg.get(ids[0]); ok {
		t.Fatal("oldest terminal job should have been evicted")
	}
}

// waitFor polls cond until it holds or a short deadline elapses, failing the
// test on timeout. Polling (rather than a fixed sleep) keeps the test fast.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition never became true")
}
