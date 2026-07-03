package runtime

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// A share whose owner was persisted at creation (#1534) gets that ownership
// re-applied to its root directory when shares are loaded from the DB at
// startup. Before the fix, LoadSharesFromStore passed no RootAttr, so
// prepareShare defaulted the root owner to UID/GID 0.
func TestLoadSharesFromStore_ReappliesPersistedOwner(t *testing.T) {
	rt, s := setupTestRuntime(t)
	ctx := context.Background()

	localID := createLocalBlockStoreConfig(t, s, "owner-local")
	metaStores, err := s.ListMetadataStores(ctx)
	if err != nil || len(metaStores) == 0 {
		t.Fatalf("ListMetadataStores: %v", err)
	}

	uid, gid := uint32(1000), uint32(2000)
	share := &models.Share{
		Name:              "/owned-restart",
		MetadataStoreID:   metaStores[0].ID,
		LocalBlockStoreID: localID,
		OwnerUID:          &uid,
		OwnerGID:          &gid,
	}
	if _, err := s.CreateShare(ctx, share); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	if err := LoadSharesFromStore(ctx, rt, s); err != nil {
		t.Fatalf("LoadSharesFromStore: %v", err)
	}

	rootHandle, err := rt.GetRootHandle("/owned-restart")
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	root, err := rt.GetMetadataService().GetFile(ctx, rootHandle)
	if err != nil {
		t.Fatalf("GetFile(root): %v", err)
	}
	if root.UID != uid || root.GID != gid {
		t.Errorf("root owner after load = %d:%d, want %d:%d", root.UID, root.GID, uid, gid)
	}
}

// Full restart simulation against a persistent (badger) metadata store, which
// force-syncs an existing root's UID/GID to the attrs the runtime passes on
// AddShare. The regression (#1534): a share created with an owner came back
// root-owned after a server restart, so the owner got EACCES/ACCESS_DENIED on
// writes. The persisted owner must survive the reload.
func TestLoadSharesFromStore_OwnerSurvivesRestart_Badger(t *testing.T) {
	rt, s := setupTestRuntime(t)
	ctx := context.Background()

	metaStore, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	t.Cleanup(func() { _ = metaStore.Close() })
	metaCfg := &models.MetadataStoreConfig{Name: "badger-meta", Type: "badger"}
	if _, err := s.CreateMetadataStore(ctx, metaCfg); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	if err := rt.RegisterMetadataStore("badger-meta", metaStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	localID := createLocalBlockStoreConfig(t, s, "owner-local-badger")
	uid, gid := uint32(1000), uint32(1000)
	share := &models.Share{
		Name:              "/owned-badger",
		MetadataStoreID:   metaCfg.ID,
		LocalBlockStoreID: localID,
		OwnerUID:          &uid,
		OwnerGID:          &gid,
	}
	if _, err := s.CreateShare(ctx, share); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	// First boot: root directory is created with the owner's UID/GID.
	if err := LoadSharesFromStore(ctx, rt, s); err != nil {
		t.Fatalf("LoadSharesFromStore (first boot): %v", err)
	}

	// Simulate a restart: drop the runtime share, then reload from the DB.
	// The badger root directory persists, so the reload goes through the
	// existing-root force-sync path.
	if err := rt.RemoveShare("/owned-badger"); err != nil {
		t.Fatalf("RemoveShare: %v", err)
	}
	if err := LoadSharesFromStore(ctx, rt, s); err != nil {
		t.Fatalf("LoadSharesFromStore (restart): %v", err)
	}

	rootHandle, err := rt.GetRootHandle("/owned-badger")
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	root, err := rt.GetMetadataService().GetFile(ctx, rootHandle)
	if err != nil {
		t.Fatalf("GetFile(root): %v", err)
	}
	if root.UID != uid || root.GID != gid {
		t.Errorf("root owner after restart = %d:%d, want %d:%d", root.UID, root.GID, uid, gid)
	}
}
