package runtime

import (
	"context"
	"testing"

	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

func TestEnsureBlockStoreLocalOnly(t *testing.T) {
	// Create a real store with SQLite in-memory (no remote block stores configured).
	s, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	rt := New(s)
	ctx := context.Background()

	// Register a metadata store (required by EnsureBlockStore).
	metaStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("test-meta", metaStore); err != nil {
		t.Fatalf("failed to register metadata store: %v", err)
	}

	// Set cache config (required by EnsureBlockStore).
	rt.SetCacheConfig(&CacheConfig{
		Path: t.TempDir(),
		Size: 0, // unlimited
	})

	// No remote block stores are configured in the database.
	// EnsureBlockStore should succeed in local-only mode.
	if err := rt.EnsureBlockStore(ctx); err != nil {
		t.Fatalf("EnsureBlockStore should succeed with no remote stores (local-only), got: %v", err)
	}

	// Verify BlockStore was created.
	bs := rt.GetBlockStore()
	if bs == nil {
		t.Fatal("expected non-nil BlockStore after local-only init")
	}
}
