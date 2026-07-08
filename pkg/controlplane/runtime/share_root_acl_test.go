package runtime

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestShareRootGrantACL_ProjectsSIDGrant verifies the grant-collection method
// extracted for #1608 projects a direct AD/SID share grant into an allow ACE
// (Who="sid:<SID>") that the SMB Security-tab path then merges into file
// descriptors. Uses DefaultPermission=none so only the always-on trustees and
// the configured SID grant appear.
func TestShareRootGrantACL_ProjectsSIDGrant(t *testing.T) {
	rt, s := setupTestRuntime(t)
	ctx := context.Background()

	localID := createLocalBlockStoreConfig(t, s, "grant-local")
	metaStores, _ := s.ListMetadataStores(ctx)
	share := &models.Share{
		Name:              "/grants",
		MetadataStoreID:   metaStores[0].ID,
		LocalBlockStoreID: localID,
		DefaultPermission: string(models.PermissionNone),
	}
	if _, err := s.CreateShare(ctx, share); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	if err := LoadSharesFromStore(ctx, rt, s); err != nil {
		t.Fatalf("LoadSharesFromStore: %v", err)
	}

	dbShare, err := s.GetShare(ctx, "/grants")
	if err != nil {
		t.Fatalf("GetShare(db): %v", err)
	}

	const aliceSID = "S-1-5-21-188294588-3368521931-100232490-1106"
	if err := s.SetSIDSharePermission(ctx, &models.SIDSharePermission{
		SID:        aliceSID,
		ShareID:    dbShare.ID,
		ShareName:  "/grants",
		Permission: string(models.PermissionRead),
	}); err != nil {
		t.Fatalf("SetSIDSharePermission: %v", err)
	}

	dacl, err := rt.ShareRootGrantACL(ctx, "/grants")
	if err != nil {
		t.Fatalf("ShareRootGrantACL: %v", err)
	}
	if dacl == nil {
		t.Fatal("ShareRootGrantACL returned nil for a fully-wired runtime")
	}

	found := false
	for _, ace := range dacl.ACEs {
		if ace.Who == "sid:"+aliceSID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("grant ACL missing the AD SID grant sid:%s; got ACEs %+v", aliceSID, dacl.ACEs)
	}
}
