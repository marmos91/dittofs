package runtime

import (
	"context"
	"testing"

	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

func TestEnsurePayloadServiceLocalOnly(t *testing.T) {
	// Create a real store with SQLite in-memory (no payload stores configured).
	s, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	rt := New(s)
	ctx := context.Background()

	// Register a metadata store (required by EnsurePayloadService).
	metaStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("test-meta", metaStore); err != nil {
		t.Fatalf("failed to register metadata store: %v", err)
	}

	// Set cache config (required by EnsurePayloadService).
	rt.SetCacheConfig(&CacheConfig{
		Path: t.TempDir(),
		Size: 0, // unlimited
	})

	// No payload stores are configured in the database.
	// EnsurePayloadService should succeed in local-only mode.
	if err := rt.EnsurePayloadService(ctx); err != nil {
		t.Fatalf("EnsurePayloadService should succeed with no payload stores (local-only), got: %v", err)
	}

	// Verify PayloadService was created.
	ps := rt.GetPayloadService()
	if ps == nil {
		t.Fatal("expected non-nil PayloadService after local-only init")
	}
}
