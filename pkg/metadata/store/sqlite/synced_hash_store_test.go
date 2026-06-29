package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
)

// newSQLiteSyncedTestStore builds a fresh, migrated SQLite store for the
// synced-hash suites. The concrete *sqlite.SQLiteMetadataStore satisfies both
// the SyncedHashStore contract and the LIST-free EnumerateSynced capability.
func newSQLiteSyncedTestStore(t *testing.T) *sqlite.SQLiteMetadataStore {
	t.Helper()
	cfg := &sqlite.SQLiteMetadataStoreConfig{
		Path:        filepath.Join(t.TempDir(), "synced.db"),
		AutoMigrate: true,
	}
	store, err := sqlite.NewSQLiteMetadataStore(context.Background(), cfg, sqliteTestCapabilities())
	if err != nil {
		t.Fatalf("NewSQLiteMetadataStore() failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestSQLiteSyncedHashStore_Suite runs the shared SyncedHashStore conformance
// suite against the SQLite backend.
func TestSQLiteSyncedHashStore_Suite(t *testing.T) {
	metadata.RunSyncedHashStoreSuite(t, newSQLiteSyncedTestStore(t))
}

// TestSQLiteSyncedHashEnumerator_Suite exercises the LIST-free GC sweep's
// EnumerateSynced contract against the SQLite backend, covering the
// synced_at TIMESTAMP scan into time.Time.
func TestSQLiteSyncedHashEnumerator_Suite(t *testing.T) {
	metadata.RunSyncedHashEnumeratorSuite(t, newSQLiteSyncedTestStore(t))
}
