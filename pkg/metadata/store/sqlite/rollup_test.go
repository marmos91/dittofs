package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
)

// TestSQLiteRollupStore_Suite runs the shared RollupStore conformance suite
// against the SQLite backend, so every backend (memory, badger, postgres,
// sqlite) exercises the same contract from a single source of truth —
// including the newOffset==0 unconditional-reset behavior.
func TestSQLiteRollupStore_Suite(t *testing.T) {
	cfg := &sqlite.SQLiteMetadataStoreConfig{
		Path:        filepath.Join(t.TempDir(), "rollup.db"),
		AutoMigrate: true,
	}
	store, err := sqlite.NewSQLiteMetadataStore(context.Background(), cfg, sqliteTestCapabilities())
	if err != nil {
		t.Fatalf("NewSQLiteMetadataStore() failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	metadata.RunRollupStoreSuite(t, store)
}
