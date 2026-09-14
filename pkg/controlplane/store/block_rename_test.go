//go:build integration

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func TestRenameBlockStore(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	metaID, err := store.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: "rn-meta", Type: "memory"})
	if err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	blocksID, err := store.CreateBlockStore(ctx, &models.BlockStoreConfig{Name: "rn-blocks", Type: "memory"})
	if err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	t.Run("renames and resolves under the new name", func(t *testing.T) {
		got, err := store.RenameBlockStore(ctx, "rn-blocks", "rn-archive")
		if err != nil {
			t.Fatalf("RenameBlockStore: %v", err)
		}
		if got.Name != "rn-archive" {
			t.Errorf("returned name = %q, want %q", got.Name, "rn-archive")
		}
		if got.ID != blocksID {
			t.Errorf("returned ID = %q, want the same store %q", got.ID, blocksID)
		}
		if _, err := store.GetBlockStore(ctx, "rn-archive"); err != nil {
			t.Errorf("the store should resolve under its new name: %v", err)
		}
		if _, err := store.GetBlockStore(ctx, "rn-blocks"); !errors.Is(err, models.ErrStoreNotFound) {
			t.Errorf("the old name should resolve to nothing, got %v", err)
		}
	})

	t.Run("refuses a name already in use", func(t *testing.T) {
		if _, err := store.CreateBlockStore(ctx, &models.BlockStoreConfig{Name: "rn-taken", Type: "memory"}); err != nil {
			t.Fatalf("CreateBlockStore: %v", err)
		}
		_, err := store.RenameBlockStore(ctx, "rn-archive", "rn-taken")
		if !errors.Is(err, models.ErrDuplicateStore) {
			t.Fatalf("rename onto a used name = %v, want ErrDuplicateStore", err)
		}
		// The refusal must leave the store exactly as it was.
		if _, err := store.GetBlockStore(ctx, "rn-archive"); err != nil {
			t.Errorf("the store should keep its name after a refused rename: %v", err)
		}
	})

	// A share's binding normally holds the UUID, but the older update path
	// persisted the name. Such a share resolves nothing once the name moves.
	t.Run("repoints a share that referenced the old name", func(t *testing.T) {
		if _, err := store.CreateShare(ctx, &models.Share{
			Name:            "/by-name",
			MetadataStoreID: metaID,
			BlockStoreID:    "rn-archive", // the name, not the UUID
		}); err != nil {
			t.Fatalf("CreateShare: %v", err)
		}

		if _, err := store.RenameBlockStore(ctx, "rn-archive", "rn-final"); err != nil {
			t.Fatalf("RenameBlockStore: %v", err)
		}

		got, err := store.GetShare(ctx, "/by-name")
		if err != nil {
			t.Fatalf("GetShare: %v", err)
		}
		if got.BlockStoreID != blocksID {
			t.Errorf("share binding = %q, want the canonical ID %q: a rename must not strand a share that named its store",
				got.BlockStoreID, blocksID)
		}
	})

	t.Run("renaming to the current name is a no-op", func(t *testing.T) {
		got, err := store.RenameBlockStore(ctx, "rn-final", "rn-final")
		if err != nil {
			t.Fatalf("RenameBlockStore(same name): %v", err)
		}
		if got.Name != "rn-final" {
			t.Errorf("name = %q, want %q", got.Name, "rn-final")
		}
	})

	t.Run("renaming a store that does not exist", func(t *testing.T) {
		_, err := store.RenameBlockStore(ctx, "rn-missing", "rn-whatever")
		if !errors.Is(err, models.ErrStoreNotFound) {
			t.Errorf("expected ErrStoreNotFound, got %v", err)
		}
	})
}
