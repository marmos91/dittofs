package snapshot_test

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

func TestValidateRetryTarget(t *testing.T) {
	tests := []struct {
		name    string
		snap    *models.Snapshot
		wantErr error
	}{
		{
			name:    "nil snapshot returns NotFound",
			snap:    nil,
			wantErr: models.ErrSnapshotRetryTargetNotFound,
		},
		{
			name:    "state=failed is valid retry target",
			snap:    &models.Snapshot{ID: "abc", State: models.StateFailed},
			wantErr: nil,
		},
		{
			name:    "state=ready rejected (only failed can retry)",
			snap:    &models.Snapshot{ID: "abc", State: models.StateReady},
			wantErr: models.ErrSnapshotRetryTargetNotFailed,
		},
		{
			name:    "state=creating rejected (in-flight retries forbidden)",
			snap:    &models.Snapshot{ID: "abc", State: models.StateCreating},
			wantErr: models.ErrSnapshotRetryTargetNotFailed,
		},
		{
			name:    "unknown state rejected",
			snap:    &models.Snapshot{ID: "abc", State: "garbage"},
			wantErr: models.ErrSnapshotRetryTargetNotFailed,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := snapshot.ValidateRetryTarget(tc.snap)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateRetryTarget: got %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("ValidateRetryTarget: got %v, want errors.Is %v", err, tc.wantErr)
			}
		})
	}
}
