package config

import (
	"context"
	"strings"
	"testing"
)

func TestCreateContentStore_Filesystem(t *testing.T) {
	ctx := context.Background()
	cfg := &ContentConfig{
		Type: "filesystem",
		Filesystem: map[string]any{
			"path": t.TempDir(),
		},
	}

	store, err := CreateContentStore(ctx, cfg)
	if err != nil {
		t.Fatalf("Failed to create filesystem content store: %v", err)
	}

	if store == nil {
		t.Fatal("Expected non-nil store")
	}
}

func TestCreateContentStore_FilesystemMissingPath(t *testing.T) {
	ctx := context.Background()
	cfg := &ContentConfig{
		Type:       "filesystem",
		Filesystem: map[string]any{},
	}

	_, err := CreateContentStore(ctx, cfg)
	if err == nil {
		t.Fatal("Expected error for missing path")
	}
	if !strings.Contains(err.Error(), "path is required") {
		t.Errorf("Expected 'path is required' error, got: %v", err)
	}
}

func TestCreateContentStore_Memory(t *testing.T) {
	ctx := context.Background()
	cfg := &ContentConfig{
		Type:   "memory",
		Memory: map[string]any{},
	}

	_, err := CreateContentStore(ctx, cfg)
	if err == nil {
		t.Fatal("Expected error for unimplemented memory store")
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("Expected 'not yet implemented' error, got: %v", err)
	}
}

func TestCreateContentStore_UnknownType(t *testing.T) {
	ctx := context.Background()
	cfg := &ContentConfig{
		Type: "s3",
	}

	_, err := CreateContentStore(ctx, cfg)
	if err == nil {
		t.Fatal("Expected error for unknown store type")
	}
	if !strings.Contains(err.Error(), "unknown content store type") {
		t.Errorf("Expected 'unknown content store type' error, got: %v", err)
	}
}

func TestCreateMetadataStore_Memory(t *testing.T) {
	ctx := context.Background()
	cfg := &MetadataConfig{
		Type:   "memory",
		Memory: map[string]any{},
	}

	store, err := CreateMetadataStore(ctx, cfg)
	if err != nil {
		t.Fatalf("Failed to create memory metadata store: %v", err)
	}

	if store == nil {
		t.Fatal("Expected non-nil store")
	}
}

func TestCreateMetadataStore_UnknownType(t *testing.T) {
	ctx := context.Background()
	cfg := &MetadataConfig{
		Type: "postgres",
	}

	_, err := CreateMetadataStore(ctx, cfg)
	if err == nil {
		t.Fatal("Expected error for unknown store type")
	}
	if !strings.Contains(err.Error(), "unknown metadata store type") {
		t.Errorf("Expected 'unknown metadata store type' error, got: %v", err)
	}
}

func TestCreateShares_SingleShare(t *testing.T) {
	ctx := context.Background()

	// Create metadata store
	metadataCfg := &MetadataConfig{
		Type:   "memory",
		Memory: map[string]any{},
	}
	store, err := CreateMetadataStore(ctx, metadataCfg)
	if err != nil {
		t.Fatalf("Failed to create metadata store: %v", err)
	}

	// Create share
	shares := []ShareConfig{
		{
			Name:     "/test",
			ReadOnly: false,
			Async:    true,
			AllowedAuthMethods: []string{"anonymous", "unix"},
			IdentityMapping: IdentityMappingConfig{
				MapAllToAnonymous: true,
				AnonymousUID:      65534,
				AnonymousGID:      65534,
			},
			RootAttr: RootAttrConfig{
				Mode: 0755,
				UID:  0,
				GID:  0,
			},
		},
	}

	err = CreateShares(ctx, store, shares)
	if err != nil {
		t.Fatalf("Failed to create shares: %v", err)
	}

	// Verify share was created
	shareList, err := store.GetShares(ctx)
	if err != nil {
		t.Fatalf("Failed to get shares: %v", err)
	}

	if len(shareList) != 1 {
		t.Errorf("Expected 1 share, got %d", len(shareList))
	}

	if shareList[0].Name != "/test" {
		t.Errorf("Expected share name '/test', got %q", shareList[0].Name)
	}
}

func TestCreateShares_MultipleShares(t *testing.T) {
	ctx := context.Background()

	metadataCfg := &MetadataConfig{
		Type:   "memory",
		Memory: map[string]any{},
	}
	store, err := CreateMetadataStore(ctx, metadataCfg)
	if err != nil {
		t.Fatalf("Failed to create metadata store: %v", err)
	}

	shares := []ShareConfig{
		{
			Name: "/share1",
			IdentityMapping: IdentityMappingConfig{
				AnonymousUID: 65534,
				AnonymousGID: 65534,
			},
			RootAttr: RootAttrConfig{Mode: 0755},
		},
		{
			Name: "/share2",
			IdentityMapping: IdentityMappingConfig{
				AnonymousUID: 65534,
				AnonymousGID: 65534,
			},
			RootAttr: RootAttrConfig{Mode: 0755},
		},
	}

	err = CreateShares(ctx, store, shares)
	if err != nil {
		t.Fatalf("Failed to create multiple shares: %v", err)
	}

	shareList, err := store.GetShares(ctx)
	if err != nil {
		t.Fatalf("Failed to get shares: %v", err)
	}

	if len(shareList) != 2 {
		t.Errorf("Expected 2 shares, got %d", len(shareList))
	}
}

func TestConfigureMetadataStore_DumpRestricted(t *testing.T) {
	ctx := context.Background()

	metadataCfg := &MetadataConfig{
		Type:               "memory",
		Memory:             map[string]any{},
		DumpRestricted:     true,
		DumpAllowedClients: []string{"127.0.0.1", "::1"},
	}

	store, err := CreateMetadataStore(ctx, metadataCfg)
	if err != nil {
		t.Fatalf("Failed to create metadata store: %v", err)
	}

	err = ConfigureMetadataStore(ctx, store, metadataCfg)
	if err != nil {
		t.Fatalf("Failed to configure metadata store: %v", err)
	}

	// Verify configuration was applied
	config, err := store.GetServerConfig(ctx)
	if err != nil {
		t.Fatalf("Failed to get server config: %v", err)
	}

	if config.CustomSettings == nil {
		t.Fatal("Expected CustomSettings to be set")
	}

	clients, ok := config.CustomSettings["nfs.mount.dump_allowed_clients"]
	if !ok {
		t.Fatal("Expected dump_allowed_clients to be set in custom settings")
	}

	clientList, ok := clients.([]string)
	if !ok {
		t.Fatal("Expected dump_allowed_clients to be []string")
	}

	if len(clientList) != 2 {
		t.Errorf("Expected 2 allowed clients, got %d", len(clientList))
	}
}

func TestConfigureMetadataStore_DumpNotRestricted(t *testing.T) {
	ctx := context.Background()

	metadataCfg := &MetadataConfig{
		Type:           "memory",
		Memory:         map[string]any{},
		DumpRestricted: false,
	}

	store, err := CreateMetadataStore(ctx, metadataCfg)
	if err != nil {
		t.Fatalf("Failed to create metadata store: %v", err)
	}

	err = ConfigureMetadataStore(ctx, store, metadataCfg)
	if err != nil {
		t.Fatalf("Failed to configure metadata store: %v", err)
	}

	// Verify configuration
	config, err := store.GetServerConfig(ctx)
	if err != nil {
		t.Fatalf("Failed to get server config: %v", err)
	}

	// dump_allowed_clients should not be set
	if config.CustomSettings != nil {
		if _, ok := config.CustomSettings["nfs.mount.dump_allowed_clients"]; ok {
			t.Error("Did not expect dump_allowed_clients to be set when not restricted")
		}
	}
}

func TestCreateContentStore_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	cfg := &ContentConfig{
		Type: "filesystem",
		Filesystem: map[string]any{
			"path": t.TempDir(),
		},
	}

	_, err := CreateContentStore(ctx, cfg)
	if err == nil {
		t.Fatal("Expected error for canceled context")
	}
	// The error may be wrapped, so check with errors.Is or just check it's not nil
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("Expected context canceled error, got: %v", err)
	}
}

func TestCreateMetadataStore_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	cfg := &MetadataConfig{
		Type:   "memory",
		Memory: map[string]any{},
	}

	_, err := CreateMetadataStore(ctx, cfg)
	if err == nil {
		t.Fatal("Expected error for canceled context")
	}
	if err != context.Canceled {
		t.Errorf("Expected context.Canceled error, got: %v", err)
	}
}
