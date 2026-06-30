package memory_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/metadata/storetest"
)

// TestBlockLayoutConformance runs the per-share BlockLayout conformance
// scenarios against the in-memory metadata store.
func TestBlockLayoutConformance(t *testing.T) {
	storetest.RunBlockLayoutSuite(t, func(t *testing.T) metadata.Store {
		return memory.NewMemoryMetadataStoreWithDefaults()
	})
}

// TestSeededShareRegistration verifies that CreateRootDirectory alone (the path
// the runtime uses — it never calls CreateShare) makes a share resolvable by
// name, and that a later CreateShare "finishes" the seeded entry without
// changing the root handle. Regression for the #recycle trash path returning
// "share not found" (PR #1463).
func TestSeededShareRegistration(t *testing.T) {
	ctx := context.Background()

	t.Run("CreateRootDirectory seeds by-name resolution", func(t *testing.T) {
		store := memory.NewMemoryMetadataStoreWithDefaults()

		root, err := store.CreateRootDirectory(ctx, "/export", &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o755,
		})
		if err != nil {
			t.Fatalf("CreateRootDirectory: %v", err)
		}

		// GetRootHandle must resolve the share without a prior CreateShare.
		rh, err := store.GetRootHandle(ctx, "/export")
		if err != nil {
			t.Fatalf("GetRootHandle after CreateRootDirectory: %v", err)
		}
		if len(rh) == 0 {
			t.Fatal("GetRootHandle returned an empty handle")
		}

		// GetShareOptions returns defaults (not "share not found") for a seeded share.
		if _, err := store.GetShareOptions(ctx, "/export"); err != nil {
			t.Fatalf("GetShareOptions after CreateRootDirectory: %v", err)
		}

		// The root file returned must match the handle GetRootHandle reports.
		gotRoot, err := store.GetFile(ctx, rh)
		if err != nil {
			t.Fatalf("GetFile(rootHandle): %v", err)
		}
		if gotRoot.ID != root.ID {
			t.Fatalf("root handle points to %s, want the created root %s", gotRoot.ID, root.ID)
		}
	})

	t.Run("CreateShare finishes a seed without changing the root handle", func(t *testing.T) {
		store := memory.NewMemoryMetadataStoreWithDefaults()

		if _, err := store.CreateRootDirectory(ctx, "/export", &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o755,
		}); err != nil {
			t.Fatalf("CreateRootDirectory: %v", err)
		}
		seedHandle, err := store.GetRootHandle(ctx, "/export")
		if err != nil {
			t.Fatalf("GetRootHandle (seed): %v", err)
		}

		// CreateShare on a seeded share must succeed (finish it), not error
		// "already exists", and must keep the seed's root handle so the existing
		// file tree stays reachable.
		if err := store.CreateShare(ctx, &metadata.Share{Name: "/export"}); err != nil {
			t.Fatalf("CreateShare finishing a seed: %v", err)
		}
		afterHandle, err := store.GetRootHandle(ctx, "/export")
		if err != nil {
			t.Fatalf("GetRootHandle (after CreateShare): %v", err)
		}
		if string(afterHandle) != string(seedHandle) {
			t.Fatal("CreateShare changed the seeded root handle")
		}

		// A second CreateShare on a now-real share is a genuine duplicate.
		if err := store.CreateShare(ctx, &metadata.Share{Name: "/export"}); err == nil {
			t.Fatal("expected ErrAlreadyExists creating a non-seeded share twice")
		}
	})
}
