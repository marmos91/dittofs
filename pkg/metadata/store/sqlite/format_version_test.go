package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
)

// migratedDBPath creates a database at the current schema and returns its path.
func migratedDBPath(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "format.db")
	cfg := &sqlite.SQLiteMetadataStoreConfig{Path: dbPath, AutoMigrate: true}
	store, err := sqlite.NewSQLiteMetadataStore(context.Background(), cfg, sqliteTestCapabilities())
	if err != nil {
		t.Fatalf("NewSQLiteMetadataStore() failed: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}
	return dbPath
}

// recordFutureMigration records a schema version this build does not ship,
// standing in for a database a newer release migrated.
func recordFutureMigration(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`INSERT INTO schema_migrations (version) VALUES (999999)`); err != nil {
		t.Fatalf("insert future version: %v", err)
	}
}

// TestFormatVersion_CurrentSchemaOpens pins that a database at the version this
// build migrated it to reopens without tripping the guard.
func TestFormatVersion_CurrentSchemaOpens(t *testing.T) {
	dbPath := migratedDBPath(t)

	cfg := &sqlite.SQLiteMetadataStoreConfig{Path: dbPath, AutoMigrate: true}
	store, err := sqlite.NewSQLiteMetadataStore(context.Background(), cfg, sqliteTestCapabilities())
	if err != nil {
		t.Fatalf("reopen at current schema version: %v", err)
	}
	_ = store.Close()
}

// TestFormatVersion_FutureSchemaRefusesOpen is the downgrade case, asserted on
// both AutoMigrate settings. The guard is a safety check rather than a
// migration, so the default (AutoMigrate off) configuration — which otherwise
// inspects the schema not at all — must refuse just as loudly.
func TestFormatVersion_FutureSchemaRefusesOpen(t *testing.T) {
	for _, autoMigrate := range []bool{true, false} {
		name := "auto_migrate_off"
		if autoMigrate {
			name = "auto_migrate_on"
		}
		t.Run(name, func(t *testing.T) {
			dbPath := migratedDBPath(t)
			recordFutureMigration(t, dbPath)

			cfg := &sqlite.SQLiteMetadataStoreConfig{Path: dbPath, AutoMigrate: autoMigrate}
			store, err := sqlite.NewSQLiteMetadataStore(context.Background(), cfg, sqliteTestCapabilities())
			if err == nil {
				_ = store.Close()
				t.Fatal("opening a future schema must fail")
			}
			if !errors.Is(err, block.ErrFutureFormat) {
				t.Fatalf("error must wrap block.ErrFutureFormat; got %v", err)
			}
		})
	}
}

// TestFormatVersion_FutureSchemaRefusesMigration pins that the manual migration
// entrypoint refuses too, instead of reporting the database as up to date.
func TestFormatVersion_FutureSchemaRefusesMigration(t *testing.T) {
	dbPath := migratedDBPath(t)
	recordFutureMigration(t, dbPath)

	cfg := &sqlite.SQLiteMetadataStoreConfig{Path: dbPath}
	err := sqlite.RunMigrations(context.Background(), cfg)
	if !errors.Is(err, block.ErrFutureFormat) {
		t.Fatalf("error must wrap block.ErrFutureFormat; got %v", err)
	}
}
