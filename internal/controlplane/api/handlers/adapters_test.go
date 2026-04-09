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
)

// setupAdapterTest wires an in-memory control-plane store, a real
// runtime (no adapter started), and an AdapterHandler. No adapter is
// running, so the status field defaults to "unknown" which is the
// correct state for a stored-but-not-loaded adapter config.
func setupAdapterTest(t *testing.T) (store.Store, *runtime.Runtime, *AdapterHandler) {
	t.Helper()

	cfg := store.Config{
		Type:   "sqlite",
		SQLite: store.SQLiteConfig{Path: ":memory:"},
	}
	cpStore, err := store.New(&cfg)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	rt := runtime.New(cpStore)
	handler := NewAdapterHandler(rt)
	return cpStore, rt, handler
}

func withAdapterType(r *http.Request, adapterType string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("type", adapterType)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

func createAdapterConfig(t *testing.T, cpStore store.Store, adapterType string) {
	t.Helper()
	ctx := context.Background()
	_, err := cpStore.CreateAdapter(ctx, &models.AdapterConfig{
		ID:        uuid.New().String(),
		Type:      adapterType,
		Enabled:   false,
		Port:      0,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("CreateAdapter: %v", err)
	}
}

func TestAdapterHandler_Status_OK(t *testing.T) {
	cpStore, _, handler := setupAdapterTest(t)
	createAdapterConfig(t, cpStore, "nfs")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/adapters/nfs/status", nil)
	req = withAdapterType(req, "nfs")
	w := httptest.NewRecorder()

	handler.Status(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Status = %d, want 200, body=%s", w.Code, w.Body.String())
	}

	var rep health.Report
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Adapter is configured but not running in this fixture; the
	// runtime checker surfaces that as StatusUnknown with a
	// "not running" message. Asserting on the exact status pins
	// the documented semantics for a stored-but-stopped adapter.
	if rep.Status != health.StatusUnknown {
		t.Errorf("Status.Status = %s, want unknown (adapter not running)", rep.Status)
	}
}

func TestAdapterHandler_Status_NotFound(t *testing.T) {
	_, _, handler := setupAdapterTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/adapters/missing/status", nil)
	req = withAdapterType(req, "missing")
	w := httptest.NewRecorder()

	handler.Status(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("Status(missing) = %d, want 404", w.Code)
	}
}

func TestAdapterHandler_List_IncludesStatus(t *testing.T) {
	cpStore, _, handler := setupAdapterTest(t)
	createAdapterConfig(t, cpStore, "nfs")
	createAdapterConfig(t, cpStore, "smb")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/adapters", nil)
	w := httptest.NewRecorder()
	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("List = %d, want 200", w.Code)
	}
	var resp []AdapterResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) != 2 {
		t.Fatalf("len = %d, want 2", len(resp))
	}
	for i, r := range resp {
		if !isValidHealthStatus(r.Status.Status) {
			t.Errorf("resp[%d].Status.Status = %q, want valid health.Status", i, r.Status.Status)
		}
	}
}

func TestAdapterHandler_Get_IncludesStatus(t *testing.T) {
	cpStore, _, handler := setupAdapterTest(t)
	createAdapterConfig(t, cpStore, "nfs")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/adapters/nfs", nil)
	req = withAdapterType(req, "nfs")
	w := httptest.NewRecorder()
	handler.Get(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Get = %d, want 200", w.Code)
	}
	var resp AdapterResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !isValidHealthStatus(resp.Status.Status) {
		t.Errorf("Get.Status.Status = %q, want valid health.Status", resp.Status.Status)
	}
}
