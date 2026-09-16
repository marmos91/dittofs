//go:build integration

package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
	"github.com/marmos91/dittofs/pkg/metadata/storetest"
)

// TestPostgresSyncedHashStore_Suite exercises the shared SyncedHashStore
// conformance suite against the Postgres backend. DITTOFS_TEST_POSTGRES_DSN
// is a BOOLEAN GATE — any non-empty value opts in; the value itself is
// NOT parsed as a DSN. The connection parameters come from
// postgresTestConfig, matching the CI Postgres service container the other
// postgres_test.go files connect to. Idempotency is enforced by ON CONFLICT
// DO NOTHING — see pkg/metadata/store/postgres/synced_hash_store.go.
//
// State isolation across runs: the conformance suite picks distinct hash
// seeds per subtest and scopes them to the run, so no assertion turns on
// the synced_hashes rows an earlier run against the same database left
// behind — EnumerateSynced still scans them, it just never matches one.
// No table truncation is required.
func TestPostgresSyncedHashStore_Suite(t *testing.T) {
	if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL synced-hash tests")
	}

	cfg, caps := postgresTestConfig()

	store, err := postgres.NewPostgresMetadataStore(context.Background(), cfg, caps)
	if err != nil {
		t.Fatalf("NewPostgresMetadataStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	storetest.RunSyncedHashStoreSuite(t, store)
	storetest.RunSyncedHashEnumeratorSuite(t, store)
}
