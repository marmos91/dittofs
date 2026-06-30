//go:build integration

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestStoreAndShareNameOrIDResolution locks the docker-style addressing
// contract: metadata stores, block stores, and shares can all be looked up and
// deleted by either their name or their full ID.
func TestStoreAndShareNameOrIDResolution(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()
	ctx := context.Background()

	metaID, err := store.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: "meta", Type: "memory"})
	if err != nil {
		t.Fatalf("create metadata store: %v", err)
	}
	localID, err := store.CreateBlockStore(ctx, &models.BlockStoreConfig{Name: "local", Kind: models.BlockStoreKindLocal, Type: "fs"})
	if err != nil {
		t.Fatalf("create block store: %v", err)
	}

	t.Run("metadata store by name and by id", func(t *testing.T) {
		byName, err := store.GetMetadataStore(ctx, "meta")
		if err != nil || byName.ID != metaID {
			t.Fatalf("by name: got %+v err %v", byName, err)
		}
		byID, err := store.GetMetadataStore(ctx, metaID)
		if err != nil || byID.Name != "meta" {
			t.Fatalf("by id: got %+v err %v", byID, err)
		}
	})

	t.Run("block store by id is kind-scoped", func(t *testing.T) {
		byID, err := store.GetBlockStore(ctx, localID, models.BlockStoreKindLocal)
		if err != nil || byID.Name != "local" {
			t.Fatalf("by id (local): got %+v err %v", byID, err)
		}
		// The same ID under the wrong kind must not resolve.
		if _, err := store.GetBlockStore(ctx, localID, models.BlockStoreKindRemote); !errors.Is(err, models.ErrStoreNotFound) {
			t.Fatalf("by id (remote): expected ErrStoreNotFound, got %v", err)
		}
	})

	shareID, err := store.CreateShare(ctx, &models.Share{
		Name:              "/share",
		MetadataStoreID:   metaID,
		LocalBlockStoreID: localID,
	})
	if err != nil {
		t.Fatalf("create share: %v", err)
	}

	t.Run("share by name, by id, and by api-prefixed id", func(t *testing.T) {
		byName, err := store.GetShare(ctx, "/share")
		if err != nil || byName.ID != shareID {
			t.Fatalf("by name: got %+v err %v", byName, err)
		}
		byID, err := store.GetShare(ctx, shareID)
		if err != nil || byID.Name != "/share" {
			t.Fatalf("by id: got %+v err %v", byID, err)
		}
		// The API layer prepends "/" to the path token, so an ID arrives as
		// "/<id>"; it must still resolve.
		byPrefixedID, err := store.GetShare(ctx, "/"+shareID)
		if err != nil || byPrefixedID.ID != shareID {
			t.Fatalf("by prefixed id: got %+v err %v", byPrefixedID, err)
		}
	})

	t.Run("unknown token does not resolve", func(t *testing.T) {
		if _, err := store.GetMetadataStore(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, models.ErrStoreNotFound) {
			t.Fatalf("expected ErrStoreNotFound, got %v", err)
		}
	})

	t.Run("delete share by id", func(t *testing.T) {
		if err := store.DeleteShare(ctx, shareID); err != nil {
			t.Fatalf("delete share by id: %v", err)
		}
		if _, err := store.GetShare(ctx, shareID); !errors.Is(err, models.ErrShareNotFound) {
			t.Fatalf("expected ErrShareNotFound after delete, got %v", err)
		}
	})

	t.Run("delete metadata store by id", func(t *testing.T) {
		if err := store.DeleteMetadataStore(ctx, metaID); err != nil {
			t.Fatalf("delete metadata store by id: %v", err)
		}
		if _, err := store.GetMetadataStore(ctx, metaID); !errors.Is(err, models.ErrStoreNotFound) {
			t.Fatalf("expected ErrStoreNotFound after delete, got %v", err)
		}
	})
}
