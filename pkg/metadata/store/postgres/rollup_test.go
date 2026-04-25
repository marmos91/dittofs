//go:build integration

package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
)

// TestPostgresRollupStore_Suite exercises the shared RollupStore conformance
// suite against the Postgres backend. Requires a live PostgreSQL at
// DITTOFS_TEST_POSTGRES_DSN (the same env-var gate used by the Postgres
// conformance suite). Atomic-monotone semantics (INV-03) are enforced by
// the ON CONFLICT ... WHERE clause — see pkg/metadata/store/postgres/rollup.go.
func TestPostgresRollupStore_Suite(t *testing.T) {
	if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL rollup tests")
	}

	cfg := &postgres.PostgresMetadataStoreConfig{
		Host:        "localhost",
		Port:        5432,
		Database:    "dittofs_test",
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

	store, err := postgres.NewPostgresMetadataStore(context.Background(), cfg, caps)
	if err != nil {
		t.Fatalf("NewPostgresMetadataStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	metadata.RunRollupStoreSuite(t, store)
}
