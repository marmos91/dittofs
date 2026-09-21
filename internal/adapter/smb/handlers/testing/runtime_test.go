package testing

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestNewShareRuntime_AddsAShare exercises the one contract every caller of
// these fixtures depends on: that the returned runtime accepts an AddShare
// carrying the returned block-store id.
//
// It covers both halves that can silently rot. AddShare fails without a
// journal root, so a dropped SetLocalStoreDefaults fails here; and it needs a
// block store that exists, so a block-store id that was never created fails
// here too. Neither is observable from the fixture's return values alone.
func TestNewShareRuntime_AddsAShare(t *testing.T) {
	rt, bsID := NewShareRuntime(t)

	if err := rt.RegisterMetadataStore("test-meta", memory.NewMemoryMetadataStoreWithDefaults()); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:              "/fixture",
		MetadataStore:     "test-meta",
		BlockStoreID:      bsID,
		DefaultPermission: string(models.PermissionReadWrite),
		RootAttr:          &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0755},
	}); err != nil {
		t.Fatalf("AddShare with the fixture's block-store id: %v", err)
	}

	if _, err := rt.GetRootHandle("/fixture"); err != nil {
		t.Fatalf("GetRootHandle on the share just added: %v", err)
	}
}
