//go:build integration

package badger_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/marmos91/dittofs/pkg/metadata/storetest"
)

// Compile-time assertion that *badger.BadgerMetadataStore satisfies the
// storetest.BackupTestStore union (MetadataStore + Backupable + io.Closer).
// A break here indicates drift between the driver and the shared
// conformance suite contract — fix the driver rather than the test.
var _ storetest.BackupTestStore = (*badger.BadgerMetadataStore)(nil)

// TestBackupConformance runs the shared Phase-2 backup/restore conformance
// suite against a fresh Badger store in a new t.TempDir(). The factory is
// called at least twice per sub-test (source + destination); each call
// produces an independent on-disk database so truncation, rollback, and
// cross-wave contamination are impossible between sub-tests.
func TestBackupConformance(t *testing.T) {
	storetest.RunBackupConformanceSuite(t, func(t *testing.T) storetest.BackupTestStore {
		dbPath := filepath.Join(t.TempDir(), "metadata.db")
		store, err := badger.NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("NewBadgerMetadataStoreWithDefaults() failed: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	})
}

// TestBadgerStoreID_PersistedAcrossRestart runs the Phase 5 D-06
// conformance check: opening the SAME Badger directory twice returns the
// same store_id. Pins the invariant that a control-plane DB reset (which
// rotates cfg.ID) does NOT re-identify the engine — the ULID is bound to
// the data directory itself.
//
// The dbPath is declared ONCE in the outer scope; both factory calls point
// at the same directory so close+reopen exercises persistence rather than
// fresh-instance creation.
func TestBadgerStoreID_PersistedAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	storetest.TestStoreID_PersistedAcrossRestart(t, func(t *testing.T) metadata.MetadataStore {
		store, err := badger.NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("open badger at %s: %v", dbPath, err)
		}
		// Each call leaves the store open; the conformance helper calls
		// Close on the first instance before opening the second.
		return store
	})
}

// TestBadgerStoreID_PreservedAcrossRestore runs the Phase 5 D-06
// "receiver identity wins" conformance check. A fresh destination Badger
// keeps its own store_id after Restore — the cfg:store_id key is re-anchored
// to the receiver's ULID at the end of Restore.
func TestBadgerStoreID_PreservedAcrossRestore(t *testing.T) {
	factory := func(t *testing.T) storetest.BackupTestStore {
		dbPath := filepath.Join(t.TempDir(), "metadata.db")
		store, err := badger.NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("NewBadgerMetadataStoreWithDefaults() failed: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
	storetest.TestStoreID_PreservedAcrossRestore(t, factory, factory)
}
