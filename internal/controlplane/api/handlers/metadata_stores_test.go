//go:build integration

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/health"
	memoryMeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

func setupMetadataStoreHealthTest(t *testing.T) (store.Store, *MetadataStoreHandler, *runtime.Runtime) {
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

	rt := runtime.New(cpStore)
	handler := NewMetadataStoreHandler(cpStore, rt)
	return cpStore, handler, rt
}

func withMetadataStoreName(r *http.Request, name string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", name)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

func TestMetadataStoreHandler_HealthCheck_Loaded(t *testing.T) {
	cpStore, handler, rt := setupMetadataStoreHealthTest(t)
	ctx := context.Background()

	// Create store config in DB
	cfg := &models.MetadataStoreConfig{
		ID: uuid.New().String(), Name: "test-meta", Type: "memory",
		CreatedAt: time.Now(),
	}
	cpStore.CreateMetadataStore(ctx, cfg)

	// Register a running store in the runtime
	metaStore := memoryMeta.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("test-meta", metaStore); err != nil {
		t.Fatalf("Failed to register metadata store: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/metadata/test-meta/health", nil)
	req = withMetadataStoreName(req, "test-meta")
	w := httptest.NewRecorder()

	handler.HealthCheck(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("HealthCheck(loaded) status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp MetadataStoreHealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	if !resp.Healthy {
		t.Errorf("Expected healthy=true for loaded store, got false")
	}
	if resp.CheckedAt == "" {
		t.Error("Expected checked_at to be set")
	}
}

func TestMetadataStoreHandler_HealthCheck_NotLoaded(t *testing.T) {
	cpStore, handler, _ := setupMetadataStoreHealthTest(t)
	ctx := context.Background()

	// Create store config in DB but do NOT load it in the runtime
	cfg := &models.MetadataStoreConfig{
		ID: uuid.New().String(), Name: "unloaded-meta", Type: "badger",
		Config:    `{"path":"/tmp/test"}`,
		CreatedAt: time.Now(),
	}
	cpStore.CreateMetadataStore(ctx, cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/metadata/unloaded-meta/health", nil)
	req = withMetadataStoreName(req, "unloaded-meta")
	w := httptest.NewRecorder()

	handler.HealthCheck(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("HealthCheck(not loaded) status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp MetadataStoreHealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	if resp.Healthy {
		t.Errorf("Expected healthy=false for unloaded store")
	}
}

func TestMetadataStoreHandler_HealthCheck_NotFound(t *testing.T) {
	_, handler, _ := setupMetadataStoreHealthTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/metadata/nonexistent/health", nil)
	req = withMetadataStoreName(req, "nonexistent")
	w := httptest.NewRecorder()

	handler.HealthCheck(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("HealthCheck(not found) status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestMetadataStoreHandler_HealthCheck_NoRuntime(t *testing.T) {
	dbConfig := store.Config{
		Type:   "sqlite",
		SQLite: store.SQLiteConfig{Path: ":memory:"},
	}
	cpStore, err := store.New(&dbConfig)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	// Handler with nil runtime
	handler := NewMetadataStoreHandler(cpStore, nil)
	ctx := context.Background()

	cfg := &models.MetadataStoreConfig{
		ID: uuid.New().String(), Name: "no-rt-meta", Type: "memory",
		CreatedAt: time.Now(),
	}
	cpStore.CreateMetadataStore(ctx, cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/metadata/no-rt-meta/health", nil)
	req = withMetadataStoreName(req, "no-rt-meta")
	w := httptest.NewRecorder()

	handler.HealthCheck(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("HealthCheck(nil runtime) status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp MetadataStoreHealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	if resp.Healthy {
		t.Errorf("Expected healthy=false when runtime is nil")
	}
}

// --- U-E status tests ---

func TestMetadataStoreHandler_Status_OK(t *testing.T) {
	cpStore, handler, rt := setupMetadataStoreHealthTest(t)
	ctx := context.Background()

	cfg := &models.MetadataStoreConfig{
		ID: uuid.New().String(), Name: "m-ok", Type: "memory",
		CreatedAt: time.Now(),
	}
	cpStore.CreateMetadataStore(ctx, cfg)
	if err := rt.RegisterMetadataStore("m-ok", memoryMeta.NewMemoryMetadataStoreWithDefaults()); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/metadata/m-ok/status", nil)
	req = withMetadataStoreName(req, "m-ok")
	w := httptest.NewRecorder()

	handler.Status(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var rep health.Report
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rep.Status != health.StatusHealthy {
		t.Errorf("Status = %s, want healthy", rep.Status)
	}
}

func TestMetadataStoreHandler_Status_NotFound(t *testing.T) {
	_, handler, _ := setupMetadataStoreHealthTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/metadata/missing/status", nil)
	req = withMetadataStoreName(req, "missing")
	w := httptest.NewRecorder()

	handler.Status(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("Status(missing) = %d, want 404", w.Code)
	}
}

func TestMetadataStoreHandler_List_IncludesStatus(t *testing.T) {
	cpStore, handler, rt := setupMetadataStoreHealthTest(t)
	ctx := context.Background()

	cpStore.CreateMetadataStore(ctx, &models.MetadataStoreConfig{
		ID: uuid.New().String(), Name: "m-list", Type: "memory",
		CreatedAt: time.Now(),
	})
	_ = rt.RegisterMetadataStore("m-list", memoryMeta.NewMemoryMetadataStoreWithDefaults())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/metadata", nil)
	w := httptest.NewRecorder()
	handler.List(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("List = %d, want 200", w.Code)
	}
	var resp []MetadataStoreResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) == 0 {
		t.Fatalf("empty list")
	}
	if !isValidHealthStatus(resp[0].Status.Status) {
		t.Errorf("list[0].Status.Status = %q, want a valid health.Status", resp[0].Status.Status)
	}
}

func TestMetadataStoreHandler_Get_IncludesStatus(t *testing.T) {
	cpStore, handler, rt := setupMetadataStoreHealthTest(t)
	ctx := context.Background()

	cpStore.CreateMetadataStore(ctx, &models.MetadataStoreConfig{
		ID: uuid.New().String(), Name: "m-get", Type: "memory",
		CreatedAt: time.Now(),
	})
	_ = rt.RegisterMetadataStore("m-get", memoryMeta.NewMemoryMetadataStoreWithDefaults())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/store/metadata/m-get", nil)
	req = withMetadataStoreName(req, "m-get")
	w := httptest.NewRecorder()
	handler.Get(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Get = %d, want 200", w.Code)
	}
	var resp MetadataStoreResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !isValidHealthStatus(resp.Status.Status) {
		t.Errorf("Get.Status.Status = %q, want a valid health.Status", resp.Status.Status)
	}
}
