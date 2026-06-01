package runtime

import (
	"bytes"
	"context"
	"testing"
	"time"

	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestRestoreRollback_QuiescesRollupBeforeReset is the #8 H3 regression. The
// startup-rollback path (recoverInterruptedRestores → restoreSnapshot with
// isRollback=true) restores a pre-restore safety snapshot to DISCARD a
// half-restored state. The fs rollup worker pool is already running (started
// by AddShare) over boot-recovered dirty intervals; without quiescing it
// before ResetLocalState an in-flight rollup could persist post-snapshot
// FileBlock refs over the just-restored FileAttr.Blocks (silent corruption of
// the rolled-back share).
//
// This drives the real fs block store (append-log + rollup worker active) and
// asserts that after a rollback the restored file is byte-identical to the
// safety snapshot and its FileAttr.Blocks are the snapshot's blocks, not the
// discarded post-snapshot state.
func TestRestoreRollback_QuiescesRollupBeforeReset(t *testing.T) {
	meta := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	fx := newByteVerifyFixture(t, meta, "memory")
	defer fx.close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const mib = 1 << 20
	// Multi-chunk so FastCDC emits several CAS blocks and FileAttr.Blocks is
	// a meaningful structure to compare.
	orig := distinctBytes(2*mib, 0x5A)

	pid := fx.createEmptyFile(ctx, "rollme.bin")
	fx.writeFile(ctx, pid, orig)

	origFile := fx.getFile(ctx, "rollme.bin")
	if len(origFile.Blocks) < 2 {
		t.Fatalf("file produced %d CAS block(s), want >= 2 (multi-chunk path not exercised)", len(origFile.Blocks))
	}
	type blockKey struct {
		hash   string
		offset uint64
		size   uint32
	}
	var origBlocks []blockKey
	for _, b := range origFile.Blocks {
		origBlocks = append(origBlocks, blockKey{b.Hash.String(), b.Offset, b.Size})
	}

	// (1) The safety snapshot S captures the pristine pre-restore state. A
	// real startup rollback restores S to discard the half-restored state.
	safetyID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{NoVerify: true})
	if err != nil {
		t.Fatalf("CreateSnapshot(safety): %v", err)
	}
	if snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, safetyID); werr != nil {
		t.Fatalf("WaitForSnapshot(safety): %v", werr)
	} else if snap.State != models.StateReady {
		t.Fatalf("safety snap state = %q, want ready", snap.State)
	}

	// (2) Mutate the file in place with different bytes of the same length —
	// the half-restored, post-snapshot state we want the rollback to discard.
	// This leaves the fs append-log + rollup worker active on fresh intervals.
	mut := distinctBytes(2*mib, 0xB10C)
	if len(mut) != len(orig) {
		t.Fatalf("test bug: mut len %d != orig len %d", len(mut), len(orig))
	}
	fx.writeFile(ctx, pid, mut)
	if got := fx.readFile(ctx, pid, len(mut)); !bytes.Equal(got, mut) {
		t.Fatalf("post-mutate file should read the NEW bytes pre-rollback")
	}

	// (3) Disable the share and plant a restore marker naming S as the safety
	// snapshot — exactly what a crash mid-restore leaves behind. The rollup
	// worker pool stays alive.
	if err := fx.rt.DisableShare(ctx, fx.shareName); err != nil {
		t.Fatalf("DisableShare: %v", err)
	}
	if perr := fx.store.PutRestoreMarker(ctx, &models.RestoreMarker{
		ShareName:        fx.shareName,
		TargetSnapshotID: safetyID,
		SafetySnapshotID: safetyID,
		Step:             models.RestoreStepStarted,
	}); perr != nil {
		t.Fatalf("PutRestoreMarker: %v", perr)
	}

	// (4) Run startup recovery — the rollback path. With the H3 fix it drains
	// the fs rollup workers before ResetLocalState, so no in-flight rollup can
	// re-inject the discarded post-snapshot block refs over the restored tree.
	if err := fx.rt.recoverInterruptedRestores(ctx); err != nil {
		t.Fatalf("recoverInterruptedRestores: %v", err)
	}

	// Marker cleared (rollback completed).
	if _, gerr := fx.store.GetRestoreMarker(ctx, fx.shareName); gerr == nil {
		t.Fatal("restore marker still present after successful rollback")
	}

	// (5) The file must read back as the SAFETY snapshot (pristine) bytes and
	// its FileAttr.Blocks must be the snapshot's blocks — not the discarded
	// post-mutation state a racing rollup could have written.
	if !fx.fileExists(ctx, "rollme.bin") {
		t.Fatal("file missing after rollback")
	}
	got := fx.readFile(ctx, pid, len(orig))
	if !bytes.Equal(got, orig) {
		t.Fatalf("ROLLBACK file NOT byte-identical to safety snapshot: %s", firstDiff(orig, got))
	}

	rolled := fx.getFile(ctx, "rollme.bin")
	if len(rolled.Blocks) != len(origBlocks) {
		t.Fatalf("rolled-back FileAttr.Blocks count = %d, want %d (in-flight rollup clobbered restored blocks)",
			len(rolled.Blocks), len(origBlocks))
	}
	for i, b := range rolled.Blocks {
		if b.Hash.String() != origBlocks[i].hash || b.Offset != origBlocks[i].offset || b.Size != origBlocks[i].size {
			t.Fatalf("rolled-back block[%d] = {hash=%s off=%d size=%d}, want {hash=%s off=%d size=%d} — restored Blocks were clobbered",
				i, b.Hash.String(), b.Offset, b.Size,
				origBlocks[i].hash, origBlocks[i].offset, origBlocks[i].size)
		}
	}
}
