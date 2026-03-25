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
