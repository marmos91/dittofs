//go:build integration

package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
	"github.com/marmos91/dittofs/pkg/metadata/storetest"
)

// postgresTestConfig returns the config + capabilities every Postgres test
// store is opened with. Centralized so the connection params stay consistent.
func postgresTestConfig() (*postgres.PostgresMetadataStoreConfig, metadata.FilesystemCapabilities) {
	dbName := os.Getenv("DITTOFS_TEST_PG_DBNAME")
	if dbName == "" {
		dbName = "dittofs_test"
	}
	cfg := &postgres.PostgresMetadataStoreConfig{
		Host:        "localhost",
		Port:        5432,
		Database:    dbName,
		User:        "postgres",
		Password:    "postgres",
		SSLMode:     "disable",
		AutoMigrate: true,
	}
	caps := metadata.FilesystemCapabilities{
		MaxReadSize:         1048576,
		PreferredReadSize:   1048576,
		MaxWriteSize:        1048576,
		PreferredWriteSize:  1048576,
		MaxFileSize:         9223372036854775807,
		MaxFilenameLen:      255,
		MaxPathLen:          4096,
		MaxHardLinkCount:    32767,
		SupportsHardLinks:   true,
		SupportsSymlinks:    true,
		CaseSensitive:       true,
		CasePreserving:      true,
		TimestampResolution: 1,
	}
	return cfg, caps
}

// newPostgresStore opens a Postgres store WITHOUT resetting it. Used by
// dedicated tests that manage their own data lifecycle (store_id stability,
// CreateShare contract). The store is closed via t.Cleanup.
func newPostgresStore(t *testing.T) *postgres.PostgresMetadataStore {
	t.Helper()
	cfg, caps := postgresTestConfig()
	store, err := postgres.NewPostgresMetadataStore(context.Background(), cfg, caps)
	if err != nil {
		t.Fatalf("NewPostgresMetadataStore() failed: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// newPostgresStoreFactory returns a factory that hands each conformance
// subtest a CLEAN store. The conformance subtests share one database and
// reuse fixed share names ("/test"), so per-subtest isolation is mandatory:
// open to ensure the schema is migrated, Reset to truncate any data a prior
// subtest left behind, then re-open. The re-open re-seeds
// filesystem_capabilities and server_config (store_id) on the now-empty
// singleton tables, exercising the post-reset reopen path the restore
// orchestration depends on.
func newPostgresStoreFactory() func(t *testing.T) metadata.MetadataStore {
	return func(t *testing.T) metadata.MetadataStore {
		t.Helper()
		cfg, caps := postgresTestConfig()
		ctx := context.Background()

		seed, err := postgres.NewPostgresMetadataStore(ctx, cfg, caps)
		if err != nil {
			t.Fatalf("NewPostgresMetadataStore() failed: %v", err)
		}
		if err := seed.Reset(ctx); err != nil {
			seed.Close()
			t.Fatalf("Reset() failed: %v", err)
		}
		seed.Close()

		store, err := postgres.NewPostgresMetadataStore(ctx, cfg, caps)
		if err != nil {
			t.Fatalf("NewPostgresMetadataStore() reopen failed: %v", err)
		}
		t.Cleanup(func() { store.Close() })
		return store
	}
}

func TestConformance(t *testing.T) {
	if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL conformance tests")
	}

	storetest.RunConformanceSuite(t, newPostgresStoreFactory())
}

func TestBackupConformance(t *testing.T) {
	if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL backup conformance tests")
	}

	storetest.RunBackupConformanceSuite(t, newPostgresStoreFactory())
}

func TestResetThenRestoreConformance(t *testing.T) {
	if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL reset-then-restore conformance tests")
	}

	storetest.ResetThenRestoreConformance(t, newPostgresStoreFactory())
}
