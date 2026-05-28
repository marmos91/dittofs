package snapshot

import (
	"fmt"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ValidateRetryTarget checks that snap is eligible as a retry target for
// CreateSnapshot(..., CreateSnapshotOpts{RetryOf: snap.ID}).
//
// Semantics (Phase 23 D-23-10):
//   - snap == nil → ErrSnapshotRetryTargetNotFound (the orchestration
//     caller has already looked up the row; nil means "no such ID").
//   - snap.State == StateFailed → eligible, returns nil.
//   - Any other state (ready, creating, unknown) → ErrSnapshotRetryTargetNotFailed.
//
// The error is wrapped with the observed snapshot ID + state so operators
// can diagnose without re-reading the DB.
func ValidateRetryTarget(snap *models.Snapshot) error {
	if snap == nil {
		return models.ErrSnapshotRetryTargetNotFound
	}
	if snap.State == models.StateFailed {
		return nil
	}
	return fmt.Errorf("snapshot retry target %q in state %q: %w",
		snap.ID, snap.State, models.ErrSnapshotRetryTargetNotFailed)
}
