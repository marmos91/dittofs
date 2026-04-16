//go:build integration

package badger_test

import (
	"context"
	"path/filepath"
	"testing"

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
