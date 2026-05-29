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
	// An empty manifest on a non-empty share is the manifestCount==0 edge
	// of the broader incomplete-manifest guard, so it surfaces the more
	// precise ErrSnapshotManifestIncomplete sentinel.
	if !errors.Is(werr, models.ErrSnapshotManifestIncomplete) {
		t.Fatalf("WaitForSnapshot err = %v, want errors.Is(ErrSnapshotManifestIncomplete)", werr)
	}
	if snap == nil || snap.State != models.StateFailed {
		t.Fatalf("snapshot state = %v, want failed", snap)
	}
	if snap.RemoteDurable {
		t.Fatal("RemoteDurable=true on a snapshot that captured zero hashes for a non-empty share (hollow durability)")
	}
}

// TestCreateSnapshot_PartialManifestFails covers the #789 guard: a
// metadata backend that persists block refs incompletely yields a manifest
// that covers SOME but not ALL of the blocks the metadata still references
// (0 < manifestCount < liveHashes). VerifyRemoteDurability would only probe
// the captured subset and the snapshot would be marked remote_durable=true
// over a silently-truncated manifest. The orchestration must refuse with
// ErrSnapshotManifestIncomplete.
//
// Scenario: populate three files {a,b,c} (each seeds a File.Blocks entry
// AND a standalone FileBlock row). Delete ONLY the File entry for c,
// leaving its FileBlock row in place. Backup builds the manifest from
// File.Blocks -> {a,b}; HashSetFromMetadataStore enumerates FileBlock rows
// -> {a,b,c}. The completeness cross-check detects 2 < 3 and fails.
func TestCreateSnapshot_PartialManifestFails(t *testing.T) {
	t.Parallel()
	fx := newRestoreFixture(t, restoreFixtureOpts{})
	defer fx.close()

	ctx := fx.ctx()
	files := fx.populateFiles(ctx, []string{"a.bin", "b.bin", "c.bin"})
	fx.seedRemoteAll(fx.allHashes())

	// Delete ONLY the File for c.bin, NOT its standalone FileBlock row.
	if err := fx.meta.DeleteFile(ctx, files[2].handle); err != nil {
		t.Fatalf("DeleteFile c.bin: %v", err)
	}

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot (sync part): %v", err)
	}
	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	if !errors.Is(werr, models.ErrSnapshotManifestIncomplete) {
		t.Fatalf("WaitForSnapshot err = %v, want errors.Is(ErrSnapshotManifestIncomplete)", werr)
	}
	if snap == nil || snap.State != models.StateFailed {
		t.Fatalf("snapshot state = %v, want failed", snap)
	}
	if snap.RemoteDurable {
		t.Fatal("RemoteDurable=true on a snapshot whose manifest dropped a referenced block")
	}
}

// TestCreateSnapshot_CompleteManifestSucceeds is the positive counterpart:
// when the manifest covers EVERY block the metadata references
// (manifestCount == liveHashes), the completeness guard passes and the
// snapshot reaches ready+remote_durable=true.
func TestCreateSnapshot_CompleteManifestSucceeds(t *testing.T) {
	t.Parallel()
	fx := newRestoreFixture(t, restoreFixtureOpts{})
	defer fx.close()

	ctx := fx.ctx()
	fx.populateFiles(ctx, []string{"a.bin", "b.bin", "c.bin"})
	fx.seedRemoteAll(fx.allHashes())

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot (sync part): %v", err)
	}
	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	if werr != nil {
		t.Fatalf("WaitForSnapshot on complete manifest: %v", werr)
	}
	if snap.State != models.StateReady {
		t.Fatalf("snapshot state = %q, want ready", snap.State)
	}
	if !snap.RemoteDurable {
		t.Fatal("RemoteDurable=false on a complete-manifest snapshot")
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
