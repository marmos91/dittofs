package memory_test

import (
	"context"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestShareRegistration verifies that CreateRootDirectory — the only entry
// point that records a share — makes it resolvable by name. Regression for the
// #recycle trash path returning "share not found".
func TestShareRegistration(t *testing.T) {
	ctx := context.Background()

	t.Run("CreateRootDirectory registers the share by name", func(t *testing.T) {
		store := memory.NewMemoryMetadataStoreWithDefaults()

		root, err := store.CreateRootDirectory(ctx, "/export", &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o755,
		})
		if err != nil {
			t.Fatalf("CreateRootDirectory: %v", err)
		}

		// GetRootHandle must resolve the share off the root directory alone.
		rh, err := store.GetRootHandle(ctx, "/export")
		if err != nil {
			t.Fatalf("GetRootHandle after CreateRootDirectory: %v", err)
		}
		if len(rh) == 0 {
			t.Fatal("GetRootHandle returned an empty handle")
		}

		// GetShareOptions returns defaults, not "share not found".
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

}

// TestGenerateHandle_OverLongShareName pins that the memory backend reports an
// unencodable share name as an error, the way badger/sqlite/postgres do,
// instead of panicking and taking the server down.
func TestGenerateHandle_OverLongShareName(t *testing.T) {
	store := memory.NewMemoryMetadataStoreWithDefaults()
	t.Cleanup(func() { _ = store.Close() })

	name := "/" + strings.Repeat("a", metadata.MaxShareNameLen)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("GenerateHandle panicked on over-long share name: %v", r)
		}
	}()

	if _, err := store.GenerateHandle(context.Background(), name, "/f"); err == nil {
		t.Fatal("GenerateHandle with over-long share name: want error, got nil")
	}
}
