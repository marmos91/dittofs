package testruntime

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"

	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
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
// It does not cover the journal half of the share-removal cleanup, and cannot:
// only a platform that refuses to unlink a file still open fails when a share's
// journal outlives the temp dir. The store half is
// TestNew_RemovesSharesBeforeTheirStoreCloses.
func TestNewWithShareStore_AddsAShare(t *testing.T) {
	rt, bsID := NewWithShareStore(t)

	if err := rt.RegisterMetadataStore("test-meta", badgertest.NewInMemory(t)); err != nil {
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

// TestNew_RemovesSharesBeforeTheirStoreCloses pins the removal to run before
// the cleanup of a store opened after the runtime is built. A share's engine
// reads its metadata store until the share is removed, so a removal registered
// once in New runs after that store closes, and a download still in flight
// reads a closed store.
//
// The probe is registered right after the store, so it runs just before the
// store closes and sees whether the share is still served.
func TestNew_RemovesSharesBeforeTheirStoreCloses(t *testing.T) {
	const name = "/fixture"
	var live bool
	t.Run("fixture", func(t *testing.T) {
		rt, bsID := NewWithShareStore(t)
		meta := badgertest.NewInMemory(t)
		t.Cleanup(func() { live = rt.ShareExists(name) })

		if err := rt.RegisterMetadataStore("test-meta", meta); err != nil {
			t.Fatalf("RegisterMetadataStore: %v", err)
		}
		if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
			Name:          name,
			MetadataStore: "test-meta",
			BlockStoreID:  bsID,
			RootAttr:      &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0755},
		}); err != nil {
			t.Fatalf("AddShare: %v", err)
		}
	})
	if live {
		t.Fatalf("share %q was still served when its metadata store closed", name)
	}
}
