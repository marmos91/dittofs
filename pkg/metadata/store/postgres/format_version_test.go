//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
)

// recordFutureMigration records a schema version this build does not ship,
// standing in for a database a newer release migrated. The row is removed on
// cleanup — every Postgres test shares one database, so a leftover would make
// each later open refuse.
func recordFutureMigration(t *testing.T) {
	t.Helper()
	cfg, _ := postgresTestConfig()
	cfg.ApplyDefaults()

	db, err := sql.Open("pgx", cfg.ConnectionString())
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM schema_migrations WHERE version = 999999`)
		_ = db.Close()
	})

	if _, err := db.Exec(
		`INSERT INTO schema_migrations (version, dirty) VALUES (999999, false)`,
	); err != nil {
		t.Fatalf("insert future version: %v", err)
	}
}

// TestFormatVersion_CurrentSchemaOpens pins that a database at the version this
// build migrated it to opens without tripping the guard.
func TestFormatVersion_CurrentSchemaOpens(t *testing.T) {
	store := newPostgresStore(t)
	if store == nil {
		t.Fatal("store must open at the current schema version")
	}
}

// TestFormatVersion_FutureSchemaRefusesOpen is the downgrade case, asserted on
// both AutoMigrate settings. The guard is a safety check rather than a
// migration, so the default (AutoMigrate off) configuration — which otherwise
// inspects the schema not at all — must refuse just as loudly.
func TestFormatVersion_FutureSchemaRefusesOpen(t *testing.T) {
	// Ensure the schema (and schema_migrations) exists before recording into it.
	newPostgresStore(t)

	for _, autoMigrate := range []bool{true, false} {
		name := "auto_migrate_off"
		if autoMigrate {
			name = "auto_migrate_on"
		}
		t.Run(name, func(t *testing.T) {
			recordFutureMigration(t)

			cfg, caps := postgresTestConfig()
			cfg.AutoMigrate = autoMigrate
			store, err := postgres.NewPostgresMetadataStore(context.Background(), cfg, caps)
			if err == nil {
				store.Close()
				t.Fatal("opening a future schema must fail")
			}
			if !errors.Is(err, block.ErrFutureFormat) {
				t.Fatalf("error must wrap block.ErrFutureFormat; got %v", err)
			}
		})
	}
}
