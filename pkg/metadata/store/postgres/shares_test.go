//go:build integration

package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/storetest"
)

// TestBlockLayoutConformance runs the per-share BlockLayout conformance
// scenarios against the Postgres metadata store. Gated by
// DITTOFS_TEST_POSTGRES_DSN matching the existing convention
// (postgres_conformance_test.go) — skip cleanly outside the dedicated
// CI lane.
func TestBlockLayoutConformance(t *testing.T) {
	connStr := os.Getenv("DITTOFS_TEST_POSTGRES_DSN")
	if connStr == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL BlockLayout conformance")
	}

	// Per-subtest clean store: the suite uses fixed share names and this
	// package's tests share one database, so reset each store on open.
	storetest.RunBlockLayoutSuite(t, func(t *testing.T) metadata.MetadataStore {
		store := newPostgresStore(t)
		if err := store.Reset(context.Background()); err != nil {
			t.Fatalf("Reset: %v", err)
		}
		return store
	})
}
