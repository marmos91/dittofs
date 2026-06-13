//go:build integration

package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// craftCollidingConfig builds an NFS adapter config JSON that contains
// targetNetgroupID as a substring in an unrelated string field ("comment") but
// has no netgroup_id field at all. A naive LIKE '%<uuid>%' over the JSON blob
// would spuriously match this config; a JSON-path query must not.
func craftCollidingConfig(t *testing.T, targetNetgroupID string) string {
	t.Helper()
	type rawConfig struct {
		Squash     string  `json:"squash"`
		NetgroupID *string `json:"netgroup_id,omitempty"`
		Comment    string  `json:"comment"`
	}
	cfg := rawConfig{
		Squash:  "none",
		Comment: "ref:" + targetNetgroupID, // contains target UUID as substring
		// NetgroupID intentionally absent (nil) — this share references NO netgroup.
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("craftCollidingConfig: marshal: %v", err)
	}
	return string(b)
}

// TestDeleteNetgroup_NoFalsePositiveOnUUIDSubstring verifies that DeleteNetgroup
// does NOT block deletion when another share's adapter config contains the
// netgroup UUID as a substring in an unrelated JSON field (not in netgroup_id).
func TestDeleteNetgroup_NoFalsePositiveOnUUIDSubstring(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	ng := &models.Netgroup{Name: "ng-delete-test"}
	if _, err := s.CreateNetgroup(ctx, ng); err != nil {
		t.Fatalf("CreateNetgroup: %v", err)
	}
	ng, err := s.GetNetgroup(ctx, "ng-delete-test")
	if err != nil {
		t.Fatalf("GetNetgroup: %v", err)
	}

	meta := &models.MetadataStoreConfig{Name: "ng-test-meta", Type: "memory"}
	metaID, _ := s.CreateMetadataStore(ctx, meta)
	local := &models.BlockStoreConfig{Name: "ng-test-local", Kind: models.BlockStoreKindLocal, Type: "fs"}
	localID, _ := s.CreateBlockStore(ctx, local)
	share := &models.Share{
		Name:              "/ng-test-share",
		MetadataStoreID:   metaID,
		LocalBlockStoreID: localID,
	}
	shareID, err := s.CreateShare(ctx, share)
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	collidingConfig := craftCollidingConfig(t, ng.ID)
	sac := &models.ShareAdapterConfig{
		ID:          uuid.New().String(),
		ShareID:     shareID,
		AdapterType: "nfs",
		Config:      collidingConfig,
	}
	if err := s.DB().WithContext(ctx).Create(sac).Error; err != nil {
		t.Fatalf("insert colliding ShareAdapterConfig: %v", err)
	}

	if err := s.DeleteNetgroup(ctx, "ng-delete-test"); err != nil {
		t.Errorf("DeleteNetgroup returned unexpected error (false positive): %v", err)
	}
}

// TestDeleteNetgroup_BlockedWhenActuallyReferenced verifies the true-positive
// path: deletion IS blocked when netgroup_id in the JSON equals the netgroup UUID.
func TestDeleteNetgroup_BlockedWhenActuallyReferenced(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	ng := &models.Netgroup{Name: "ng-inuse-test"}
	if _, err := s.CreateNetgroup(ctx, ng); err != nil {
		t.Fatalf("CreateNetgroup: %v", err)
	}
	ng, _ = s.GetNetgroup(ctx, "ng-inuse-test")

	meta := &models.MetadataStoreConfig{Name: "ng-inuse-meta", Type: "memory"}
	metaID, _ := s.CreateMetadataStore(ctx, meta)
	local := &models.BlockStoreConfig{Name: "ng-inuse-local", Kind: models.BlockStoreKindLocal, Type: "fs"}
	localID, _ := s.CreateBlockStore(ctx, local)
	share := &models.Share{
		Name:              "/ng-inuse-share",
		MetadataStoreID:   metaID,
		LocalBlockStoreID: localID,
	}
	shareID, _ := s.CreateShare(ctx, share)

	ngID := ng.ID
	opts := models.NFSExportOptions{
		Squash:     "none",
		NetgroupID: &ngID,
	}
	var sac models.ShareAdapterConfig
	sac.ID = uuid.New().String()
	sac.ShareID = shareID
	sac.AdapterType = "nfs"
	if err := sac.SetConfig(opts); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if err := s.DB().WithContext(ctx).Create(&sac).Error; err != nil {
		t.Fatalf("insert referencing ShareAdapterConfig: %v", err)
	}

	if err := s.DeleteNetgroup(ctx, "ng-inuse-test"); err == nil {
		t.Error("expected ErrNetgroupInUse, got nil")
	}
}

// TestGetSharesByNetgroup_NoFalsePositiveOnUUIDSubstring mirrors the delete test
// for GetSharesByNetgroup: a share whose adapter config contains ng.ID as a
// substring in a non-netgroup_id field must NOT be returned.
func TestGetSharesByNetgroup_NoFalsePositiveOnUUIDSubstring(t *testing.T) {
	s := createTestStore(t)
	defer s.Close()
	ctx := context.Background()

	ng := &models.Netgroup{Name: "ng-query-test"}
	if _, err := s.CreateNetgroup(ctx, ng); err != nil {
		t.Fatalf("CreateNetgroup: %v", err)
	}
	ng, _ = s.GetNetgroup(ctx, "ng-query-test")

	meta := &models.MetadataStoreConfig{Name: "ng-query-meta", Type: "memory"}
	metaID, _ := s.CreateMetadataStore(ctx, meta)
	local := &models.BlockStoreConfig{Name: "ng-query-local", Kind: models.BlockStoreKindLocal, Type: "fs"}
	localID, _ := s.CreateBlockStore(ctx, local)
	share := &models.Share{
		Name:              "/ng-query-share",
		MetadataStoreID:   metaID,
		LocalBlockStoreID: localID,
	}
	shareID, _ := s.CreateShare(ctx, share)

	collidingConfig := craftCollidingConfig(t, ng.ID)
	sac := &models.ShareAdapterConfig{
		ID:          uuid.New().String(),
		ShareID:     shareID,
		AdapterType: "nfs",
		Config:      collidingConfig,
	}
	if err := s.DB().WithContext(ctx).Create(sac).Error; err != nil {
		t.Fatalf("insert colliding ShareAdapterConfig: %v", err)
	}

	shares, err := s.GetSharesByNetgroup(ctx, "ng-query-test")
	if err != nil {
		t.Fatalf("GetSharesByNetgroup: %v", err)
	}
	if len(shares) != 0 {
		t.Errorf("expected 0 shares (no true reference), got %d (false positive from LIKE)", len(shares))
	}
}
