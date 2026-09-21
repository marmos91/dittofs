package testruntime

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestNewWithShareStore_AddsAShare exercises the one contract every caller of
// these fixtures depends on: that the returned runtime accepts an AddShare
// carrying the returned block-store id.
//
// It covers the two halves that are invisible from the return values. AddShare
// fails without a journal root, so a dropped SetLocalStoreDefaults fails here;
// and it needs a block store that exists, so an id that was never created
// fails here too.
//
// It does not cover the share-removal cleanup, and cannot: dropping that block
// leaves this test green. Only a platform that refuses to unlink a file still
// open fails when a share's journal outlives the temp dir, so that ordering is
// checked by the Windows CI run, not here.
func TestNewWithShareStore_AddsAShare(t *testing.T) {
	rt, bsID := NewWithShareStore(t)

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
