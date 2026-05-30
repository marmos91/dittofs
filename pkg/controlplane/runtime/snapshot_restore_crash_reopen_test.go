package runtime

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestRestoreCrashRecovery_AcrossRealReopen proves #810 startup recovery rolls
// back a half-finished restore after a REAL metadata-store reopen — not the
// in-memory re-register the memory-only crash suite uses. The marker lives in
// the control-plane store (kept alive across the simulated restart); the
// metadata store is closed and reopened from its durable location (badger
// dir / postgres DSN), so this also guards the postgres ensureStoreID reopen
// path that previously deadlocked an unbootable store and blocked recovery.
//
// Flow (real write path, mutation = ADD so it is backend-robust):
//  1. write a.bin + b.bin, snapshot S (local-only, NoVerify)
//  2. add c.bin  → the safety snap taken during the restore captures the
//     {a,b,c} state
//  3. restore S successfully → c.bin is gone again ({a,b})
//  4. re-insert the restore marker at RestoreStepRestored (models a crash
//     after replay, before the marker clear)
//  5. simulate restart: close + reopen the metadata store, rebuild the Runtime
//  6. recoverInterruptedRestores → rolls back to the safety snap, so c.bin
//     REAPPEARS (proving the rollback actually ran, not just a marker clear),
//     and a.bin/b.bin remain byte-identical through the reopened store.
func TestRestoreCrashRecovery_AcrossRealReopen(t *testing.T) {
	for _, bk := range byteVerifyBackends(t) {
		bk := bk
		t.Run(bk.name, func(t *testing.T) {
			if bk.skip != "" {
				t.Skip(bk.skip)
			}
			if bk.reopen == nil {
				t.Skipf("%s cannot survive a restart (no durable reopen)", bk.name)
			}
			meta, metaType := bk.open(t)
			fx := newByteVerifyFixtureOpts(t, meta, metaType, nil)
			defer fx.close()

			ctx := context.Background()
			origA := distinctBytes(64*1024, 0xA)
			origB := distinctBytes(48*1024, 0xB)
			origC := distinctBytes(32*1024, 0xC)

			pidA := fx.createEmptyFile(ctx, "a.bin")
			fx.writeFile(ctx, pidA, origA)
			pidB := fx.createEmptyFile(ctx, "b.bin")
			fx.writeFile(ctx, pidB, origB)

			snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{NoVerify: true})
			if err != nil {
				t.Fatalf("CreateSnapshot: %v", err)
			}
			if _, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID); werr != nil {
				t.Fatalf("WaitForSnapshot: %v", werr)
			}

			// Mutate AFTER the snapshot: add c.bin. The safety snap taken
			// during the restore below captures this {a,b,c} state.
			pidC := fx.createEmptyFile(ctx, "c.bin")
			fx.writeFile(ctx, pidC, origC)

			// Restore requires the share disabled.
			if err := fx.rt.DisableShare(ctx, fx.shareName); err != nil {
				t.Fatalf("DisableShare: %v", err)
			}

			safetyID, err := fx.rt.RestoreSnapshot(ctx, fx.shareName, snapID, RestoreSnapshotOpts{AllowNonDurable: true})
			if err != nil {
				t.Fatalf("RestoreSnapshot: %v", err)
			}
			if fx.fileExists(ctx, "c.bin") {
				t.Fatal("c.bin still present after restore to the pre-c snapshot")
			}

			// Model a crash AFTER replay but BEFORE the marker clear.
			if perr := fx.store.PutRestoreMarker(ctx, &models.RestoreMarker{
				ShareName:        fx.shareName,
				TargetSnapshotID: snapID,
				SafetySnapshotID: safetyID,
				Step:             models.RestoreStepRestored,
			}); perr != nil {
				t.Fatalf("PutRestoreMarker: %v", perr)
			}

			// --- REAL restart: close + reopen the metadata store ---
			fx.simulateRestart(bk.reopen)

			if err := fx.rt.recoverInterruptedRestores(ctx); err != nil {
				t.Fatalf("recoverInterruptedRestores: %v", err)
			}

			// Marker cleared.
			if _, gerr := fx.store.GetRestoreMarker(ctx, fx.shareName); !errors.Is(gerr, models.ErrRestoreMarkerNotFound) {
				t.Fatalf("marker not cleared after recovery: err=%v", gerr)
			}

			// Rolled back to the safety snap: c.bin REAPPEARS (proves the
			// rollback restore actually ran across the reopen, not a bare
			// marker clear), and a.bin/b.bin are byte-identical through the
			// reopened store + persisted CAS.
			if !fx.fileExists(ctx, "c.bin") {
				t.Fatal("c.bin missing after rollback — recovery cleared the marker without restoring the safety snap")
			}
			pidC2 := fx.getFile(ctx, "c.bin").PayloadID
			if gotC := fx.readFile(ctx, pidC2, len(origC)); !bytes.Equal(gotC, origC) {
				t.Fatalf("c.bin NOT byte-identical after rollback+reopen: %s", firstDiff(origC, gotC))
			}
			pidA2 := fx.getFile(ctx, "a.bin").PayloadID
			if gotA := fx.readFile(ctx, pidA2, len(origA)); !bytes.Equal(gotA, origA) {
				t.Fatalf("a.bin NOT byte-identical after rollback+reopen: %s", firstDiff(origA, gotA))
			}
			pidB2 := fx.getFile(ctx, "b.bin").PayloadID
			if gotB := fx.readFile(ctx, pidB2, len(origB)); !bytes.Equal(gotB, origB) {
				t.Fatalf("b.bin NOT byte-identical after rollback+reopen: %s", firstDiff(origB, gotB))
			}

			// Share stays disabled after recovery.
			if enabled, _ := fx.rt.sharesSvc.IsShareEnabled(fx.shareName); enabled {
				t.Fatal("share enabled after recovery rollback — must stay disabled")
			}
		})
	}
}
