package runtime

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// TestRecoverInterruptedRestores_KeepsALiveSharesMarker: with no content
// store to roll back with, a live share's marker is kept (it is the only
// record of the safety snapshot), and a removed share's marker is cleared.
func TestRecoverInterruptedRestores_KeepsALiveSharesMarker(t *testing.T) {
	ctx := context.Background()
	rt := newTestRuntime(t)
	rt.sharesSvc.InjectShareForTesting(&shares.Share{Name: "live"})
	for _, name := range []string{"live", "gone"} {
		if err := rt.store.PutRestoreMarker(ctx, &models.RestoreMarker{
			ShareName:        name,
			TargetSnapshotID: uuid.NewString(),
			SafetySnapshotID: uuid.NewString(),
			Step:             models.RestoreStepStarted,
		}); err != nil {
			t.Fatalf("PutRestoreMarker %s: %v", name, err)
		}
	}

	if err := rt.recoverInterruptedRestores(ctx); err != nil {
		t.Fatalf("recoverInterruptedRestores: %v", err)
	}
	if m, err := rt.store.GetRestoreMarker(ctx, "live"); err != nil || m == nil {
		t.Fatalf("live share's marker = %v, %v; want it kept", m, err)
	}
	if _, err := rt.store.GetRestoreMarker(ctx, "gone"); err == nil {
		t.Fatal("removed share's marker kept, want it cleared")
	}
}
