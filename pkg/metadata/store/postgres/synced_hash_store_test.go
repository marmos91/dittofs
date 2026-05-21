//go:build integration

package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
)

// TestPostgresSyncedHashStore_Suite exercises the shared SyncedHashStore
// conformance suite against the Postgres backend. Requires a live
// PostgreSQL at DITTOFS_TEST_POSTGRES_DSN (same env-var gate as the
// other Postgres integration tests). Idempotency is enforced by
// ON CONFLICT DO NOTHING — see pkg/metadata/store/postgres/synced_hash_store.go.
//
// State isolation across runs: the conformance suite picks distinct
// hash seeds per subtest (via mustHash), and every subtest's
// assertions are idempotent against rows left by prior runs (Mark is
// idempotent; Delete is idempotent; IsSyncedBeforeMark uses a seed
// that no other subtest mutates). No table truncation is required;
// this mirrors the rollup_store sibling test exactly.
func TestPostgresSyncedHashStore_Suite(t *testing.T) {
	if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL synced-hash tests")
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

	metadata.RunSyncedHashStoreSuite(t, store)
}
