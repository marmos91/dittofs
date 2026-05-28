//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

func TestReset_Postgres_Empty(t *testing.T) {
	store := newTestStore(t)
	r, ok := any(store).(metadata.Resetable)
	if !ok {
		t.Fatal("*PostgresMetadataStore must implement metadata.Resetable")
	}
	if err := r.Reset(t.Context()); err != nil {
		t.Fatalf("Reset on empty store: %v", err)
	}
}

func TestReset_Postgres_Populated(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()

	const shareName = "/reset-pop-pg"
	if _, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	}); err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}

	r := any(store).(metadata.Resetable)
	if err := r.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	// Every backupTables-listed table must be empty after Reset.
	shares, err := store.ListShares(ctx)
	if err != nil {
		t.Fatalf("ListShares: %v", err)
	}
	if len(shares) != 0 {
		t.Fatalf("ListShares = %v, want empty after Reset", shares)
	}
}

func TestReset_Postgres_HandleReusable(t *testing.T) {
	// Same *pgx.Pool stays usable across Reset.
	store := newTestStore(t)
	ctx := t.Context()

	if _, err := store.CreateRootDirectory(ctx, "/before-pg", &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	}); err != nil {
		t.Fatalf("CreateRootDirectory first: %v", err)
	}

	r := any(store).(metadata.Resetable)
	if err := r.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if _, err := store.CreateRootDirectory(ctx, "/after-pg", &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	}); err != nil {
		t.Fatalf("CreateRootDirectory after Reset (handle reuse): %v", err)
	}
}

func TestReset_Postgres_CtxCancellation(t *testing.T) {
	store := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := any(store).(metadata.Resetable)
	err := r.Reset(ctx)
	if err == nil {
		t.Fatal("Reset with cancelled ctx: err = nil, want non-nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Reset with cancelled ctx: err = %v, want errors.Is(context.Canceled)", err)
	}
}
