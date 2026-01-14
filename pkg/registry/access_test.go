package registry

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	metadataMemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

func TestApplyIdentityMapping_NoMapping(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()

	_ = reg.RegisterMetadataStore("meta1", metaStore)
	_ = reg.AddShare(context.Background(), &ShareConfig{
		Name:          "/export",
		MetadataStore: "meta1",
		RootAttr:      &metadata.FileAttr{},
		// No squashing configured
	})

	uid := uint32(1000)
	gid := uint32(1000)
	identity := &metadata.Identity{
		UID:      &uid,
		GID:      &gid,
		GIDs:     []uint32{1000, 1001},
		Username: "testuser",
	}

	effective, err := reg.ApplyIdentityMapping("/export", identity)
	if err != nil {
		t.Fatalf("ApplyIdentityMapping failed: %v", err)
	}

	// Should return unchanged identity
	if *effective.UID != 1000 {
		t.Errorf("Expected UID 1000, got %d", *effective.UID)
	}
	if *effective.GID != 1000 {
		t.Errorf("Expected GID 1000, got %d", *effective.GID)
	}
	if effective.Username != "testuser" {
		t.Errorf("Expected username 'testuser', got %q", effective.Username)
	}
}

func TestApplyIdentityMapping_AllSquash(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()

	_ = reg.RegisterMetadataStore("meta1", metaStore)
	_ = reg.AddShare(context.Background(), &ShareConfig{
		Name:              "/export",
		MetadataStore:     "meta1",
		RootAttr:          &metadata.FileAttr{},
		MapAllToAnonymous: true,
		AnonymousUID:      65534,
		AnonymousGID:      65534,
	})

	uid := uint32(1000)
	gid := uint32(1000)
	identity := &metadata.Identity{
		UID:      &uid,
		GID:      &gid,
		GIDs:     []uint32{1000, 1001},
		Username: "testuser",
	}

	effective, err := reg.ApplyIdentityMapping("/export", identity)
	if err != nil {
		t.Fatalf("ApplyIdentityMapping failed: %v", err)
	}

	// All users should be mapped to anonymous
	if *effective.UID != 65534 {
		t.Errorf("Expected anonymous UID 65534, got %d", *effective.UID)
	}
	if *effective.GID != 65534 {
		t.Errorf("Expected anonymous GID 65534, got %d", *effective.GID)
	}
	if len(effective.GIDs) != 1 || effective.GIDs[0] != 65534 {
		t.Errorf("Expected GIDs [65534], got %v", effective.GIDs)
	}
}

func TestApplyIdentityMapping_RootSquash(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()

	_ = reg.RegisterMetadataStore("meta1", metaStore)
	_ = reg.AddShare(context.Background(), &ShareConfig{
		Name:                     "/export",
		MetadataStore:            "meta1",
		RootAttr:                 &metadata.FileAttr{},
		MapPrivilegedToAnonymous: true,
		AnonymousUID:             65534,
		AnonymousGID:             65534,
	})

	// Test root user is squashed
	rootUID := uint32(0)
	rootGID := uint32(0)
	rootIdentity := &metadata.Identity{
		UID:      &rootUID,
		GID:      &rootGID,
		GIDs:     []uint32{0},
		Username: "root",
	}

	effective, err := reg.ApplyIdentityMapping("/export", rootIdentity)
	if err != nil {
		t.Fatalf("ApplyIdentityMapping failed: %v", err)
	}

	// Root should be mapped to anonymous
	if *effective.UID != 65534 {
		t.Errorf("Expected anonymous UID 65534 for root, got %d", *effective.UID)
	}
	if *effective.GID != 65534 {
		t.Errorf("Expected anonymous GID 65534 for root, got %d", *effective.GID)
	}

	// Test non-root user is NOT squashed
	normalUID := uint32(1000)
	normalGID := uint32(1000)
	normalIdentity := &metadata.Identity{
		UID:      &normalUID,
		GID:      &normalGID,
		GIDs:     []uint32{1000},
		Username: "normaluser",
	}

	effective, err = reg.ApplyIdentityMapping("/export", normalIdentity)
	if err != nil {
		t.Fatalf("ApplyIdentityMapping failed: %v", err)
	}

	// Non-root should keep their identity
	if *effective.UID != 1000 {
		t.Errorf("Expected UID 1000 for non-root, got %d", *effective.UID)
	}
	if *effective.GID != 1000 {
		t.Errorf("Expected GID 1000 for non-root, got %d", *effective.GID)
	}
}

func TestApplyIdentityMapping_AllSquashTakesPrecedence(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()

	_ = reg.RegisterMetadataStore("meta1", metaStore)
	_ = reg.AddShare(context.Background(), &ShareConfig{
		Name:                     "/export",
		MetadataStore:            "meta1",
		RootAttr:                 &metadata.FileAttr{},
		MapAllToAnonymous:        true, // all_squash
		MapPrivilegedToAnonymous: true, // root_squash (should be ignored)
		AnonymousUID:             65534,
		AnonymousGID:             65534,
	})

	// Even non-root should be squashed with all_squash
	uid := uint32(1000)
	gid := uint32(1000)
	identity := &metadata.Identity{
		UID:      &uid,
		GID:      &gid,
		Username: "testuser",
	}

	effective, err := reg.ApplyIdentityMapping("/export", identity)
	if err != nil {
		t.Fatalf("ApplyIdentityMapping failed: %v", err)
	}

	if *effective.UID != 65534 {
		t.Errorf("Expected anonymous UID 65534, got %d", *effective.UID)
	}
}

func TestApplyIdentityMapping_ShareNotFound(t *testing.T) {
	reg := NewRegistry()

	uid := uint32(1000)
	identity := &metadata.Identity{
		UID: &uid,
	}

	_, err := reg.ApplyIdentityMapping("/nonexistent", identity)
	if err == nil {
		t.Error("Expected error for non-existent share")
	}
}

func TestGetShareNameForHandle(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()

	_ = reg.RegisterMetadataStore("meta1", metaStore)
	_ = reg.AddShare(context.Background(), &ShareConfig{
		Name:          "/export",
		MetadataStore: "meta1",
		RootAttr:      &metadata.FileAttr{},
	})

	// Get root handle for share
	rootHandle, err := reg.GetRootHandle("/export")
	if err != nil {
		t.Fatalf("GetRootHandle failed: %v", err)
	}

	// Extract share name from handle
	shareName, err := reg.GetShareNameForHandle(context.Background(), rootHandle)
	if err != nil {
		t.Fatalf("GetShareNameForHandle failed: %v", err)
	}

	if shareName != "/export" {
		t.Errorf("Expected share name '/export', got %q", shareName)
	}
}

func TestGetShareNameForHandle_InvalidHandle(t *testing.T) {
	reg := NewRegistry()

	// Invalid handle
	invalidHandle := metadata.FileHandle([]byte("invalid"))

	_, err := reg.GetShareNameForHandle(context.Background(), invalidHandle)
	if err == nil {
		t.Error("Expected error for invalid handle")
	}
}

func TestGetShareNameForHandle_ShareNotInRegistry(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()

	_ = reg.RegisterMetadataStore("meta1", metaStore)
	_ = reg.AddShare(context.Background(), &ShareConfig{
		Name:          "/export",
		MetadataStore: "meta1",
		RootAttr:      &metadata.FileAttr{},
	})

	// Get root handle
	rootHandle, _ := reg.GetRootHandle("/export")

	// Remove share from registry
	_ = reg.RemoveShare("/export")

	// Now the share doesn't exist but we have a valid handle format
	_, err := reg.GetShareNameForHandle(context.Background(), rootHandle)
	if err == nil {
		t.Error("Expected error when share no longer exists in registry")
	}
}

func TestApplyIdentityMapping_AnonymousAccess(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()

	_ = reg.RegisterMetadataStore("meta1", metaStore)
	_ = reg.AddShare(context.Background(), &ShareConfig{
		Name:          "/export",
		MetadataStore: "meta1",
		RootAttr:      &metadata.FileAttr{},
		AnonymousUID:  65534,
		AnonymousGID:  65534,
		// No squashing - but nil UID should still map to anonymous
	})

	// Test nil UID (AUTH_NULL / anonymous access)
	identity := &metadata.Identity{
		UID:      nil, // No credentials provided
		GID:      nil,
		GIDs:     nil,
		Username: "",
	}

	effective, err := reg.ApplyIdentityMapping("/export", identity)
	if err != nil {
		t.Fatalf("ApplyIdentityMapping failed: %v", err)
	}

	// Anonymous access should map to configured anonymous UID/GID
	if *effective.UID != 65534 {
		t.Errorf("Expected anonymous UID 65534 for nil UID, got %d", *effective.UID)
	}
	if *effective.GID != 65534 {
		t.Errorf("Expected anonymous GID 65534 for nil GID, got %d", *effective.GID)
	}
	if len(effective.GIDs) != 1 || effective.GIDs[0] != 65534 {
		t.Errorf("Expected GIDs [65534] for anonymous, got %v", effective.GIDs)
	}
}

func TestApplyIdentityMapping_PreservesOriginalIdentity(t *testing.T) {
	reg := NewRegistry()
	metaStore := metadataMemory.NewMemoryMetadataStoreWithDefaults()

	_ = reg.RegisterMetadataStore("meta1", metaStore)
	_ = reg.AddShare(context.Background(), &ShareConfig{
		Name:              "/export",
		MetadataStore:     "meta1",
		RootAttr:          &metadata.FileAttr{},
		MapAllToAnonymous: true,
		AnonymousUID:      65534,
		AnonymousGID:      65534,
	})

	uid := uint32(1000)
	gid := uint32(1000)
	original := &metadata.Identity{
		UID:      &uid,
		GID:      &gid,
		GIDs:     []uint32{1000, 1001},
		Username: "testuser",
	}

	_, _ = reg.ApplyIdentityMapping("/export", original)

	// Original identity should not be modified
	if *original.UID != 1000 {
		t.Errorf("Original UID was modified: got %d, want 1000", *original.UID)
	}
	if *original.GID != 1000 {
		t.Errorf("Original GID was modified: got %d, want 1000", *original.GID)
	}
	if original.Username != "testuser" {
		t.Errorf("Original username was modified: got %q, want 'testuser'", original.Username)
	}
}
