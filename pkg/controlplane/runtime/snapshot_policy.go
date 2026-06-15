package runtime

import (
	"context"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// GetSnapshotPolicy returns the snapshot policy for a share, or
// models.ErrSnapshotPolicyNotFound when none exists.
func (r *Runtime) GetSnapshotPolicy(ctx context.Context, share string) (*models.SnapshotPolicy, error) {
	return r.store.GetSnapshotPolicy(ctx, share)
}

// ListSnapshotPolicies returns every snapshot policy across all shares.
func (r *Runtime) ListSnapshotPolicies(ctx context.Context) ([]*models.SnapshotPolicy, error) {
	return r.store.ListSnapshotPolicies(ctx)
}

// UpsertSnapshotPolicy creates or updates the policy for policy.ShareName. The
// share must exist (returns models.ErrShareNotFound otherwise) so a policy is
// never orphaned. On update, the existing run clock (LastRunAt) is preserved by
// the store.
func (r *Runtime) UpsertSnapshotPolicy(ctx context.Context, policy *models.SnapshotPolicy) error {
	if _, err := r.store.GetShare(ctx, policy.ShareName); err != nil {
		return err
	}
	return r.store.UpsertSnapshotPolicy(ctx, policy)
}

// DeleteSnapshotPolicy removes a share's snapshot policy. Returns
// models.ErrSnapshotPolicyNotFound when none exists.
func (r *Runtime) DeleteSnapshotPolicy(ctx context.Context, share string) error {
	return r.store.DeleteSnapshotPolicy(ctx, share)
}

// RunSnapshotPolicyNow triggers the share's policy immediately (manual
// override): it creates a scheduled snapshot ignoring the interval, advances
// the run clock, then prunes per the retention bounds. Returns the new
// snapshot ID. Returns models.ErrSnapshotPolicyNotFound when the share has no
// policy, or models.ErrSnapshotStateConflict when a snapshot is already in
// flight.
func (r *Runtime) RunSnapshotPolicyNow(ctx context.Context, share string) (string, error) {
	return r.SnapshotScheduler().RunNow(ctx, share)
}
