//go:build integration

package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestPostgresRollupStore_Suite exercises the shared RollupStore conformance
// suite against the Postgres backend. Requires a live PostgreSQL at
// DITTOFS_TEST_POSTGRES_DSN (the same env-var gate used by the Postgres
// conformance suite). Atomic-monotone semantics are enforced by reading the
// prior offset under a FOR UPDATE row lock — see SetRollupOffset in
// pkg/metadata/store/postgres/rollup.go.
func TestPostgresRollupStore_Suite(t *testing.T) {
	if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL rollup tests")
	}

	// Reset first: this package's tests share one database, and the suite
	// uses fixed payload IDs — start from a known-empty rollup_offsets table.
	store := newPostgresStore(t)
	if err := store.Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	metadata.RunRollupStoreSuite(t, store)
}
