//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestPostgres_CreateShare_MaterializesRootAndOptions pins the fix for the
// CreateShare contract violation. The shares.root_file_id column is NOT NULL
// (FK to files), so the previous bare INSERT INTO shares (...) could never
// succeed — it always raised a not_null_violation. The store interface
// documents CreateShare as "Also creates the root directory for the share"
// (the memory/badger backends do), so postgres must materialize the root and
// persist the caller's options. This was invisible because CI never ran the
// postgres integration suite.
func TestPostgres_CreateShare_MaterializesRootAndOptions(t *testing.T) {
	if os.Getenv("DITTOFS_TEST_POSTGRES_DSN") == "" {
		t.Skip("DITTOFS_TEST_POSTGRES_DSN not set, skipping PostgreSQL CreateShare test")
	}

	store := newPostgresStore(t)
	ctx := context.Background()

	// Clean slate so the fixed share name does not collide with prior runs.
	if err := store.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	const shareName = "/cs-contract-test"
	share := &metadata.Share{
		Name:    shareName,
		Options: metadata.ShareOptions{ReadOnly: true},
	}

	// CreateShare must succeed (the bug: it always failed on root_file_id).
	if err := store.CreateShare(ctx, share); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	// The root directory must exist (contract: "Also creates the root
	// directory for the share").
	if _, err := store.GetRootHandle(ctx, shareName); err != nil {
		t.Fatalf("GetRootHandle after CreateShare: root not materialized: %v", err)
	}

	// The caller's options must be persisted, not left at column defaults.
	opts, err := store.GetShareOptions(ctx, shareName)
	if err != nil {
		t.Fatalf("GetShareOptions: %v", err)
	}
	if !opts.ReadOnly {
		t.Fatal("CreateShare did not persist Options.ReadOnly=true")
	}

	// A second CreateShare for the same name must report ErrAlreadyExists.
	err = store.CreateShare(ctx, share)
	var se *metadata.StoreError
	if !errors.As(err, &se) || se.Code != metadata.ErrAlreadyExists {
		t.Fatalf("duplicate CreateShare: got %v, want StoreError{Code: ErrAlreadyExists}", err)
	}
}
