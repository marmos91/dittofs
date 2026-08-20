package runtime

import (
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestWaitForSnapshot_ErrorSurvivesOrchestrationFinishingFirst pins the
// contract that WaitForSnapshot reports a failed snapshot as a failure even
// when the caller arrives after orchestration has already finished and torn
// down its in-memory result channel. The row is the authoritative outcome, so
// a terminal state=failed row must yield the typed sentinel regardless of who
// won the race.
func TestWaitForSnapshot_ErrorSurvivesOrchestrationFinishingFirst(t *testing.T) {
	t.Parallel()
	fx := newRestoreFixture(t, restoreFixtureOpts{})
	defer fx.close()

	ctx := fx.ctx()
	files := fx.populateFiles(ctx, []string{"waitrace.bin"})

	// Same C3 inconsistency used by the empty-manifest test: the snapshot
	// fails verify deterministically.
	if err := fx.meta.DeleteFile(ctx, files[0].handle); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot (sync part): %v", err)
	}

	// Let orchestration settle before waiting — this is what a loaded machine
	// does by accident.
	deadline := time.Now().Add(30 * time.Second)
	for {
		row, gerr := fx.rt.GetSnapshot(ctx, fx.shareName, snapID)
		if gerr != nil {
			t.Fatalf("GetSnapshot: %v", gerr)
		}
		if row.State != models.StateCreating {
			t.Logf("orchestration settled: state=%s", row.State)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("snapshot never left state=creating")
		}
		time.Sleep(5 * time.Millisecond)
	}

	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	if snap == nil || snap.State != models.StateFailed {
		t.Fatalf("snapshot state = %v, want failed", snap)
	}
	if !errors.Is(werr, models.ErrSnapshotVerifyFailed) {
		t.Fatalf("WaitForSnapshot err = %v, want errors.Is(ErrSnapshotVerifyFailed) for a state=failed row", werr)
	}
}
