//go:build integration

package badger_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

func newBadgerStoreForReset(t *testing.T) *badger.BadgerMetadataStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	store, err := badger.NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestReset_Empty(t *testing.T) {
	store := newBadgerStoreForReset(t)
	r, ok := any(store).(metadata.Resetable)
	if !ok {
		t.Fatal("*BadgerMetadataStore must implement metadata.Resetable")
	}
	if err := r.Reset(t.Context()); err != nil {
		t.Fatalf("Reset on empty store: %v", err)
	}
	shares, err := store.ListShares(t.Context())
	if err != nil {
		t.Fatalf("ListShares: %v", err)
	}
	if len(shares) != 0 {
		t.Fatalf("ListShares = %v, want empty", shares)
	}
}

func TestReset_Populated(t *testing.T) {
	store := newBadgerStoreForReset(t)
	ctx := t.Context()

	const shareName = "/reset-populated"
	if err := store.CreateShare(ctx, &metadata.Share{Name: shareName}); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	rootAttr := &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755}
	if _, err := store.CreateRootDirectory(ctx, shareName, rootAttr); err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}

	r := any(store).(metadata.Resetable)
	if err := r.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	shares, err := store.ListShares(ctx)
	if err != nil {
		t.Fatalf("ListShares: %v", err)
	}
	if len(shares) != 0 {
		t.Fatalf("ListShares = %v, want empty after Reset", shares)
	}
}

func TestReset_HandleReusable(t *testing.T) {
	// Verifies that the same *badger.DB handle stays valid after Reset
	// (DropAll preserves the live handle per Badger's documented contract).
	store := newBadgerStoreForReset(t)
	ctx := t.Context()

	if err := store.CreateShare(ctx, &metadata.Share{Name: "/before"}); err != nil {
		t.Fatalf("CreateShare first: %v", err)
	}

	r := any(store).(metadata.Resetable)
	if err := r.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if err := store.CreateShare(ctx, &metadata.Share{Name: "/after"}); err != nil {
		t.Fatalf("CreateShare after Reset (handle reuse): %v", err)
	}
	shares, err := store.ListShares(ctx)
	if err != nil {
		t.Fatalf("ListShares: %v", err)
	}
	if len(shares) != 1 || shares[0] != "/after" {
		t.Fatalf("post-reset ListShares = %v, want [%q]", shares, "/after")
	}
}

func TestReset_CtxCancellation(t *testing.T) {
	store := newBadgerStoreForReset(t)
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
