//go:build integration

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func TestBlockStoreOperations(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	t.Run("create local block store", func(t *testing.T) {
		bs := &models.BlockStoreConfig{
			Name:   "test-local-fs",
			Type:   "fs",
			Config: `{"path":"/data/blocks"}`,
		}

		id, err := store.CreateBlockStore(ctx, bs)
		if err != nil {
			t.Fatalf("failed to create block store: %v", err)
		}
		if id == "" {
			t.Error("expected non-empty ID")
		}
	})

	t.Run("create remote block store", func(t *testing.T) {
		bs := &models.BlockStoreConfig{
			Name:   "test-remote-s3",
			Type:   "s3",
			Config: `{"bucket":"test-bucket"}`,
		}

		id, err := store.CreateBlockStore(ctx, bs)
		if err != nil {
			t.Fatalf("failed to create block store: %v", err)
		}
		if id == "" {
			t.Error("expected non-empty ID")
		}
	})

	t.Run("duplicate block store fails", func(t *testing.T) {
		bs := &models.BlockStoreConfig{
			Name: "test-local-fs",
			Type: "fs",
		}
		_, err := store.CreateBlockStore(ctx, bs)
		if !errors.Is(err, models.ErrDuplicateStore) {
			t.Errorf("expected ErrDuplicateStore, got %v", err)
		}
	})

	// A name identifies a block store on its own, so two stores may not share
	// one whatever their types. Two differently-typed stores both named
	// "default" used to be a supported pair; startup now refuses that database.
	t.Run("same name with a different type still collides", func(t *testing.T) {
		first := &models.BlockStoreConfig{
			Name: "default",
			Type: "memory",
		}
		if _, err := store.CreateBlockStore(ctx, first); err != nil {
			t.Fatalf("create 'default': %v", err)
		}

		second := &models.BlockStoreConfig{
			Name:   "default",
			Type:   "s3",
			Config: `{"bucket":"x"}`,
		}
		if _, err := store.CreateBlockStore(ctx, second); !errors.Is(err, models.ErrDuplicateStore) {
			t.Fatalf("create a second 'default': expected ErrDuplicateStore, got %v", err)
		}

		got, err := store.GetBlockStore(ctx, "default")
		if err != nil {
			t.Fatalf("get default: %v", err)
		}
		if got.Type != "memory" {
			t.Errorf("default: expected the first store to stand, type 'memory', got %q", got.Type)
		}

		if err := store.DeleteBlockStore(ctx, "default"); err != nil {
			t.Fatalf("delete default: %v", err)
		}
		if _, err := store.GetBlockStore(ctx, "default"); !errors.Is(err, models.ErrStoreNotFound) {
			t.Errorf("default should be gone after deletion, got %v", err)
		}
	})

	t.Run("get block store by name", func(t *testing.T) {
		bs, err := store.GetBlockStore(ctx, "test-local-fs")
		if err != nil {
			t.Fatalf("failed to get block store: %v", err)
		}
		if bs.Name != "test-local-fs" {
			t.Errorf("expected name 'test-local-fs', got %q", bs.Name)
		}
		if bs.Type != "fs" {
			t.Errorf("expected type 'fs', got %q", bs.Type)
		}
	})

	t.Run("get block store by ID", func(t *testing.T) {
		local, _ := store.GetBlockStore(ctx, "test-local-fs")
		bs, err := store.GetBlockStoreByID(ctx, local.ID)
		if err != nil {
			t.Fatalf("failed to get block store by ID: %v", err)
		}
		if bs.Name != "test-local-fs" {
			t.Errorf("expected name 'test-local-fs', got %q", bs.Name)
		}
	})

	t.Run("get block store by ID not found", func(t *testing.T) {
		_, err := store.GetBlockStoreByID(ctx, "nonexistent-id")
		if !errors.Is(err, models.ErrStoreNotFound) {
			t.Errorf("expected ErrStoreNotFound, got %v", err)
		}
	})

	t.Run("update block store", func(t *testing.T) {
		bs, _ := store.GetBlockStore(ctx, "test-local-fs")
		bs.Config = `{"path":"/new/path"}`

		err := store.UpdateBlockStore(ctx, bs)
		if err != nil {
			t.Fatalf("failed to update block store: %v", err)
		}

		updated, _ := store.GetBlockStore(ctx, "test-local-fs")
		if updated.Config != `{"path":"/new/path"}` {
			t.Errorf("expected updated config, got %q", updated.Config)
		}
	})

	t.Run("update nonexistent block store", func(t *testing.T) {
		bs := &models.BlockStoreConfig{ID: "nonexistent", Name: "x", Type: "fs"}
		err := store.UpdateBlockStore(ctx, bs)
		if !errors.Is(err, models.ErrStoreNotFound) {
			t.Errorf("expected ErrStoreNotFound, got %v", err)
		}
	})

	t.Run("delete block store", func(t *testing.T) {
		// Create a temporary store to delete
		bs := &models.BlockStoreConfig{
			Name: "to-delete",
			Type: "fs",
		}
		store.CreateBlockStore(ctx, bs)

		err := store.DeleteBlockStore(ctx, "to-delete")
		if err != nil {
			t.Fatalf("failed to delete block store: %v", err)
		}

		_, err = store.GetBlockStore(ctx, "to-delete")
		if !errors.Is(err, models.ErrStoreNotFound) {
			t.Error("block store should not exist after deletion")
		}
	})

	t.Run("delete nonexistent block store", func(t *testing.T) {
		err := store.DeleteBlockStore(ctx, "nonexistent")
		if !errors.Is(err, models.ErrStoreNotFound) {
			t.Errorf("expected ErrStoreNotFound, got %v", err)
		}
	})
}

func TestBlockStoreList(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	for _, name := range []string{"s3-1", "s3-2", "mem-1"} {
		store.CreateBlockStore(ctx, &models.BlockStoreConfig{
			Name: name, Type: "s3",
		})
	}

	stores, err := store.ListBlockStores(ctx)
	if err != nil {
		t.Fatalf("failed to list block stores: %v", err)
	}
	if len(stores) != 3 {
		t.Errorf("expected 3 block stores, got %d", len(stores))
	}
}

func TestShareBlockStore(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	// Create prerequisite stores
	meta := &models.MetadataStoreConfig{Name: "share-meta", Type: "memory"}
	metaID, _ := store.CreateMetadataStore(ctx, meta)

	blockStore := &models.BlockStoreConfig{Name: "share-blocks", Type: "s3"}
	blockStoreID, _ := store.CreateBlockStore(ctx, blockStore)

	t.Run("create share with a block store", func(t *testing.T) {
		share := &models.Share{
			Name:            "/test-share",
			MetadataStoreID: metaID,
			BlockStoreID:    blockStoreID,
		}

		id, err := store.CreateShare(ctx, share)
		if err != nil {
			t.Fatalf("failed to create share: %v", err)
		}
		if id == "" {
			t.Error("expected non-empty share ID")
		}
	})

	t.Run("get share loads the block store relationship", func(t *testing.T) {
		share, err := store.GetShare(ctx, "/test-share")
		if err != nil {
			t.Fatalf("failed to get share: %v", err)
		}

		if share.BlockStore.Name != "share-blocks" {
			t.Errorf("expected block store name 'share-blocks', got %q", share.BlockStore.Name)
		}
	})

	t.Run("get shares by block store", func(t *testing.T) {
		shares, err := store.GetSharesByBlockStore(ctx, "share-blocks")
		if err != nil {
			t.Fatalf("failed to get shares by block store: %v", err)
		}
		if len(shares) != 1 {
			t.Errorf("expected 1 share referencing share-blocks, got %d", len(shares))
		}
	})

	t.Run("delete block store in use fails", func(t *testing.T) {
		err := store.DeleteBlockStore(ctx, "share-blocks")
		if !errors.Is(err, models.ErrStoreInUse) {
			t.Errorf("expected ErrStoreInUse, got %v", err)
		}
	})
}
