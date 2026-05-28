package storetest

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// asResetable is a helper that type-asserts a MetadataStore to Resetable,
// calling t.Fatal if the assertion fails. Mirrors asBackupable.
func asResetable(t *testing.T, store metadata.MetadataStore) metadata.Resetable {
	t.Helper()
	r, ok := store.(metadata.Resetable)
	if !ok {
		t.Fatal("store does not implement metadata.Resetable")
	}
	return r
}

// ResetThenRestoreConformance verifies that a store implementing both
// Backupable and Resetable satisfies the restore-flow contract: a
// populated store can be backed up, Reset to empty, then restored from
// the same backup to its original content. Reset must leave the store
// empty enough for Restore to proceed past its
// ErrRestoreDestinationNotEmpty precondition, and the restored state
// must equal the pre-Reset state.
func ResetThenRestoreConformance(t *testing.T, factory BackupableStoreFactory) {
	t.Helper()

	store := factory(t)
	b := asBackupable(t, store)
	r := asResetable(t, store)

	ctx := t.Context()

	// Populate: unique prefix "rst" to avoid name collisions if a factory
	// reuses the same backing DB across the Backup suite and this suite.
	shareName, uniqueHashes := populateTestData(t, store, "rst")

	// 1. Back up the populated store into a buffer.
	var dumpBuf bytes.Buffer
	hs, err := b.Backup(ctx, &dumpBuf)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if hs.Len() != len(uniqueHashes) {
		t.Fatalf("Backup HashSet.Len() = %d, want %d", hs.Len(), len(uniqueHashes))
	}

	// 2. Reset the SAME store in place — no close/reopen.
	if err := r.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	// 3. Empty assertion: ListShares returns zero entries post-Reset.
	shares, err := store.ListShares(ctx)
	if err != nil {
		t.Fatalf("ListShares post-Reset: %v", err)
	}
	if len(shares) != 0 {
		t.Fatalf("post-Reset ListShares = %v, want empty", shares)
	}

	// 4. Restore from the same dump into the (now-empty) same store
	//    instance. The Reset above satisfied the
	//    ErrRestoreDestinationNotEmpty precondition so Restore must succeed.
	if err := b.Restore(ctx, &dumpBuf); err != nil {
		t.Fatalf("Restore post-Reset: %v", err)
	}

	// 5. Verify shares + representative file survived round-trip.
	restored, err := store.ListShares(ctx)
	if err != nil {
		t.Fatalf("ListShares post-Restore: %v", err)
	}
	found := false
	for _, s := range restored {
		if s == shareName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("share %q not found post-Restore (shares: %v)", shareName, restored)
	}

	rootHandle, err := store.GetRootHandle(ctx, shareName)
	if err != nil {
		t.Fatalf("GetRootHandle(%q): %v", shareName, err)
	}

	alphaHandle, err := store.GetChild(ctx, rootHandle, "alpha.bin")
	if err != nil {
		t.Fatalf("GetChild alpha.bin: %v", err)
	}
	alphaFile, err := store.GetFile(ctx, alphaHandle)
	if err != nil {
		t.Fatalf("GetFile alpha: %v", err)
	}
	if alphaFile.Size != 8<<20 {
		t.Errorf("alpha.Size = %d, want %d", alphaFile.Size, 8<<20)
	}
	if alphaFile.Mode != 0o644 {
		t.Errorf("alpha.Mode = %o, want %o", alphaFile.Mode, 0o644)
	}
	if len(alphaFile.Blocks) != 2 {
		t.Fatalf("alpha.Blocks len = %d, want 2", len(alphaFile.Blocks))
	}

	betaHandle, err := store.GetChild(ctx, rootHandle, "beta.bin")
	if err != nil {
		t.Fatalf("GetChild beta.bin: %v", err)
	}
	betaFile, err := store.GetFile(ctx, betaHandle)
	if err != nil {
		t.Fatalf("GetFile beta: %v", err)
	}
	if betaFile.Size != 6<<20 {
		t.Errorf("beta.Size = %d, want %d", betaFile.Size, 6<<20)
	}
	if len(betaFile.Blocks) != 2 {
		t.Fatalf("beta.Blocks len = %d, want 2", len(betaFile.Blocks))
	}
}
