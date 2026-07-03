//go:build integration

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// Creating a share with an owner persists the resolved UID/GID on the share
// row so startup can re-apply the root ownership. Regression test for #1534:
// the owner was applied to the root only in-memory, so a server restart reset
// the share root to UID/GID 0 and the owner got ACCESS_DENIED on writes.
func TestShareHandler_Create_PersistsOwner(t *testing.T) {
	cpStore, _, handler := setupShareTestWithRuntime(t)
	ctx := context.Background()

	uid, gid := uint32(1000), uint32(2000)
	user := &models.User{Username: "smb-user", UID: &uid, GID: &gid, Enabled: true}
	if _, err := cpStore.CreateUser(ctx, user); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	metaStore := &models.MetadataStoreConfig{Name: "m-owner", Type: "memory"}
	if _, err := cpStore.CreateMetadataStore(ctx, metaStore); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	blockStore := &models.BlockStoreConfig{Name: "b-owner", Kind: models.BlockStoreKindLocal, Type: "memory"}
	if _, err := cpStore.CreateBlockStore(ctx, blockStore); err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"name":              "owned",
		"metadata_store_id": metaStore.ID,
		"local_block_store": blockStore.ID,
		"owner":             "smb-user",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.Create(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("Create = %d, want 201, body=%s", w.Code, w.Body.String())
	}

	share, err := cpStore.GetShare(ctx, "/owned")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if share.OwnerUID == nil || *share.OwnerUID != uid {
		t.Errorf("persisted OwnerUID = %v, want %d", share.OwnerUID, uid)
	}
	if share.OwnerGID == nil || *share.OwnerGID != gid {
		t.Errorf("persisted OwnerGID = %v, want %d", share.OwnerGID, gid)
	}
}

// An unknown owner still fails the request up front — and must do so before
// the share row is written, since owner resolution now happens pre-create.
func TestShareHandler_Create_UnknownOwnerRejectedBeforePersist(t *testing.T) {
	cpStore, _, handler := setupShareTestWithRuntime(t)
	ctx := context.Background()

	metaStore := &models.MetadataStoreConfig{Name: "m-noowner", Type: "memory"}
	if _, err := cpStore.CreateMetadataStore(ctx, metaStore); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	blockStore := &models.BlockStoreConfig{Name: "b-noowner", Kind: models.BlockStoreKindLocal, Type: "memory"}
	if _, err := cpStore.CreateBlockStore(ctx, blockStore); err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"name":              "ghost-owned",
		"metadata_store_id": metaStore.ID,
		"local_block_store": blockStore.ID,
		"owner":             "no-such-user",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.Create(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("Create = %d, want 400, body=%s", w.Code, w.Body.String())
	}
	if _, err := cpStore.GetShare(ctx, "/ghost-owned"); err == nil {
		t.Errorf("share row was persisted despite owner resolution failure")
	}
}
