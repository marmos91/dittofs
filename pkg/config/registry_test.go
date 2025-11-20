package config

import (
	"context"
	"testing"
)

func TestInitializeRegistry_Success(t *testing.T) {
	ctx := context.Background()

	cfg := &Config{
		Metadata: MetadataConfig{
			Global: MetadataGlobalConfig{},
			Stores: map[string]MetadataStoreConfig{
				"meta1": {
					Type: "memory",
					Memory: map[string]any{
						"capabilities": map[string]any{},
					},
				},
			},
		},
		Content: ContentConfig{
			Global: ContentGlobalConfig{},
			Stores: map[string]ContentStoreConfig{
				"content1": {
					Type: "memory",
				},
			},
		},
		Shares: []ShareConfig{
			{
				Name:          "/export",
				MetadataStore: "meta1",
				ContentStore:  "content1",
				ReadOnly:      false,
			},
		},
	}

	reg, err := InitializeRegistry(ctx, cfg)
	if err != nil {
		t.Fatalf("InitializeRegistry failed: %v", err)
	}

	// Verify registry contents
	if reg.CountMetadataStores() != 1 {
		t.Errorf("Expected 1 metadata store, got %d", reg.CountMetadataStores())
	}
	if reg.CountContentStores() != 1 {
		t.Errorf("Expected 1 content store, got %d", reg.CountContentStores())
	}
	if reg.CountShares() != 1 {
		t.Errorf("Expected 1 share, got %d", reg.CountShares())
	}

	// Verify store retrieval
	_, err = reg.GetMetadataStore("meta1")
	if err != nil {
		t.Errorf("Failed to get metadata store: %v", err)
	}

	_, err = reg.GetContentStore("content1")
	if err != nil {
		t.Errorf("Failed to get content store: %v", err)
	}

	// Verify share retrieval
	share, err := reg.GetShare("/export")
	if err != nil {
		t.Fatalf("Failed to get share: %v", err)
	}
	if share.Name != "/export" {
		t.Errorf("Share name = %q, want %q", share.Name, "/export")
	}
	if share.MetadataStore != "meta1" {
		t.Errorf("Share metadata store = %q, want %q", share.MetadataStore, "meta1")
	}
	if share.ContentStore != "content1" {
		t.Errorf("Share content store = %q, want %q", share.ContentStore, "content1")
	}
	if share.ReadOnly != false {
		t.Errorf("Share read_only = %v, want %v", share.ReadOnly, false)
	}
}

func TestInitializeRegistry_MultipleStoresAndShares(t *testing.T) {
	ctx := context.Background()

	cfg := &Config{
		Metadata: MetadataConfig{
			Stores: map[string]MetadataStoreConfig{
				"meta1": {Type: "memory"},
				"meta2": {Type: "memory"},
			},
		},
		Content: ContentConfig{
			Stores: map[string]ContentStoreConfig{
				"content1": {Type: "memory"},
				"content2": {Type: "memory"},
			},
		},
		Shares: []ShareConfig{
			{
				Name:          "/export1",
				MetadataStore: "meta1",
				ContentStore:  "content1",
				ReadOnly:      false,
			},
			{
				Name:          "/export2",
				MetadataStore: "meta2",
				ContentStore:  "content2",
				ReadOnly:      true,
			},
			{
				Name:          "/export3",
				MetadataStore: "meta1", // Reuse store
				ContentStore:  "content1",
				ReadOnly:      false,
			},
		},
	}

	reg, err := InitializeRegistry(ctx, cfg)
	if err != nil {
		t.Fatalf("InitializeRegistry failed: %v", err)
	}

	if reg.CountMetadataStores() != 2 {
		t.Errorf("Expected 2 metadata stores, got %d", reg.CountMetadataStores())
	}
	if reg.CountContentStores() != 2 {
		t.Errorf("Expected 2 content stores, got %d", reg.CountContentStores())
	}
	if reg.CountShares() != 3 {
		t.Errorf("Expected 3 shares, got %d", reg.CountShares())
	}

	// Verify share store usage
	sharesUsingMeta1 := reg.ListSharesUsingMetadataStore("meta1")
	if len(sharesUsingMeta1) != 2 {
		t.Errorf("Expected 2 shares using meta1, got %d", len(sharesUsingMeta1))
	}
}

func TestInitializeRegistry_NoMetadataStores(t *testing.T) {
	ctx := context.Background()

	cfg := &Config{
		Metadata: MetadataConfig{
			Stores: map[string]MetadataStoreConfig{}, // Empty
		},
		Content: ContentConfig{
			Stores: map[string]ContentStoreConfig{
				"content1": {Type: "memory"},
			},
		},
		Shares: []ShareConfig{
			{Name: "/export", MetadataStore: "meta1", ContentStore: "content1"},
		},
	}

	_, err := InitializeRegistry(ctx, cfg)
	if err == nil {
		t.Fatal("Expected error for missing metadata stores, got nil")
	}
}

func TestInitializeRegistry_NoContentStores(t *testing.T) {
	ctx := context.Background()

	cfg := &Config{
		Metadata: MetadataConfig{
			Stores: map[string]MetadataStoreConfig{
				"meta1": {Type: "memory"},
			},
		},
		Content: ContentConfig{
			Stores: map[string]ContentStoreConfig{}, // Empty
		},
		Shares: []ShareConfig{
			{Name: "/export", MetadataStore: "meta1", ContentStore: "content1"},
		},
	}

	_, err := InitializeRegistry(ctx, cfg)
	if err == nil {
		t.Fatal("Expected error for missing content stores, got nil")
	}
}

func TestInitializeRegistry_NoShares(t *testing.T) {
	ctx := context.Background()

	cfg := &Config{
		Metadata: MetadataConfig{
			Stores: map[string]MetadataStoreConfig{
				"meta1": {Type: "memory"},
			},
		},
		Content: ContentConfig{
			Stores: map[string]ContentStoreConfig{
				"content1": {Type: "memory"},
			},
		},
		Shares: []ShareConfig{}, // Empty
	}

	_, err := InitializeRegistry(ctx, cfg)
	if err == nil {
		t.Fatal("Expected error for missing shares, got nil")
	}
}

func TestInitializeRegistry_ShareReferencesNonexistentMetadataStore(t *testing.T) {
	ctx := context.Background()

	cfg := &Config{
		Metadata: MetadataConfig{
			Stores: map[string]MetadataStoreConfig{
				"meta1": {Type: "memory"},
			},
		},
		Content: ContentConfig{
			Stores: map[string]ContentStoreConfig{
				"content1": {Type: "memory"},
			},
		},
		Shares: []ShareConfig{
			{
				Name:          "/export",
				MetadataStore: "nonexistent", // References non-existent store
				ContentStore:  "content1",
			},
		},
	}

	_, err := InitializeRegistry(ctx, cfg)
	if err == nil {
		t.Fatal("Expected error for nonexistent metadata store, got nil")
	}
}

func TestInitializeRegistry_ShareReferencesNonexistentContentStore(t *testing.T) {
	ctx := context.Background()

	cfg := &Config{
		Metadata: MetadataConfig{
			Stores: map[string]MetadataStoreConfig{
				"meta1": {Type: "memory"},
			},
		},
		Content: ContentConfig{
			Stores: map[string]ContentStoreConfig{
				"content1": {Type: "memory"},
			},
		},
		Shares: []ShareConfig{
			{
				Name:          "/export",
				MetadataStore: "meta1",
				ContentStore:  "nonexistent", // References non-existent store
			},
		},
	}

	_, err := InitializeRegistry(ctx, cfg)
	if err == nil {
		t.Fatal("Expected error for nonexistent content store, got nil")
	}
}

func TestInitializeRegistry_EmptyShareName(t *testing.T) {
	ctx := context.Background()

	cfg := &Config{
		Metadata: MetadataConfig{
			Stores: map[string]MetadataStoreConfig{
				"meta1": {Type: "memory"},
			},
		},
		Content: ContentConfig{
			Stores: map[string]ContentStoreConfig{
				"content1": {Type: "memory"},
			},
		},
		Shares: []ShareConfig{
			{
				Name:          "", // Empty name
				MetadataStore: "meta1",
				ContentStore:  "content1",
			},
		},
	}

	_, err := InitializeRegistry(ctx, cfg)
	if err == nil {
		t.Fatal("Expected error for empty share name, got nil")
	}
}

func TestInitializeRegistry_DuplicateShareName(t *testing.T) {
	ctx := context.Background()

	cfg := &Config{
		Metadata: MetadataConfig{
			Stores: map[string]MetadataStoreConfig{
				"meta1": {Type: "memory"},
			},
		},
		Content: ContentConfig{
			Stores: map[string]ContentStoreConfig{
				"content1": {Type: "memory"},
			},
		},
		Shares: []ShareConfig{
			{
				Name:          "/export",
				MetadataStore: "meta1",
				ContentStore:  "content1",
			},
			{
				Name:          "/export", // Duplicate
				MetadataStore: "meta1",
				ContentStore:  "content1",
			},
		},
	}

	_, err := InitializeRegistry(ctx, cfg)
	if err == nil {
		t.Fatal("Expected error for duplicate share name, got nil")
	}
}

func TestInitializeRegistry_InvalidMetadataStoreType(t *testing.T) {
	ctx := context.Background()

	cfg := &Config{
		Metadata: MetadataConfig{
			Stores: map[string]MetadataStoreConfig{
				"meta1": {Type: "invalid_type"},
			},
		},
		Content: ContentConfig{
			Stores: map[string]ContentStoreConfig{
				"content1": {Type: "memory"},
			},
		},
		Shares: []ShareConfig{
			{Name: "/export", MetadataStore: "meta1", ContentStore: "content1"},
		},
	}

	_, err := InitializeRegistry(ctx, cfg)
	if err == nil {
		t.Fatal("Expected error for invalid metadata store type, got nil")
	}
}

func TestInitializeRegistry_InvalidContentStoreType(t *testing.T) {
	ctx := context.Background()

	cfg := &Config{
		Metadata: MetadataConfig{
			Stores: map[string]MetadataStoreConfig{
				"meta1": {Type: "memory"},
			},
		},
		Content: ContentConfig{
			Stores: map[string]ContentStoreConfig{
				"content1": {Type: "invalid_type"},
			},
		},
		Shares: []ShareConfig{
			{Name: "/export", MetadataStore: "meta1", ContentStore: "content1"},
		},
	}

	_, err := InitializeRegistry(ctx, cfg)
	if err == nil {
		t.Fatal("Expected error for invalid content store type, got nil")
	}
}

func TestInitializeRegistry_NilConfig(t *testing.T) {
	ctx := context.Background()

	_, err := InitializeRegistry(ctx, nil)
	if err == nil {
		t.Fatal("Expected error for nil config, got nil")
	}
}
