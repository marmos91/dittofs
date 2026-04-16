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
			Kind:   models.BlockStoreKindLocal,
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
			Kind:   models.BlockStoreKindRemote,
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

	t.Run("create without kind fails", func(t *testing.T) {
		bs := &models.BlockStoreConfig{
			Name: "no-kind",
			Type: "memory",
		}
		_, err := store.CreateBlockStore(ctx, bs)
		if err == nil {
			t.Error("expected error for missing kind")
		}
	})

	t.Run("duplicate block store fails", func(t *testing.T) {
		bs := &models.BlockStoreConfig{
			Name: "test-local-fs",
			Kind: models.BlockStoreKindLocal,
			Type: "fs",
		}
		_, err := store.CreateBlockStore(ctx, bs)
		if !errors.Is(err, models.ErrDuplicateStore) {
			t.Errorf("expected ErrDuplicateStore, got %v", err)
		}
	})

	// Regression for #376: WPTS BVT bootstrap creates a local and remote
	// block store both named "default"; uniqueness must be scoped by (name, kind).
	t.Run("same name across local and remote kinds succeeds", func(t *testing.T) {
		local := &models.BlockStoreConfig{
			Name: "default",
			Kind: models.BlockStoreKindLocal,
			Type: "memory",
		}
		if _, err := store.CreateBlockStore(ctx, local); err != nil {
			t.Fatalf("create local 'default': %v", err)
		}

		remote := &models.BlockStoreConfig{
			Name:   "default",
			Kind:   models.BlockStoreKindRemote,
			Type:   "s3",
			Config: `{"bucket":"x"}`,
		}
		if _, err := store.CreateBlockStore(ctx, remote); err != nil {
			t.Fatalf("create remote 'default' should succeed alongside local: %v", err)
		}

		gotLocal, err := store.GetBlockStore(ctx, "default", models.BlockStoreKindLocal)
		if err != nil {
			t.Fatalf("get local default: %v", err)
		}
		if gotLocal.Type != "memory" {
			t.Errorf("local default: expected type 'memory', got %q", gotLocal.Type)
		}

		gotRemote, err := store.GetBlockStore(ctx, "default", models.BlockStoreKindRemote)
		if err != nil {
			t.Fatalf("get remote default: %v", err)
		}
		if gotRemote.Type != "s3" {
			t.Errorf("remote default: expected type 's3', got %q", gotRemote.Type)
		}
	})

	t.Run("get block store by name and kind", func(t *testing.T) {
		bs, err := store.GetBlockStore(ctx, "test-local-fs", models.BlockStoreKindLocal)
		if err != nil {
			t.Fatalf("failed to get block store: %v", err)
		}
		if bs.Name != "test-local-fs" {
			t.Errorf("expected name 'test-local-fs', got %q", bs.Name)
		}
		if bs.Kind != models.BlockStoreKindLocal {
			t.Errorf("expected kind 'local', got %q", bs.Kind)
		}
		if bs.Type != "fs" {
			t.Errorf("expected type 'fs', got %q", bs.Type)
		}
	})

	t.Run("get block store wrong kind returns not found", func(t *testing.T) {
		_, err := store.GetBlockStore(ctx, "test-local-fs", models.BlockStoreKindRemote)
		if !errors.Is(err, models.ErrStoreNotFound) {
			t.Errorf("expected ErrStoreNotFound, got %v", err)
		}
	})

	t.Run("get block store by ID", func(t *testing.T) {
		local, _ := store.GetBlockStore(ctx, "test-local-fs", models.BlockStoreKindLocal)
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
		bs, _ := store.GetBlockStore(ctx, "test-local-fs", models.BlockStoreKindLocal)
		bs.Config = `{"path":"/new/path"}`

		err := store.UpdateBlockStore(ctx, bs)
		if err != nil {
			t.Fatalf("failed to update block store: %v", err)
		}

		updated, _ := store.GetBlockStore(ctx, "test-local-fs", models.BlockStoreKindLocal)
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
			Kind: models.BlockStoreKindLocal,
			Type: "fs",
		}
		store.CreateBlockStore(ctx, bs)

		err := store.DeleteBlockStore(ctx, "to-delete", models.BlockStoreKindLocal)
		if err != nil {
			t.Fatalf("failed to delete block store: %v", err)
		}

		_, err = store.GetBlockStore(ctx, "to-delete", models.BlockStoreKindLocal)
		if !errors.Is(err, models.ErrStoreNotFound) {
			t.Error("block store should not exist after deletion")
		}
	})

	t.Run("delete nonexistent block store", func(t *testing.T) {
		err := store.DeleteBlockStore(ctx, "nonexistent", models.BlockStoreKindLocal)
		if !errors.Is(err, models.ErrStoreNotFound) {
			t.Errorf("expected ErrStoreNotFound, got %v", err)
		}
	})
}

func TestBlockStoreKindFilter(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	// Create local stores
	for _, name := range []string{"local-1", "local-2"} {
		store.CreateBlockStore(ctx, &models.BlockStoreConfig{
			Name: name, Kind: models.BlockStoreKindLocal, Type: "fs",
		})
	}

	// Create remote stores
	for _, name := range []string{"remote-1", "remote-2", "remote-3"} {
		store.CreateBlockStore(ctx, &models.BlockStoreConfig{
			Name: name, Kind: models.BlockStoreKindRemote, Type: "s3",
		})
	}

	t.Run("list local only", func(t *testing.T) {
		stores, err := store.ListBlockStores(ctx, models.BlockStoreKindLocal)
		if err != nil {
			t.Fatalf("failed to list local stores: %v", err)
		}
		if len(stores) != 2 {
			t.Errorf("expected 2 local stores, got %d", len(stores))
		}
		for _, s := range stores {
			if s.Kind != models.BlockStoreKindLocal {
				t.Errorf("expected kind 'local', got %q", s.Kind)
			}
		}
	})

	t.Run("list remote only", func(t *testing.T) {
		stores, err := store.ListBlockStores(ctx, models.BlockStoreKindRemote)
		if err != nil {
			t.Fatalf("failed to list remote stores: %v", err)
		}
		if len(stores) != 3 {
			t.Errorf("expected 3 remote stores, got %d", len(stores))
		}
		for _, s := range stores {
			if s.Kind != models.BlockStoreKindRemote {
				t.Errorf("expected kind 'remote', got %q", s.Kind)
			}
		}
	})
}

func TestShareBlockStore(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	// Create prerequisite stores
	meta := &models.MetadataStoreConfig{Name: "share-meta", Type: "memory"}
	metaID, _ := store.CreateMetadataStore(ctx, meta)

	local := &models.BlockStoreConfig{Name: "share-local", Kind: models.BlockStoreKindLocal, Type: "fs"}
	localID, _ := store.CreateBlockStore(ctx, local)

	remote := &models.BlockStoreConfig{Name: "share-remote", Kind: models.BlockStoreKindRemote, Type: "s3"}
	remoteID, _ := store.CreateBlockStore(ctx, remote)

	t.Run("create share with local and remote block stores", func(t *testing.T) {
		share := &models.Share{
			Name:               "/test-share",
			MetadataStoreID:    metaID,
			LocalBlockStoreID:  localID,
			RemoteBlockStoreID: &remoteID,
		}

		id, err := store.CreateShare(ctx, share)
		if err != nil {
			t.Fatalf("failed to create share: %v", err)
		}
		if id == "" {
			t.Error("expected non-empty share ID")
		}
	})

	t.Run("create share with local only", func(t *testing.T) {
		share := &models.Share{
			Name:              "/local-only-share",
			MetadataStoreID:   metaID,
			LocalBlockStoreID: localID,
		}

		id, err := store.CreateShare(ctx, share)
		if err != nil {
			t.Fatalf("failed to create local-only share: %v", err)
		}
		if id == "" {
			t.Error("expected non-empty share ID")
		}
	})

	t.Run("get share loads block store relationships", func(t *testing.T) {
		share, err := store.GetShare(ctx, "/test-share")
		if err != nil {
			t.Fatalf("failed to get share: %v", err)
		}

		if share.LocalBlockStore.Name != "share-local" {
			t.Errorf("expected local block store name 'share-local', got %q", share.LocalBlockStore.Name)
		}
		if share.RemoteBlockStore == nil {
			t.Fatal("expected remote block store to be preloaded")
		}
		if share.RemoteBlockStore.Name != "share-remote" {
			t.Errorf("expected remote block store name 'share-remote', got %q", share.RemoteBlockStore.Name)
		}
	})

	t.Run("get local-only share has nil remote", func(t *testing.T) {
		share, err := store.GetShare(ctx, "/local-only-share")
		if err != nil {
			t.Fatalf("failed to get share: %v", err)
		}

		if share.LocalBlockStore.Name != "share-local" {
			t.Errorf("expected local block store name 'share-local', got %q", share.LocalBlockStore.Name)
		}
		if share.RemoteBlockStoreID != nil {
			t.Errorf("expected nil remote block store ID, got %v", *share.RemoteBlockStoreID)
		}
	})

	t.Run("get shares by block store", func(t *testing.T) {
		shares, err := store.GetSharesByBlockStore(ctx, "share-local", models.BlockStoreKindLocal)
		if err != nil {
			t.Fatalf("failed to get shares by block store: %v", err)
		}
		if len(shares) != 2 {
			t.Errorf("expected 2 shares referencing share-local, got %d", len(shares))
		}
	})

	t.Run("get shares by remote block store", func(t *testing.T) {
		shares, err := store.GetSharesByBlockStore(ctx, "share-remote", models.BlockStoreKindRemote)
		if err != nil {
			t.Fatalf("failed to get shares by block store: %v", err)
		}
		if len(shares) != 1 {
			t.Errorf("expected 1 share referencing share-remote, got %d", len(shares))
		}
	})

	t.Run("delete block store in use fails", func(t *testing.T) {
		err := store.DeleteBlockStore(ctx, "share-local", models.BlockStoreKindLocal)
		if !errors.Is(err, models.ErrStoreInUse) {
			t.Errorf("expected ErrStoreInUse, got %v", err)
		}
	})

	t.Run("delete remote block store in use fails", func(t *testing.T) {
		err := store.DeleteBlockStore(ctx, "share-remote", models.BlockStoreKindRemote)
		if !errors.Is(err, models.ErrStoreInUse) {
			t.Errorf("expected ErrStoreInUse, got %v", err)
		}
	})
}
