//go:build integration

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/health"
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

	handler := NewBlockStoreHandler(cpStore, nil)
	return cpStore, handler
}

// withBlockStoreName creates a request with the chi URL param "name" set.
func withBlockStoreName(r *http.Request, name string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", name)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

func TestBlockStoreHandler_Create(t *testing.T) {
	_, handler := setupBlockStoreTest(t)

	body, _ := json.Marshal(CreateBlockStoreRequest{
		Name: "test-store",
		Type: "memory",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/store/block", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("Create() status = %d, want %d, body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp BlockStoreResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	if resp.Name != "test-store" {
		t.Errorf("Name = %s, want test-store", resp.Name)
	}
	if resp.Type != "memory" {
		t.Errorf("Type = %s, want memory", resp.Type)
	}
}

func TestBlockStoreHandler_Create_S3_MissingCredentials(t *testing.T) {
	_, handler := setupBlockStoreTest(t)

	tests := []struct {
		name   string
		config string
	}{
		{"missing bucket", `{"access_key_id":"k","secret_access_key":"s"}`},
		{"missing access_key_id", `{"bucket":"b","secret_access_key":"s"}`},
		{"missing secret_access_key", `{"bucket":"b","access_key_id":"k"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(CreateBlockStoreRequest{
				Name:   "s3-" + tt.name,
				Type:   "s3",
				Config: tt.config,
			})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/store/block", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
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

	memStore := &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "mem-1", Type: "memory",
		CreatedAt: time.Now(),
	}
	s3Store := &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "s3-1", Type: "s3",
		CreatedAt: time.Now(),
	}
	cpStore.CreateBlockStore(ctx, memStore)
	cpStore.CreateBlockStore(ctx, s3Store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/block", nil)
	w := httptest.NewRecorder()

	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("List() status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp []BlockStoreResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	if len(resp) != 2 {
		t.Errorf("List() returned %d items, want 2", len(resp))
	}
}

func TestBlockStoreHandler_Get_NotFound(t *testing.T) {
	_, handler := setupBlockStoreTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/block/nonexistent", nil)
	req = withBlockStoreName(req, "nonexistent")
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
		ID: uuid.New().String(), Name: "in-use-store", Type: "fs",
		CreatedAt: time.Now(),
	}
	cpStore.CreateBlockStore(ctx, blockStore)

	// Create a share referencing this block store
	share := &models.Share{
		ID:                uuid.New().String(),
		Name:              "/test-share",
		MetadataStoreID:   metaStore.ID,
		BlockStoreID:      blockStore.ID,
		DefaultPermission: "read-write",
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	cpStore.CreateShare(ctx, share)

	// Try to delete the in-use block store
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/store/block/in-use-store", nil)
	req = withBlockStoreName(req, "in-use-store")
	w := httptest.NewRecorder()

	handler.Remove(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("Delete(in-use) status = %d, want %d, body = %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestBlockStoreHandler_Delete_NotFound(t *testing.T) {
	_, handler := setupBlockStoreTest(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/store/block/nonexistent", nil)
	req = withBlockStoreName(req, "nonexistent")
	w := httptest.NewRecorder()

	handler.Remove(w, req)

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

func TestShareBlockStore_CreateWithBlockStore(t *testing.T) {
	cpStore, handler := setupShareBlockStoreTest(t)
	ctx := context.Background()

	// Create prerequisite stores
	metaStore := &models.MetadataStoreConfig{
		ID: uuid.New().String(), Name: "meta-1", Type: "memory",
		CreatedAt: time.Now(),
	}
	cpStore.CreateMetadataStore(ctx, metaStore)

	blockStore := &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "s3-store", Type: "s3",
		CreatedAt: time.Now(),
	}
	cpStore.CreateBlockStore(ctx, blockStore)

	body, _ := json.Marshal(CreateShareRequest{
		Name:            "/test-export",
		MetadataStoreID: "meta-1",
		BlockStore:      "s3-store",
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
	if resp.BlockStoreID != blockStore.ID {
		t.Errorf("BlockStoreID = %s, want %s", resp.BlockStoreID, blockStore.ID)
	}
}

func TestShareBlockStore_CreateMissingBlockStore(t *testing.T) {
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

func TestBlockStoreHandler_HealthCheck_LocalMemory(t *testing.T) {
	cpStore, handler := setupBlockStoreTest(t)
	ctx := context.Background()

	bs := &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "mem-local", Type: "memory",
		CreatedAt: time.Now(),
	}
	cpStore.CreateBlockStore(ctx, bs)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/block/mem-local/health", nil)
	req = withBlockStoreName(req, "mem-local")
	w := httptest.NewRecorder()

	handler.HealthCheck(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("HealthCheck(local/memory) status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp BlockStoreHealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	if !resp.Healthy {
		t.Errorf("Expected healthy=true, got false")
	}
	if resp.CheckedAt == "" {
		t.Error("Expected checked_at to be set")
	}
}

func TestBlockStoreHandler_HealthCheck_RemoteMemory(t *testing.T) {
	cpStore, handler := setupBlockStoreTest(t)
	ctx := context.Background()

	bs := &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "mem-remote", Type: "memory",
		CreatedAt: time.Now(),
	}
	cpStore.CreateBlockStore(ctx, bs)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/block/mem-remote/health", nil)
	req = withBlockStoreName(req, "mem-remote")
	w := httptest.NewRecorder()

	handler.HealthCheck(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("HealthCheck(remote/memory) status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp BlockStoreHealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	if !resp.Healthy {
		t.Errorf("Expected healthy=true, got false")
	}
}

func TestBlockStoreHandler_HealthCheck_NotFound(t *testing.T) {
	_, handler := setupBlockStoreTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/block/nonexistent/health", nil)
	req = withBlockStoreName(req, "nonexistent")
	w := httptest.NewRecorder()

	handler.HealthCheck(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("HealthCheck(not found) status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func setupBlockStoreTestWithRuntime(t *testing.T) (store.Store, *BlockStoreHandler) {
	t.Helper()

	dbConfig := store.Config{
		Type:   "sqlite",
		SQLite: store.SQLiteConfig{Path: ":memory:"},
	}
	cpStore, err := store.New(&dbConfig)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	rt := runtime.New(cpStore)
	return cpStore, NewBlockStoreHandler(cpStore, rt)
}

func TestBlockStoreHandler_Status_OK(t *testing.T) {
	cpStore, handler := setupBlockStoreTestWithRuntime(t)
	ctx := context.Background()

	bs := &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "mem-local", Type: "memory",
		CreatedAt: time.Now(),
	}
	cpStore.CreateBlockStore(ctx, bs)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/block/mem-local/status", nil)
	req = withBlockStoreName(req, "mem-local")
	w := httptest.NewRecorder()

	handler.Status(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Status(mem-local) status = %d, want 200, body = %s", w.Code, w.Body.String())
	}
	var rep health.Report
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("Failed to unmarshal status report: %v", err)
	}
	if rep.Status != health.StatusHealthy {
		t.Errorf("memory store Status = %s, want healthy", rep.Status)
	}
	if rep.CheckedAt.IsZero() {
		t.Error("CheckedAt is zero; expected a populated timestamp")
	}
}

func TestBlockStoreHandler_Status_NotFound(t *testing.T) {
	_, handler := setupBlockStoreTestWithRuntime(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/block/nope/status", nil)
	req = withBlockStoreName(req, "nope")
	w := httptest.NewRecorder()

	handler.Status(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("Status(nope) status = %d, want 404, body = %s", w.Code, w.Body.String())
	}
}

func TestBlockStoreHandler_List_IncludesStatus(t *testing.T) {
	cpStore, handler := setupBlockStoreTestWithRuntime(t)
	ctx := context.Background()

	cpStore.CreateBlockStore(ctx, &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "a", Type: "memory",
		CreatedAt: time.Now(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/block", nil)
	w := httptest.NewRecorder()

	handler.List(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("List status = %d, want 200", w.Code)
	}
	var resp []BlockStoreResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) == 0 {
		t.Fatalf("empty list")
	}
	if !isValidHealthStatus(resp[0].Status.Status) {
		t.Errorf("list[0].Status.Status = %q, expected a valid health.Status", resp[0].Status.Status)
	}
}

func TestBlockStoreHandler_Get_IncludesStatus(t *testing.T) {
	cpStore, handler := setupBlockStoreTestWithRuntime(t)
	ctx := context.Background()

	cpStore.CreateBlockStore(ctx, &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "g", Type: "memory",
		CreatedAt: time.Now(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/block/g", nil)
	req = withBlockStoreName(req, "g")
	w := httptest.NewRecorder()

	handler.Get(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Get status = %d, want 200", w.Code)
	}
	var resp BlockStoreResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !isValidHealthStatus(resp.Status.Status) {
		t.Errorf("Get.Status.Status = %q, expected a valid health.Status", resp.Status.Status)
	}
}

// isValidHealthStatus returns true if s is one of the enumerated
// health.Status values. Used across U-E wire tests to assert that
// status fields are populated without pinning the exact value (since
// some fixtures start an adapter, others don't).
func isValidHealthStatus(s health.Status) bool {
	switch s {
	case health.StatusHealthy,
		health.StatusDegraded,
		health.StatusUnhealthy,
		health.StatusUnknown,
		health.StatusDisabled:
		return true
	default:
		return false
	}
}

// TestBlockStoreHandler_Update_EncryptionCannotBeRemoved pins the
// one-way door: blocks already written to an encrypted store carry an
// encryption frame, and an undecorated store would hand that framed
// ciphertext back as plaintext, so an update that drops the policy is
// refused. Changing the policy in place stays allowed.
func TestBlockStoreHandler_Update_EncryptionCannotBeRemoved(t *testing.T) {
	const encrypted = `{"bucket":"b","region":"us-east-1","access_key_id":"AK","secret_access_key":"SK",` +
		`"encryption":{"algorithm":"aes-256-gcm","key":{"kind":"local","file":"/k.pem"}}}`

	update := func(t *testing.T, initial, cfg string) *httptest.ResponseRecorder {
		t.Helper()
		cpStore, handler := setupBlockStoreTest(t)
		bs := &models.BlockStoreConfig{
			ID: uuid.New().String(), Name: "enc-test",
			Type: "s3", Config: initial, CreatedAt: time.Now(),
		}
		if _, err := cpStore.CreateBlockStore(context.Background(), bs); err != nil {
			t.Fatalf("CreateBlockStore: %v", err)
		}
		body, _ := json.Marshal(UpdateBlockStoreRequest{Config: &cfg})
		req := httptest.NewRequest(http.MethodPut, "/api/v1/store/block/enc-test", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = withBlockStoreName(req, "enc-test")
		w := httptest.NewRecorder()
		handler.Update(w, req)
		return w
	}

	t.Run("removal rejected", func(t *testing.T) {
		w := update(t, encrypted, `{"bucket":"b","region":"eu-west-1","access_key_id":"AK","secret_access_key":"SK"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d, body = %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
	})

	t.Run("policy change allowed", func(t *testing.T) {
		w := update(t, encrypted, `{"bucket":"b","region":"eu-west-1","access_key_id":"AK","secret_access_key":"SK",`+
			`"encryption":{"algorithm":"aes-256-gcm","key":{"kind":"local","file":"/k2.pem"}}}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
		}
	})

	t.Run("nulling the policy is removal", func(t *testing.T) {
		w := update(t, encrypted, `{"bucket":"b","region":"eu-west-1","access_key_id":"AK","secret_access_key":"SK","encryption":null}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d, body = %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
	})

	t.Run("unparseable config reports the parse error", func(t *testing.T) {
		w := update(t, encrypted, `not json`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d, body = %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "cannot be removed") {
			t.Fatalf("invalid JSON reported as an encryption removal: %s", w.Body.String())
		}
	})

	t.Run("never encrypted stays editable", func(t *testing.T) {
		w := update(t, `{"bucket":"b","region":"us-east-1","access_key_id":"AK","secret_access_key":"SK"}`, `{"bucket":"b","region":"eu-west-1","access_key_id":"AK","secret_access_key":"SK"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
		}
	})
}

// A PUT that changes the config and renames onto a name already in use answers
// 409. The rename runs after the config write, so without a preflight the 409
// would be returned with the new config already committed — a reply that reads
// as "nothing happened" against a store that changed.
func TestBlockStoreHandler_Update_ConflictingRenameLeavesConfigUntouched(t *testing.T) {
	cpStore, handler := setupBlockStoreTest(t)
	ctx := context.Background()

	const original = `{"bucket":"b","region":"us-east-1","access_key_id":"AK","secret_access_key":"SK"}`
	if _, err := cpStore.CreateBlockStore(ctx, &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "blocks-a",
		Type: "s3", Config: original, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateBlockStore blocks-a: %v", err)
	}
	if _, err := cpStore.CreateBlockStore(ctx, &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "blocks-b",
		Type: "s3", Config: original, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateBlockStore blocks-b: %v", err)
	}

	taken := "blocks-b"
	rotated := `{"bucket":"b","region":"us-east-1","access_key_id":"AK2","secret_access_key":"SK2"}`
	body, _ := json.Marshal(UpdateBlockStoreRequest{Name: &taken, Config: &rotated})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/store/block/blocks-a", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withBlockStoreName(req, "blocks-a")
	w := httptest.NewRecorder()
	handler.Update(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d, body = %s", w.Code, http.StatusConflict, w.Body.String())
	}

	after, err := cpStore.GetBlockStore(ctx, "blocks-a")
	if err != nil {
		t.Fatalf("GetBlockStore blocks-a: %v", err)
	}
	if after.Config != original {
		t.Errorf("config changed despite the 409: got %s, want %s", after.Config, original)
	}
}

// The preflight resolves its target with a name-or-ID lookup, but only a name
// collides. A rename whose target resolves to a row under some other key — the
// store's own UUID here — is not a conflict and must pass through.
func TestBlockStoreHandler_Update_RenameTargetResolvingByIDIsNotAConflict(t *testing.T) {
	cpStore, handler := setupBlockStoreTest(t)
	ctx := context.Background()

	id := uuid.New().String()
	if _, err := cpStore.CreateBlockStore(ctx, &models.BlockStoreConfig{
		ID: id, Name: "blocks-self",
		Type: "s3", Config: `{"bucket":"b","region":"us-east-1","access_key_id":"AK","secret_access_key":"SK"}`,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	body, _ := json.Marshal(UpdateBlockStoreRequest{Name: &id})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/store/block/blocks-self", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withBlockStoreName(req, "blocks-self")
	w := httptest.NewRecorder()
	handler.Update(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}
}
