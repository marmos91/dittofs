package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/engine"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
)

// blockingAPIServer fails on demand. Start holds until the test releases it and
// then reports an error, which is the shutdown the server runs when its API
// server dies rather than when it is signalled.
type blockingAPIServer struct {
	release chan struct{}
}

func (a *blockingAPIServer) Start(context.Context) error {
	<-a.release
	return errors.New("api server failed")
}

func (a *blockingAPIServer) Stop(context.Context) error { return nil }

func (a *blockingAPIServer) Port() int { return 0 }

// trashReapers counts the live recycle-bin reaper goroutines.
func trashReapers() int {
	return countGoroutines("trash.(*Service).Start.func")
}

// TestServerShutdownStopsBackgroundWorkers is the regression guard for the two
// background workers the server's shutdown sequence owns: the recycle-bin
// reaper and an async block GC run in flight. Both write through the metadata
// stores the same sequence closes, and neither is reached by stopping the
// adapters or closing the block stores.
//
// The shutdown it drives is the one an API-server failure triggers, not the one
// a signal triggers, because that is the path on which the runtime's context
// stays LIVE. A cancelled context reaches the reaper by itself, so the same
// assertion on the signal path would hold whether or not the shutdown sequence
// stopped anything — and the GC run never sees that context at all: it runs on
// a detached one and is reachable only by cancelActive.
func TestServerShutdownStopsBackgroundWorkers(t *testing.T) {
	cps, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	t.Cleanup(func() { _ = cps.Close() })

	rt := New(cps)
	rt.SetSnapshotSchedulerConfig(0, true)
	api := &blockingAPIServer{release: make(chan struct{})}
	rt.SetAPIServer(api)

	// A GC run parked on its own context: it reports cancellation rather than
	// finishing, so the assertion is about the run being reached and not about
	// the registry's bookkeeping around it.
	gcCancelled := make(chan struct{})
	rt.gcReg.start("/gc", false, false, func(ctx context.Context, _ func(engine.GCStats)) (*engine.GCStats, error) {
		<-ctx.Done()
		close(gcCancelled)
		return nil, ctx.Err()
	})

	base := trashReapers()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() { serveErr <- rt.Serve(ctx) }()

	select {
	case <-rt.StartupDone():
	case err := <-serveErr:
		t.Fatalf("Serve returned before finishing startup: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("Serve did not finish startup")
	}

	// A reaper that is not running before shutdown makes the assertion below
	// vacuous. Serve launches it, so this waits for the goroutine to be
	// scheduled rather than assuming it already is.
	if !waitUntil(30*time.Second, func() bool { return trashReapers() > base }) {
		t.Fatal("Serve started no recycle-bin reaper: there is nothing for the shutdown to stop")
	}

	close(api.release)

	select {
	case err := <-serveErr:
		if err == nil {
			t.Fatal("Serve returned nil: the API failure did not drive a shutdown")
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Serve did not return after the API server failed")
	}

	// Without this every assertion below could be satisfied by a cancellation
	// the test itself caused, which is the one thing this path exists to rule
	// out.
	if ctx.Err() != nil {
		t.Fatalf("the runtime context was cancelled (%v); the assertions below no longer say anything about the shutdown sequence", ctx.Err())
	}

	select {
	case <-gcCancelled:
	case <-time.After(15 * time.Second):
		t.Error("the in-flight block GC run was never cancelled; it keeps writing through the metadata stores the shutdown closed")
	}

	if !waitUntil(15*time.Second, func() bool { return trashReapers() <= base }) {
		t.Errorf("the recycle-bin reaper is still running after shutdown (%d, %d before the runtime):\n%s",
			trashReapers(), base, goroutineDump())
	}
}

// waitUntil polls cond until it holds or the budget expires. Polling rather
// than sleeping a fixed span keeps the test off the scheduler's timing: a
// machine under load takes longer to get there, it does not get there
// differently.
func waitUntil(budget time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(budget)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}
