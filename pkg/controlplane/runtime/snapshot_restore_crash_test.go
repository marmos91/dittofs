package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestRestoreCrashRecovery exercises issue #810: a crash mid-RestoreSnapshot
// must be self-healing on the next boot — the durable restore marker is
// detected at startup and the share is rolled back to the pre-restore safety
// snapshot, then the marker is cleared.
//
// The "crash" is simulated two ways:
//   - inject a real failure at a destructive step boundary (metadata Reset)
//     so RestoreSnapshot bails with the marker still durable, then drive the
//     startup-recovery path;
//   - directly leave a marker (a crash after Restore completed but before the
//     clear) and assert recovery still rolls back.
//
// "Restart" is modeled by building a fresh Runtime over the SAME control-plane
// store (where the marker lives) and re-registering the same in-memory
// metadata + block store instances (their post-crash contents survive an
// in-process restart the way an on-disk backend would survive a process
// restart).
func TestRestoreCrashRecovery(t *testing.T) {
	t.Run("MarkerClearedAfterSuccessfulRestore", testMarkerClearedAfterSuccess)
	t.Run("NoMarkerNoRecovery", testRecoveryNoMarkerIsNoop)
	t.Run("RecoverAfterMetaResetCrash", testRecoverAfterMetaResetCrash)
	t.Run("RecoverAfterRestoreCompleteButPreClear", testRecoverAfterRestorePreClear)
	t.Run("MarkerDurableAcrossRestart", testMarkerDurableAcrossRestart)
	t.Run("RollbackDoesNotCreateSafetySnapOrMarker", testRollbackNonRecursive)
	t.Run("RollbackIdempotentAcrossRepeatedRecovery", testRollbackIdempotent)
	t.Run("MarkerForUnknownShareIsCleared", testRecoveryClearsUnknownShareMarker)
	t.Run("MarkerClearFailureAbortsRestore", testMarkerClearFailureAborts)
}

// markerClearFailStore wraps a control-plane Store and makes the next
// DeleteRestoreMarker call fail. Models a DB error at the restore commit
// point so the test can assert the restore surfaces ErrRestoreAborted rather
// than reporting a success the next reboot would silently roll back.
type markerClearFailStore struct {
	cpstore.Store
	failNextDelete bool
}

func (s *markerClearFailStore) DeleteRestoreMarker(ctx context.Context, shareName string) error {
	if s.failNextDelete {
		s.failNextDelete = false
		return errors.New("synthetic DeleteRestoreMarker failure injected by test")
	}
	return s.Store.DeleteRestoreMarker(ctx, shareName)
}

// testMarkerClearFailureAborts: if the marker cannot be cleared after a
// fully-successful restore, restore returns ErrRestoreAborted (the clear is
// the commit point) and the marker is left in place for startup rollback.
func testMarkerClearFailureAborts(t *testing.T) {
	fx := newRestoreFixture(t, restoreFixtureOpts{})
	defer fx.close()

	// Swap in a wrapper store that fails the post-restore marker clear.
	wrapped := &markerClearFailStore{Store: fx.rt.store}
	fx.rt.store = wrapped
	fx.store = wrapped

	ctx := fx.ctx()
	files := fx.populateFiles(ctx, []string{"c1.bin", "c2.bin"})
	fx.seedRemoteAll(fx.allHashes())

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID); werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}
	fx.deleteFileCascade(ctx, files[0])

	wrapped.failNextDelete = true
	_, err = fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{})
	if !errors.Is(err, models.ErrRestoreAborted) {
		t.Fatalf("RestoreSnapshot err = %v, want ErrRestoreAborted on marker-clear failure", err)
	}

	// Marker is still present → the next boot would roll back. That is the
	// intended safety behavior.
	if _, gerr := fx.getMarker(ctx); gerr != nil {
		t.Fatalf("marker missing after clear failure (should be retained): %v", gerr)
	}
}

// testRecoveryClearsUnknownShareMarker: a marker stranded for a share that
// no longer exists in the runtime registry is cleared (not retried forever).
func testRecoveryClearsUnknownShareMarker(t *testing.T) {
	fx := newRestoreFixture(t, restoreFixtureOpts{})
	defer fx.close()

	ctx := fx.ctx()
	if err := fx.store.PutRestoreMarker(ctx, &models.RestoreMarker{
		ShareName:        "ghost-share",
		TargetSnapshotID: "t",
		SafetySnapshotID: "s",
		Step:             models.RestoreStepStarted,
	}); err != nil {
		t.Fatalf("PutRestoreMarker: %v", err)
	}

	if err := fx.rt.recoverInterruptedRestores(ctx); err != nil {
		t.Fatalf("recoverInterruptedRestores: %v", err)
	}

	if _, err := fx.store.GetRestoreMarker(ctx, "ghost-share"); !errors.Is(err, models.ErrRestoreMarkerNotFound) {
		t.Fatalf("ghost-share marker not cleared: err=%v", err)
	}
}

// simulateRestart builds a fresh Runtime over the fixture's existing
// control-plane store and re-registers the same metadata + block store
// instances under the same share. It swaps fx.rt to the new Runtime and
// returns it. Used to model a process restart: the control-plane DB (and
// therefore any restore marker) persists; the in-memory stores keep their
// post-crash contents.
func (f *restoreFixture) simulateRestart() *Runtime {
	f.t.Helper()

	// Tear down the old runtime's goroutines/ctx without touching the
	// shared cpstore (Shutdown closes metadata stores but not the cp).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = f.rt.Shutdown(ctx)
	cancel()

	rt := New(f.store)

	var registered metadata.Store = f.meta
	if f.failable != nil {
		registered = f.failable
	}
	if err := rt.RegisterMetadataStore("memory-restore", registered); err != nil {
		f.t.Fatalf("simulateRestart RegisterMetadataStore: %v", err)
	}
	rt.sharesSvc.InjectShareForTesting(&shares.Share{
		Name:          f.shareName,
		MetadataStore: "memory-restore",
		BlockStore:    f.bs,
		Enabled:       false,
	})
	if err := rt.sharesSvc.SetLocalStoreDirForTesting(f.shareName, f.localStoreDir); err != nil {
		f.t.Fatalf("simulateRestart SetLocalStoreDirForTesting: %v", err)
	}
	f.rt = rt
	return rt
}

func (f *restoreFixture) getMarker(ctx context.Context) (*models.RestoreMarker, error) {
	f.t.Helper()
	return f.store.GetRestoreMarker(ctx, f.shareName)
}

func (f *restoreFixture) mustNoMarker(ctx context.Context, when string) {
	f.t.Helper()
	_, err := f.getMarker(ctx)
	if !errors.Is(err, models.ErrRestoreMarkerNotFound) {
		f.t.Fatalf("%s: expected no restore marker, got err=%v", when, err)
	}
}

// testMarkerClearedAfterSuccess: a normal successful restore leaves no
// restore marker behind.
func testMarkerClearedAfterSuccess(t *testing.T) {
	fx := newRestoreFixture(t, restoreFixtureOpts{})
	defer fx.close()

	ctx := fx.ctx()
	files := fx.populateFiles(ctx, []string{"a.bin", "b.bin"})
	fx.seedRemoteAll(fx.allHashes())

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID); werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}

	fx.deleteFileCascade(ctx, files[0])

	if _, err := fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{}); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	fx.mustNoMarker(ctx, "after successful restore")
}

// testRecoveryNoMarkerIsNoop: recovery on a share with no marker is a no-op
// and does not error.
func testRecoveryNoMarkerIsNoop(t *testing.T) {
	fx := newRestoreFixture(t, restoreFixtureOpts{})
	defer fx.close()

	ctx := fx.ctx()
	fx.populateFiles(ctx, []string{"only.bin"})

	if err := fx.rt.recoverInterruptedRestores(ctx); err != nil {
		t.Fatalf("recoverInterruptedRestores (no marker): %v", err)
	}
	fx.mustNoMarker(ctx, "after no-op recovery")
}

// testRecoverAfterMetaResetCrash: inject a metadata-Reset failure so restore
// aborts with the marker durable, then simulate a restart and assert startup
// recovery rolls the share back to the safety snapshot and clears the marker.
func testRecoverAfterMetaResetCrash(t *testing.T) {
	fx := newRestoreFixture(t, restoreFixtureOpts{useFailableResetable: true})
	defer fx.close()

	ctx := fx.ctx()
	files := fx.populateFiles(ctx, []string{"keep.bin", "gone.bin"})
	fx.seedRemoteAll(fx.allHashes())

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID); werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}

	// Mutate AFTER the source snap: delete files[0]. The safety snap taken
	// during the (failing) restore captures this mutated state — recovery
	// must roll us back to it (files[0] absent, files[1] present).
	fx.deleteFileCascade(ctx, files[0])

	// Inject Reset failure: restore aborts after the marker is written and
	// after ResetLocalState, before metadata Reset succeeds.
	fx.failable.setFailNextReset(true)
	_, err = fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{})
	if !errors.Is(err, models.ErrRestoreAborted) {
		t.Fatalf("RestoreSnapshot err = %v, want ErrRestoreAborted", err)
	}

	// Marker durable, naming the safety snap.
	marker, merr := fx.getMarker(ctx)
	if merr != nil {
		t.Fatalf("expected durable restore marker after crash, got err=%v", merr)
	}
	if marker.SafetySnapshotID == "" {
		t.Fatal("marker has empty SafetySnapshotID")
	}
	if marker.TargetSnapshotID != snapID {
		t.Fatalf("marker.TargetSnapshotID = %q, want %q", marker.TargetSnapshotID, snapID)
	}
	safetyID := marker.SafetySnapshotID

	// --- restart + startup recovery ---
	rt := fx.simulateRestart()
	if err := rt.recoverInterruptedRestores(ctx); err != nil {
		t.Fatalf("recoverInterruptedRestores: %v", err)
	}

	// Marker cleared.
	fx.mustNoMarker(ctx, "after recovery rollback")

	// Share rolled back to the safety snap's state: files[1] present,
	// files[0] absent (it was deleted before the safety snap was taken).
	if _, gerr := fx.meta.GetFile(ctx, files[1].handle); gerr != nil {
		t.Fatalf("files[1] missing after rollback: %v", gerr)
	}
	if _, gerr := fx.meta.GetFile(ctx, files[0].handle); gerr == nil {
		t.Fatal("files[0] present after rollback — safety snap captured the deleted state")
	}

	// Safety snap still exists (rollback consumed it as the restore source,
	// did not delete it).
	if _, gerr := fx.store.GetSnapshot(ctx, fx.shareName, safetyID); gerr != nil {
		t.Fatalf("safety snap missing after rollback: %v", gerr)
	}

	// Share stays disabled.
	enabled, _ := rt.sharesSvc.IsShareEnabled(fx.shareName)
	if enabled {
		t.Fatal("share enabled after rollback recovery — must stay disabled")
	}
}

// testRecoverAfterRestorePreClear: model a crash that happened AFTER the
// metadata dump was fully replayed but BEFORE the marker was cleared. We
// drive a real, fully-successful restore, then re-insert a marker (as if the
// clear never landed) and assert recovery still rolls back to the safety
// snap cleanly.
func testRecoverAfterRestorePreClear(t *testing.T) {
	fx := newRestoreFixture(t, restoreFixtureOpts{})
	defer fx.close()

	ctx := fx.ctx()
	files := fx.populateFiles(ctx, []string{"x.bin", "y.bin"})
	fx.seedRemoteAll(fx.allHashes())

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID); werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}

	// Delete files[0] before restore so the safety snap reflects "only
	// files[1]". The restore brings files[0] back (target snap had both).
	fx.deleteFileCascade(ctx, files[0])

	safetyID, err := fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{})
	if err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	// Sanity: restore brought files[0] back.
	if _, gerr := fx.meta.GetFile(ctx, files[0].handle); gerr != nil {
		t.Fatalf("files[0] not restored: %v", gerr)
	}
	fx.mustNoMarker(ctx, "after successful restore")

	// Simulate "crash happened after Restore but before clear" by
	// re-inserting the marker at the post-restore step.
	if perr := fx.store.PutRestoreMarker(ctx, &models.RestoreMarker{
		ShareName:        fx.shareName,
		TargetSnapshotID: snapID,
		SafetySnapshotID: safetyID,
		Step:             models.RestoreStepRestored,
	}); perr != nil {
		t.Fatalf("PutRestoreMarker: %v", perr)
	}

	// Restart + recover → roll back to the safety snap (files[0] removed
	// again, files[1] present).
	rt := fx.simulateRestart()
	if err := rt.recoverInterruptedRestores(ctx); err != nil {
		t.Fatalf("recoverInterruptedRestores: %v", err)
	}
	fx.mustNoMarker(ctx, "after recovery")

	if _, gerr := fx.meta.GetFile(ctx, files[1].handle); gerr != nil {
		t.Fatalf("files[1] missing after rollback: %v", gerr)
	}
	if _, gerr := fx.meta.GetFile(ctx, files[0].handle); gerr == nil {
		t.Fatal("files[0] present after rollback to safety snap — wrong state")
	}
}

// testMarkerDurableAcrossRestart: the marker persists through a restart (it
// lives in the control-plane store, not in-memory runtime state).
func testMarkerDurableAcrossRestart(t *testing.T) {
	fx := newRestoreFixture(t, restoreFixtureOpts{useFailableResetable: true})
	defer fx.close()

	ctx := fx.ctx()
	files := fx.populateFiles(ctx, []string{"m1.bin", "m2.bin"})
	fx.seedRemoteAll(fx.allHashes())

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID); werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}
	fx.deleteFileCascade(ctx, files[0])

	fx.failable.setFailNextReset(true)
	if _, err := fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{}); !errors.Is(err, models.ErrRestoreAborted) {
		t.Fatalf("RestoreSnapshot err = %v, want ErrRestoreAborted", err)
	}

	before, berr := fx.getMarker(ctx)
	if berr != nil {
		t.Fatalf("marker missing pre-restart: %v", berr)
	}

	// Restart: build a fresh Runtime; the marker must still be readable.
	fx.simulateRestart()
	after, aerr := fx.getMarker(ctx)
	if aerr != nil {
		t.Fatalf("marker missing post-restart (not durable): %v", aerr)
	}
	if after.SafetySnapshotID != before.SafetySnapshotID || after.TargetSnapshotID != before.TargetSnapshotID {
		t.Fatalf("marker changed across restart: before=%+v after=%+v", before, after)
	}
}

// testRollbackNonRecursive: the recovery rollback must NOT create a fresh
// safety snapshot and must NOT write a new marker (guarding against the
// recursive-marker / unbounded-chain failure mode).
func testRollbackNonRecursive(t *testing.T) {
	fx := newRestoreFixture(t, restoreFixtureOpts{useFailableResetable: true})
	defer fx.close()

	ctx := fx.ctx()
	files := fx.populateFiles(ctx, []string{"r1.bin", "r2.bin"})
	fx.seedRemoteAll(fx.allHashes())

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID); werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}
	fx.deleteFileCascade(ctx, files[0])

	fx.failable.setFailNextReset(true)
	if _, err := fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{}); !errors.Is(err, models.ErrRestoreAborted) {
		t.Fatalf("RestoreSnapshot err = %v, want ErrRestoreAborted", err)
	}

	// Snapshot count before rollback: source + safety = 2.
	snapsBefore, err := fx.store.ListSnapshots(ctx, fx.shareName)
	if err != nil {
		t.Fatalf("ListSnapshots before: %v", err)
	}

	rt := fx.simulateRestart()
	if err := rt.recoverInterruptedRestores(ctx); err != nil {
		t.Fatalf("recoverInterruptedRestores: %v", err)
	}

	snapsAfter, err := fx.store.ListSnapshots(ctx, fx.shareName)
	if err != nil {
		t.Fatalf("ListSnapshots after: %v", err)
	}
	if len(snapsAfter) != len(snapsBefore) {
		t.Fatalf("rollback created snapshots: before=%d after=%d (must not snapshot the half-restored state)",
			len(snapsBefore), len(snapsAfter))
	}

	// No new marker written by the rollback.
	fx.mustNoMarker(ctx, "after non-recursive rollback")
}

// testRollbackIdempotent: running recovery twice (e.g. a crash during the
// first rollback) is safe — the second run is a clean no-op once the marker
// is cleared, and re-inserting the marker drives an identical rollback.
func testRollbackIdempotent(t *testing.T) {
	fx := newRestoreFixture(t, restoreFixtureOpts{useFailableResetable: true})
	defer fx.close()

	ctx := fx.ctx()
	files := fx.populateFiles(ctx, []string{"id1.bin", "id2.bin"})
	fx.seedRemoteAll(fx.allHashes())

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID); werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}
	fx.deleteFileCascade(ctx, files[0])

	fx.failable.setFailNextReset(true)
	if _, err := fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{}); !errors.Is(err, models.ErrRestoreAborted) {
		t.Fatalf("RestoreSnapshot err = %v, want ErrRestoreAborted", err)
	}
	marker, merr := fx.getMarker(ctx)
	if merr != nil {
		t.Fatalf("marker missing: %v", merr)
	}
	safetyID := marker.SafetySnapshotID

	rt := fx.simulateRestart()
	if err := rt.recoverInterruptedRestores(ctx); err != nil {
		t.Fatalf("recoverInterruptedRestores (1st): %v", err)
	}
	fx.mustNoMarker(ctx, "after first recovery")

	// Second recovery with no marker is a clean no-op.
	if err := rt.recoverInterruptedRestores(ctx); err != nil {
		t.Fatalf("recoverInterruptedRestores (2nd, no marker): %v", err)
	}

	// Re-insert the same marker (models a crash mid-first-rollback) and
	// recover again — must reach the same end state without error.
	if perr := fx.store.PutRestoreMarker(ctx, &models.RestoreMarker{
		ShareName:        fx.shareName,
		TargetSnapshotID: snapID,
		SafetySnapshotID: safetyID,
		Step:             models.RestoreStepStarted,
	}); perr != nil {
		t.Fatalf("PutRestoreMarker: %v", perr)
	}
	if err := rt.recoverInterruptedRestores(ctx); err != nil {
		t.Fatalf("recoverInterruptedRestores (re-run): %v", err)
	}
	fx.mustNoMarker(ctx, "after idempotent re-run")

	if _, gerr := fx.meta.GetFile(ctx, files[1].handle); gerr != nil {
		t.Fatalf("files[1] missing after idempotent rollback: %v", gerr)
	}
}
