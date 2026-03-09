//go:build integration

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

func setupBlockStoreTest(t *testing.T) (store.Store, *BlockStoreHandler) {
	t.Helper()

	dbConfig := store.Config{
		Type: "sqlite",
		SQLite: store.SQLiteConfig{
			Path: ":memory:",
		},
	}
	cpStore, err := store.New(&dbConfig)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	handler := NewBlockStoreHandler(cpStore)
	return cpStore, handler
}

// withBlockStoreKind creates a request with a chi URL param "kind" set.
func withBlockStoreKind(r *http.Request, kind string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("kind", kind)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// withBlockStoreKindAndName creates a request with chi URL params "kind" and "name" set.
func withBlockStoreKindAndName(r *http.Request, kind, name string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("kind", kind)
	rctx.URLParams.Add("name", name)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

func TestBlockStoreHandler_Create(t *testing.T) {
	_, handler := setupBlockStoreTest(t)

	body, _ := json.Marshal(CreateBlockStoreRequest{
		Name: "test-local-store",
		Type: "fs",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/store/block/local", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withBlockStoreKind(req, "local")
	w := httptest.NewRecorder()

	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("Create() status = %d, want %d, body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp BlockStoreResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	if resp.Name != "test-local-store" {
		t.Errorf("Name = %s, want test-local-store", resp.Name)
	}
	if resp.Kind != models.BlockStoreKindLocal {
		t.Errorf("Kind = %s, want %s", resp.Kind, models.BlockStoreKindLocal)
	}
	if resp.Type != "fs" {
		t.Errorf("Type = %s, want fs", resp.Type)
	}
}

func TestBlockStoreHandler_Create_InvalidKind(t *testing.T) {
	_, handler := setupBlockStoreTest(t)

	body, _ := json.Marshal(CreateBlockStoreRequest{
		Name: "bad-store",
		Type: "fs",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/store/block/invalid", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withBlockStoreKind(req, "invalid")
	w := httptest.NewRecorder()

	handler.Create(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Create(invalid kind) status = %d, want %d, body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestBlockStoreHandler_Create_TypeKindMismatch(t *testing.T) {
	_, handler := setupBlockStoreTest(t)

	tests := []struct {
		name     string
		kind     string
		storeTyp string
	}{
		{"s3 with local kind", "local", "s3"},
		{"fs with remote kind", "remote", "fs"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(CreateBlockStoreRequest{
				Name: "mismatched-store",
				Type: tt.storeTyp,
			})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/store/block/"+tt.kind, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req = withBlockStoreKind(req, tt.kind)
			w := httptest.NewRecorder()

			handler.Create(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("Create(%s) status = %d, want %d, body = %s", tt.name, w.Code, http.StatusBadRequest, w.Body.String())
			}
		})
	}
}

func TestBlockStoreHandler_List(t *testing.T) {
	cpStore, handler := setupBlockStoreTest(t)
	ctx := context.Background()

	// Create local and remote stores
	localStore := &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "local-1", Kind: models.BlockStoreKindLocal, Type: "fs",
		CreatedAt: time.Now(),
	}
	remoteStore := &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "remote-1", Kind: models.BlockStoreKindRemote, Type: "s3",
		CreatedAt: time.Now(),
	}
	cpStore.CreateBlockStore(ctx, localStore)
	cpStore.CreateBlockStore(ctx, remoteStore)

	// List remote stores
	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/block/remote", nil)
	req = withBlockStoreKind(req, "remote")
	w := httptest.NewRecorder()

	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("List() status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp []BlockStoreResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	if len(resp) != 1 {
		t.Errorf("List(remote) returned %d items, want 1", len(resp))
	}
	if len(resp) > 0 && resp[0].Kind != models.BlockStoreKindRemote {
		t.Errorf("List(remote) returned kind = %s, want remote", resp[0].Kind)
	}
}

func TestBlockStoreHandler_Get_NotFound(t *testing.T) {
	_, handler := setupBlockStoreTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/block/local/nonexistent", nil)
	req = withBlockStoreKindAndName(req, "local", "nonexistent")
	w := httptest.NewRecorder()

	handler.Get(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Get(nonexistent) status = %d, want %d, body = %s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestBlockStoreHandler_Delete_InUse(t *testing.T) {
	cpStore, handler := setupBlockStoreTest(t)
	ctx := context.Background()

	// Create a metadata store first (required for shares)
	metaStore := &models.MetadataStoreConfig{
		ID: uuid.New().String(), Name: "meta-1", Type: "memory",
		CreatedAt: time.Now(),
	}
	cpStore.CreateMetadataStore(ctx, metaStore)

	// Create a local block store
	blockStore := &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "in-use-store", Kind: models.BlockStoreKindLocal, Type: "fs",
		CreatedAt: time.Now(),
	}
	cpStore.CreateBlockStore(ctx, blockStore)

	// Create a share referencing this block store
	share := &models.Share{
		ID:                uuid.New().String(),
		Name:              "/test-share",
		MetadataStoreID:   metaStore.ID,
		LocalBlockStoreID: blockStore.ID,
		DefaultPermission: "read-write",
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	cpStore.CreateShare(ctx, share)

	// Try to delete the in-use block store
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/store/block/local/in-use-store", nil)
	req = withBlockStoreKindAndName(req, "local", "in-use-store")
	w := httptest.NewRecorder()

	handler.Delete(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("Delete(in-use) status = %d, want %d, body = %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestBlockStoreHandler_Delete_NotFound(t *testing.T) {
	_, handler := setupBlockStoreTest(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/store/block/local/nonexistent", nil)
	req = withBlockStoreKindAndName(req, "local", "nonexistent")
	w := httptest.NewRecorder()

	handler.Delete(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Delete(nonexistent) status = %d, want %d, body = %s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

// --- Share + Block Store integration tests ---

func setupShareBlockStoreTest(t *testing.T) (store.Store, *ShareHandler) {
	t.Helper()

	dbConfig := store.Config{
		Type: "sqlite",
		SQLite: store.SQLiteConfig{
			Path: ":memory:",
		},
	}
	cpStore, err := store.New(&dbConfig)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	handler := NewShareHandler(cpStore, nil)
	return cpStore, handler
}

func TestShareBlockStore_CreateWithLocal(t *testing.T) {
	cpStore, handler := setupShareBlockStoreTest(t)
	ctx := context.Background()

	// Create prerequisite stores
	metaStore := &models.MetadataStoreConfig{
		ID: uuid.New().String(), Name: "meta-1", Type: "memory",
		CreatedAt: time.Now(),
	}
	cpStore.CreateMetadataStore(ctx, metaStore)

	localStore := &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "local-fs", Kind: models.BlockStoreKindLocal, Type: "fs",
		CreatedAt: time.Now(),
	}
	cpStore.CreateBlockStore(ctx, localStore)

	remoteStore := &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "remote-s3", Kind: models.BlockStoreKindRemote, Type: "s3",
		CreatedAt: time.Now(),
	}
	cpStore.CreateBlockStore(ctx, remoteStore)

	remoteName := "remote-s3"
	body, _ := json.Marshal(CreateShareRequest{
		Name:              "/test-export",
		MetadataStoreID:   "meta-1",
		LocalBlockStore:   "local-fs",
		RemoteBlockStore:  &remoteName,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("Create() status = %d, want %d, body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp ShareResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	if resp.LocalBlockStoreID != localStore.ID {
		t.Errorf("LocalBlockStoreID = %s, want %s", resp.LocalBlockStoreID, localStore.ID)
	}
	if resp.RemoteBlockStoreID == nil || *resp.RemoteBlockStoreID != remoteStore.ID {
		t.Errorf("RemoteBlockStoreID = %v, want %s", resp.RemoteBlockStoreID, remoteStore.ID)
	}
}

func TestShareBlockStore_CreateMissingLocal(t *testing.T) {
	cpStore, handler := setupShareBlockStoreTest(t)
	ctx := context.Background()

	// Create prerequisite metadata store only
	metaStore := &models.MetadataStoreConfig{
		ID: uuid.New().String(), Name: "meta-1", Type: "memory",
		CreatedAt: time.Now(),
	}
	cpStore.CreateMetadataStore(ctx, metaStore)

	body, _ := json.Marshal(CreateShareRequest{
		Name:            "/test-export",
		MetadataStoreID: "meta-1",
		// No local block store -- should fail
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.Create(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Create(missing local) status = %d, want %d, body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestShareBlockStore_CreateLocalOnly(t *testing.T) {
	cpStore, handler := setupShareBlockStoreTest(t)
	ctx := context.Background()

	// Create prerequisite stores
	metaStore := &models.MetadataStoreConfig{
		ID: uuid.New().String(), Name: "meta-1", Type: "memory",
		CreatedAt: time.Now(),
	}
	cpStore.CreateMetadataStore(ctx, metaStore)

	localStore := &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "local-fs", Kind: models.BlockStoreKindLocal, Type: "fs",
		CreatedAt: time.Now(),
	}
	cpStore.CreateBlockStore(ctx, localStore)

	body, _ := json.Marshal(CreateShareRequest{
		Name:            "/test-export-local",
		MetadataStoreID: "meta-1",
		LocalBlockStore: "local-fs",
		// remote_block_store is null -- local-only share
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("Create(local-only) status = %d, want %d, body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp ShareResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	if resp.LocalBlockStoreID != localStore.ID {
		t.Errorf("LocalBlockStoreID = %s, want %s", resp.LocalBlockStoreID, localStore.ID)
	}
	if resp.RemoteBlockStoreID != nil {
		t.Errorf("RemoteBlockStoreID = %v, want nil (local-only share)", resp.RemoteBlockStoreID)
	}
}

func TestBlockStoreHandler_Update_DoesNotChangeKind(t *testing.T) {
	cpStore, handler := setupBlockStoreTest(t)
	ctx := context.Background()

	// Create a local block store
	blockStore := &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "update-test", Kind: models.BlockStoreKindLocal, Type: "fs",
		CreatedAt: time.Now(),
	}
	cpStore.CreateBlockStore(ctx, blockStore)

	// Update the store's type but kind should stay local
	newType := "memory"
	body, _ := json.Marshal(UpdateBlockStoreRequest{
		Type: &newType,
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/store/block/local/update-test", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withBlockStoreKindAndName(req, "local", "update-test")
	w := httptest.NewRecorder()

	handler.Update(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Update() status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp BlockStoreResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	if resp.Kind != models.BlockStoreKindLocal {
		t.Errorf("Kind after update = %s, want local (unchanged)", resp.Kind)
	}
	if resp.Type != "memory" {
		t.Errorf("Type after update = %s, want memory", resp.Type)
	}
}
