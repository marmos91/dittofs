package nfs

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/adapter"
)

// TestGoTracked_ShutdownWaitsForRunningTask pins the ownership contract the
// NLM/NSM background work depends on: the drain of blocked waiters and the
// startup SM_NOTIFY sweep mutate per-share lock managers, so shutdown must not
// return while one is still running and leave it writing to state the caller is
// about to release.
func TestGoTracked_ShutdownWaitsForRunningTask(t *testing.T) {
	t.Parallel()

	s := &NFSAdapter{BaseAdapter: &adapter.BaseAdapter{}}

	release := make(chan struct{})
	finished := make(chan struct{})
	s.goTracked(func() {
		<-release
		close(finished)
	})

	waited := make(chan struct{})
	go func() {
		s.waitForBackgroundTasks()
		close(waited)
	}()

	select {
	case <-waited:
		t.Fatal("shutdown returned while a tracked task was still running")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)

	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not return after the tracked task finished")
	}

	select {
	case <-finished:
	default:
		t.Fatal("shutdown returned before the task completed")
	}
}

// TestGoTracked_DropsTasksAfterShutdown covers the other half: a byte-range
// release arriving on a connection goroutine after shutdown has begun must not
// start new lock work behind the wait.
func TestGoTracked_DropsTasksAfterShutdown(t *testing.T) {
	t.Parallel()

	s := &NFSAdapter{BaseAdapter: &adapter.BaseAdapter{}}
	s.waitForBackgroundTasks()

	ran := make(chan struct{})
	s.goTracked(func() { close(ran) })

	select {
	case <-ran:
		t.Fatal("a tracked task started after shutdown")
	case <-time.After(50 * time.Millisecond):
	}

	// A second shutdown is a no-op rather than a panic or a hang.
	s.waitForBackgroundTasks()
}
