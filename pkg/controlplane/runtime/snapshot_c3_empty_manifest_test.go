package runtime

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestCreateSnapshot_EmptyManifestOnNonEmptyShareFails covers the C3
// guard: VerifyRemoteDurability short-circuits to success on an empty
// manifest without probing any block, so a snapshot whose backup captured
// ZERO hashes while the share still references blocks would otherwise be
// reported remote_durable=true over an unverified empty set (hollow
// durability). The orchestration must refuse that case.
//
// Scenario: seed a file (which also seeds a standalone FileBlock row),
// then delete ONLY the File entry — leaving the FileBlock row in place.
// The Backup-derived manifest is now empty (no file references any
// hash), but EnumerateFileBlocks still returns the orphan hash. The C3
// cross-check detects the mismatch and fails the snapshot rather than
// claiming durability.
func TestCreateSnapshot_EmptyManifestOnNonEmptyShareFails(t *testing.T) {
	t.Parallel()
	fx := newRestoreFixture(t, restoreFixtureOpts{})
	defer fx.close()

	ctx := fx.ctx()
	files := fx.populateFiles(ctx, []string{"c3.bin"})

	// Delete ONLY the File, NOT the standalone FileBlock row. This is the
	// exact inconsistency C3 protects against: manifest (from file.Blocks)
	// is empty, but the store still references a hash via EnumerateFileBlocks.
	if err := fx.meta.DeleteFile(ctx, files[0].handle); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot (sync part): %v", err)
	}
	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	if !errors.Is(werr, models.ErrSnapshotVerifyFailed) {
		t.Fatalf("WaitForSnapshot err = %v, want errors.Is(ErrSnapshotVerifyFailed)", werr)
	}
	if snap == nil || snap.State != models.StateFailed {
		t.Fatalf("snapshot state = %v, want failed", snap)
	}
	if snap.RemoteDurable {
		t.Fatal("RemoteDurable=true on a snapshot that captured zero hashes for a non-empty share (hollow durability)")
	}
}

// TestCreateSnapshot_EmptyShareVacuousVerify is the legitimate-empty
// counterpart: a share with NO files and NO FileBlock rows produces an
// empty manifest, and the C3 guard must let it pass with a vacuous verify
// (remote_durable=true over zero blocks is honest here — there is nothing
// to verify).
func TestCreateSnapshot_EmptyShareVacuousVerify(t *testing.T) {
	t.Parallel()
	fx := newRestoreFixture(t, restoreFixtureOpts{})
	defer fx.close()

	ctx := fx.ctx()
	// No populateFiles — the share is genuinely empty.

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot (sync part): %v", err)
	}
	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	if werr != nil {
		t.Fatalf("WaitForSnapshot on empty share: %v", werr)
	}
	if snap.State != models.StateReady {
		t.Fatalf("snapshot state = %q, want ready", snap.State)
	}
	if !snap.RemoteDurable {
		t.Fatal("RemoteDurable=false on a genuinely-empty share; vacuous verify should pass")
	}
}
