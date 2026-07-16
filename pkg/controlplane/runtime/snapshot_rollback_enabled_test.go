package runtime

import (
	"bytes"
	"context"
	"testing"
	"time"

	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestRestoreRollback_ProceedsWhenShareEnabledAtBoot is the #8 R-L1 regression.
//
// The startup-rollback path (recoverInterruptedRestores → restoreSnapshot with
// isRollback=true) must NOT be blocked by the operator-restore Enabled
// precheck. A share that loaded Enabled at boot with a stranded restore marker
// would, without the isRollback exemption, fail the Enabled precheck
// (models.ErrShareEnabled) on every boot, wedging the share half-restored
// forever. The fix force-disables the share for the rollback duration instead.
//
// This plants a marker with the share STILL ENABLED, runs startup recovery, and
// asserts the rollback proceeds: the marker is cleared, the share ends up
// disabled (matching the operator-restore contract), and the file is restored
// to the safety-snapshot bytes.
func TestRestoreRollback_ProceedsWhenShareEnabledAtBoot(t *testing.T) {
	meta := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	fx := newByteVerifyFixture(t, meta, "memory")
	defer fx.close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const mib = 1 << 20
	orig := distinctBytes(mib, 0x11)

	pid := fx.createEmptyFile(ctx, "enabled-rollback.bin")
	fx.writeFile(ctx, pid, orig)

	// Safety snapshot S: the pristine pre-restore state the rollback restores.
	safetyID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{NoVerify: true})
	if err != nil {
		t.Fatalf("CreateSnapshot(safety): %v", err)
	}
	if snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, safetyID); werr != nil {
		t.Fatalf("WaitForSnapshot(safety): %v", werr)
	} else if snap.State != models.StateReady {
		t.Fatalf("safety snap state = %q, want ready", snap.State)
	}

	// Mutate in place: the half-restored, post-snapshot state to discard.
	mut := distinctBytes(mib, 0x22)
	fx.writeFile(ctx, pid, mut)

	// Confirm the share is ENABLED at this point — the whole point of the test.
	// We deliberately do NOT call DisableShare, modeling a share that loaded
	// Enabled at boot with a stranded marker.
	if enabled, eerr := fx.rt.sharesSvc.IsShareEnabled(fx.shareName); eerr != nil {
		t.Fatalf("IsShareEnabled: %v", eerr)
	} else if !enabled {
		t.Fatal("test precondition: share must be ENABLED before recovery")
	}

	// Plant the restore marker naming S as the safety snapshot — exactly what a
	// crash mid-restore leaves behind, except the share is still Enabled.
	if perr := fx.store.PutRestoreMarker(ctx, &models.RestoreMarker{
		ShareName:        fx.shareName,
		TargetSnapshotID: safetyID,
		SafetySnapshotID: safetyID,
		Step:             models.RestoreStepStarted,
	}); perr != nil {
		t.Fatalf("PutRestoreMarker: %v", perr)
	}

	// Run startup recovery. With the R-L1 fix the rollback force-disables the
	// share and proceeds; without it this returns models.ErrShareEnabled and
	// the marker is retained (wedged share).
	if err := fx.rt.recoverInterruptedRestores(ctx); err != nil {
		t.Fatalf("recoverInterruptedRestores returned error (rollback wedged on Enabled share): %v", err)
	}

	// Marker cleared → rollback completed (not retained-on-failure).
	if _, gerr := fx.store.GetRestoreMarker(ctx, fx.shareName); gerr == nil {
		t.Fatal("restore marker still present after recovery — rollback did not proceed (R-L1 regression)")
	}

	// Share force-disabled and STAYS disabled (operator-restore contract).
	if enabled, eerr := fx.rt.sharesSvc.IsShareEnabled(fx.shareName); eerr != nil {
		t.Fatalf("IsShareEnabled after rollback: %v", eerr)
	} else if enabled {
		t.Fatal("share should be DISABLED after rollback (force-disabled for rollback duration)")
	}

	// File restored to the safety-snapshot bytes, not the discarded mutation.
	if !fx.fileExists(ctx, "enabled-rollback.bin") {
		t.Fatal("file missing after rollback")
	}
	got := fx.readFile(ctx, pid, len(orig))
	if !bytes.Equal(got, orig) {
		t.Fatalf("rolled-back file NOT byte-identical to safety snapshot: %s", firstDiff(orig, got))
	}
}
