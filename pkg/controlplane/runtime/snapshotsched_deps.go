package runtime

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/snapshotsched"
)

// SetSnapshotSchedulerConfig configures the background snapshot scheduler
// before Serve launches it. A zero pollInterval keeps the built-in default.
// disabled=true prevents Serve from starting the scheduler at all. Must be
// called before Serve.
func (r *Runtime) SetSnapshotSchedulerConfig(pollInterval time.Duration, disabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapSchedPollInterval = pollInterval
	r.snapSchedDisabled = disabled
}

// SnapshotScheduler returns the background snapshot scheduler, constructed
// lazily on first call so a Runtime that never serves carries no extra state.
// The scheduler goroutine is launched/stopped by the lifecycle Serve/shutdown
// path.
func (r *Runtime) SnapshotScheduler() *snapshotsched.Service {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.snapSchedSvc == nil {
		// New defaults a zero/negative interval to its built-in cadence.
		r.snapSchedSvc = snapshotsched.New(&snapSchedDeps{rt: r}, r.snapSchedPollInterval)
	}
	return r.snapSchedSvc
}

// snapSchedDeps adapts the Runtime to the narrow snapshotsched.Deps surface.
type snapSchedDeps struct {
	rt *Runtime
}

var _ snapshotsched.Deps = (*snapSchedDeps)(nil)

func (d *snapSchedDeps) ListPolicies(ctx context.Context) ([]*models.SnapshotPolicy, error) {
	return d.rt.store.ListSnapshotPolicies(ctx)
}

func (d *snapSchedDeps) GetPolicy(ctx context.Context, share string) (*models.SnapshotPolicy, error) {
	return d.rt.store.GetSnapshotPolicy(ctx, share)
}

// CreateScheduledSnapshot creates a snapshot marked Scheduled=true so retention
// pruning can distinguish it from manually-created snapshots.
func (d *snapSchedDeps) CreateScheduledSnapshot(ctx context.Context, share, name string) (string, error) {
	return d.rt.CreateSnapshot(ctx, share, CreateSnapshotOpts{Name: name, Scheduled: true})
}

func (d *snapSchedDeps) ListSnapshots(ctx context.Context, share string) ([]*models.Snapshot, error) {
	return d.rt.ListSnapshots(ctx, share)
}

func (d *snapSchedDeps) DeleteSnapshot(ctx context.Context, share, id string) error {
	return d.rt.DeleteSnapshot(ctx, share, id)
}

func (d *snapSchedDeps) TouchPolicyRun(ctx context.Context, share string, ranAt time.Time) error {
	return d.rt.store.TouchSnapshotPolicyRun(ctx, share, ranAt)
}
