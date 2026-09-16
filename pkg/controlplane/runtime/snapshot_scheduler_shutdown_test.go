package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/snapshotsched"
)

// blockingSchedDeps pins the first ListPolicies open, standing in for a
// scheduler tick that is inside the control-plane store when shutdown begins.
type blockingSchedDeps struct {
	once     sync.Once
	entered  chan struct{}
	release  chan struct{}
	returned chan struct{}
}

func (b *blockingSchedDeps) ListPolicies(ctx context.Context) ([]*models.SnapshotPolicy, error) {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
		close(b.returned)
	})
	return nil, nil
}

func (b *blockingSchedDeps) GetPolicy(context.Context, string) (*models.SnapshotPolicy, error) {
	return nil, models.ErrSnapshotPolicyNotFound
}
func (b *blockingSchedDeps) CreateScheduledSnapshot(context.Context, string, string) (string, error) {
	return "", nil
}
func (b *blockingSchedDeps) ListSnapshots(context.Context, string) ([]*models.Snapshot, error) {
	return nil, nil
}
func (b *blockingSchedDeps) DeleteSnapshot(context.Context, string, string) error { return nil }
func (b *blockingSchedDeps) TouchPolicyRun(context.Context, string, time.Time) error {
	return nil
}

// TestShutdownSnapshots_JoinsTheScheduler pins the wiring the control-plane
// store's lifetime rests on. The scheduler reads and writes snapshot policies
// through that store, and shutdownSnapshots is the one seam both the lifecycle
// drain and Runtime.Shutdown route through, so a tick still inside the store
// must not survive it: whoever called shutdownSnapshots closes the store next.
//
// Before the scheduler was joined here nothing on the serving path stopped it
// at all — it exited on context cancellation alone, unwaited.
func TestShutdownSnapshots_JoinsTheScheduler(t *testing.T) {
	deps := &blockingSchedDeps{
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		returned: make(chan struct{}),
	}

	rt := New(nil)
	svc := snapshotsched.New(deps, time.Millisecond)
	rt.mu.Lock()
	rt.snapSchedSvc = svc
	rt.mu.Unlock()
	svc.Start(context.Background())

	select {
	case <-deps.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler never entered a tick")
	}

	drained := make(chan struct{})
	go func() {
		rt.ShutdownSnapshots(context.Background())
		close(drained)
	}()

	select {
	case <-drained:
		t.Fatal("shutdownSnapshots returned while a scheduler tick was still in the store")
	case <-time.After(100 * time.Millisecond):
	}

	close(deps.release)

	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdownSnapshots did not return after the tick finished")
	}
	select {
	case <-deps.returned:
	default:
		t.Fatal("shutdownSnapshots returned before the tick's store call completed")
	}
}

// TestDrainStartupWorkers_JoinsTheSettingsWatcher pins the second worker the
// control-plane store's lifetime rests on. lifecycle.Serve starts the settings
// watcher before it loads adapters, and returns an adapter-load failure without
// running its shutdown hook — so on that path nothing else stops a watcher that
// polls the same store on a timer, and the caller closes that store as soon as
// Serve returns.
func TestDrainStartupWorkers_JoinsTheSettingsWatcher(t *testing.T) {
	rt := New(nil)
	rt.settingsWatcher = NewSettingsWatcher(nil, time.Hour)
	rt.settingsWatcher.Start(context.Background())

	select {
	case <-rt.settingsWatcher.stopped:
		t.Fatal("the watcher reports stopped before anything stopped it")
	default:
	}

	rt.drainStartupWorkers(context.Background())

	select {
	case <-rt.settingsWatcher.stopped:
	default:
		t.Fatal("the settings watcher outlived the startup drain, so it can still be " +
			"polling the control-plane store the caller closes next")
	}
}

// TestDrainStartupWorkers_BoundsTheSettingsWatcherJoin pins the bound on that
// join. SettingsWatcher.Stop waits for a poll already in flight and takes no
// context of its own, and on a startup error the context that poll runs under
// is still live — so a store call that hangs would hang the drain, and with it
// a process that is trying to abandon its boot.
func TestDrainStartupWorkers_BoundsTheSettingsWatcherJoin(t *testing.T) {
	rt := New(nil)
	rt.settingsWatcher = NewSettingsWatcher(nil, time.Hour)
	// A watcher whose goroutine never exits: stopped stays open, so Stop blocks
	// forever. This is the shape of a poll wedged in the store.
	rt.settingsWatcher.stopped = make(chan struct{})
	rt.settingsWatcher.stopCh = make(chan struct{})

	// The drain detaches from the caller's context on purpose — a join has to
	// outlive a cancellation — so the bound that matters is its own.
	defer func(d time.Duration) { startupDrainTimeout = d }(startupDrainTimeout)
	startupDrainTimeout = 50 * time.Millisecond

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		rt.drainStartupWorkers(context.Background())
	}()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the startup drain never returned: a wedged settings-watcher poll held it, " +
			"so the process cannot abandon a failed boot")
	}
}

// TestSettingsWatcher_ConcurrentStopIsSafeAndJoins pins what makes it safe to
// call Stop from both shutdown paths. Each of them bounds its own wait and keeps
// running after it expires, so the two calls genuinely overlap — and an
// unsynchronized check-then-close of stopCh is two goroutines racing to close
// one channel, which panics the process during the shutdown meant to be orderly.
//
// Every caller must also come back joined: a second Stop that returns early
// because the channel is already closed tells its caller the poll has finished
// when it may still be in the store.
//
// What this test does NOT do is reliably reproduce the double close. That needs
// two goroutines inside one select window, and eight racing calls do not hit it
// dependably — so treat this as a smoke test for the joining contract, not as a
// regression detector for the panic. The argument for the lock is that the code
// is correct under concurrent callers by construction rather than by their
// happening not to overlap, which they now do.
func TestSettingsWatcher_ConcurrentStopIsSafeAndJoins(t *testing.T) {
	w := NewSettingsWatcher(nil, time.Hour)
	w.Start(context.Background())

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Stop()
		}()
	}

	done := make(chan struct{})
	go func() { defer close(done); wg.Wait() }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent Stop calls did not all return")
	}

	select {
	case <-w.stopped:
	default:
		t.Error("Stop returned while the polling goroutine was still running: " +
			"a caller that believes it joined will close the store under the poll")
	}
}

// blockedSettingsStore stands in for a control-plane store whose query is not
// coming back on its own. Only GetAdapter is reached by the watcher's poll; any
// other method would panic on the nil embedded interface, which is the point —
// it says exactly what this fake supports.
type blockedSettingsStore struct {
	store.Store
	entered chan struct{}
	once    sync.Once
}

func (b *blockedSettingsStore) GetAdapter(ctx context.Context, _ string) (*models.AdapterConfig, error) {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestSettingsWatcher_StopCancelsAPollInFlight pins the difference between a
// join and a timeout. A bounded wait that expires does not stop the worker, it
// only stops waiting — and the caller then closes the control-plane store under
// a poll still inside it, which is the outcome the join exists to prevent.
// Cancelling the poll's context is what turns the wait into one that completes.
func TestSettingsWatcher_StopCancelsAPollInFlight(t *testing.T) {
	blocked := &blockedSettingsStore{entered: make(chan struct{})}
	w := NewSettingsWatcher(blocked, time.Millisecond)
	w.Start(context.Background())

	select {
	case <-blocked.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never entered a poll")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Stop()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop never returned: it is waiting on a poll it did not cancel, so a caller " +
			"that bounds this wait closes the store while that query is still running")
	}
}

// TestSettingsWatcher_StartAfterStopDoesNotLaunch pins the one-way transition.
// Start and Stop can race: if Stop wins the mutex it closes the constructor's
// stopCh and returns on the already-closed stopped channel, and a Start behind
// it would then install fresh channels and launch a poller no caller holds a
// handle to — against a control-plane store the caller is about to close.
func TestSettingsWatcher_StartAfterStopDoesNotLaunch(t *testing.T) {
	blocked := &blockedSettingsStore{entered: make(chan struct{})}
	w := NewSettingsWatcher(blocked, time.Millisecond)

	w.Stop()
	w.Start(context.Background())

	// If Start launched anything, it would reach the store.
	select {
	case <-blocked.entered:
		t.Fatal("a watcher that had already been stopped started polling: nothing can join it now")
	case <-time.After(250 * time.Millisecond):
	}

	// And a second Stop still returns rather than waiting on a goroutine that
	// was never launched.
	done := make(chan struct{})
	go func() { defer close(done); w.Stop() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop blocked after a refused Start")
	}
}
