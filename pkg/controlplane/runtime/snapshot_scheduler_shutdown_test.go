package runtime

import (
	"context"
	"sync"
	"testing"
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
