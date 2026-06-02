//go:build integration

package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestPostgres_StoreID_SurvivesResetAndReopen pins the fix for the store-open
// deadlock: Reset (and the pre-COPY truncate inside Restore) clears
// server_config, including the singleton id=1 row. AutoMigrate does not
// re-seed it on a reopen (migration 000001 is already recorded), so the
// store-bootstrap ensureStoreID step must recreate the row itself. The
// previous UPDATE-only form matched zero rows and failed every reopen with
// "no rows in result set" — which bricked the store after a Reset/restore
// crash and, fatally, blocked the boot-time restore-marker recovery from
// ever opening the store to run.
func TestPostgres_StoreID_SurvivesResetAndReopen(t *testing.T) {
	if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL store_id test")
	}

	ctx := context.Background()

	// First open seeds id=1 with a fresh store_id.
	a := newPostgresStore(t)
	sid1 := storeID(t, a)
	if sid1 == "" {
		t.Fatal("first open: store_id is empty, want a generated ULID")
	}

	// Reset truncates every table, including server_config — the exact
	// production trigger (restore orchestration resets metadata before COPY).
	if err := a.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	// Reopen MUST succeed (the bug: it failed here) and re-seed a store_id.
	// newPostgresStore calls t.Fatalf internally if the open errors.
	b := newPostgresStore(t)
	sid2 := storeID(t, b)
	if sid2 == "" {
		t.Fatal("reopen after reset: store_id is empty, want a freshly generated ULID")
	}

	// A subsequent reopen must preserve the now-seeded value (COALESCE/NULLIF
	// keeps a non-empty store_id stable across opens).
	c := newPostgresStore(t)
	sid3 := storeID(t, c)
	if sid3 != sid2 {
		t.Fatalf("store_id not stable across reopen: got %q, want %q", sid3, sid2)
	}
}

// storeID extracts the engine-persistent store identifier via the
// GetStoreID() surface the Postgres engine exposes.
func storeID(t *testing.T, store metadata.Store) string {
	t.Helper()
	getter, ok := store.(interface{ GetStoreID() string })
	if !ok {
		t.Fatalf("store does not expose GetStoreID()")
	}
	return getter.GetStoreID()
}
