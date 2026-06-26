package runtime

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// A share created with an owner (RootAttr UID/GID) stamps that ownership onto
// the export root directory. This is the filesystem layer of the share-owner
// model: the owner governs who can write at the root via POSIX, independent of
// the (gate-only) share permission grants.
func TestAddShare_StampsRootOwner(t *testing.T) {
	rt := New(nil)
	ctx := context.Background()
	if err := rt.RegisterMetadataStore("test-meta", memory.NewMemoryMetadataStoreWithDefaults()); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	const ownerUID, ownerGID = uint32(1000), uint32(1000)
	cfg := &ShareConfig{
		Name:          "/owned",
		MetadataStore: "test-meta",
		RootAttr:      &metadata.FileAttr{UID: ownerUID, GID: ownerGID},
	}
	if err := rt.AddShare(ctx, cfg); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	rootHandle, err := rt.GetRootHandle("/owned")
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	root, err := rt.GetMetadataService().GetFile(ctx, rootHandle)
	if err != nil {
		t.Fatalf("GetFile(root): %v", err)
	}
	if root.UID != ownerUID || root.GID != ownerGID {
		t.Errorf("root owner = %d:%d, want %d:%d", root.UID, root.GID, ownerUID, ownerGID)
	}
	// Secure default mode is preserved: owner-writable, not world-writable.
	if perm := root.Mode & 0o777; perm != 0o755 {
		t.Errorf("root mode = %#o, want 0755", perm)
	}
}

// With no owner specified, the root stays owned by root (UID/GID 0) — the
// secure default, unchanged.
func TestAddShare_DefaultRootOwnerIsRoot(t *testing.T) {
	rt := New(nil)
	ctx := context.Background()
	if err := rt.RegisterMetadataStore("test-meta", memory.NewMemoryMetadataStoreWithDefaults()); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	if err := rt.AddShare(ctx, &ShareConfig{Name: "/def", MetadataStore: "test-meta"}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	rootHandle, err := rt.GetRootHandle("/def")
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	root, err := rt.GetMetadataService().GetFile(ctx, rootHandle)
	if err != nil {
		t.Fatalf("GetFile(root): %v", err)
	}
	if root.UID != 0 || root.GID != 0 {
		t.Errorf("default root owner = %d:%d, want 0:0", root.UID, root.GID)
	}
}
