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

// setupShareTestWithRuntime wires an in-memory store + real runtime
// and a ShareHandler. The runtime has no shares loaded, so share
// status probes resolve to "share not found" → StatusUnknown, which
// is the documented contract for a DB-configured-but-not-loaded share.
func setupShareTestWithRuntime(t *testing.T) (store.Store, *runtime.Runtime, *ShareHandler) {
	t.Helper()

	cfg := store.Config{
		Type:   "sqlite",
		SQLite: store.SQLiteConfig{Path: ":memory:"},
	}
	cpStore, err := store.New(&cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	rt := runtime.New(cpStore)
	handler := NewShareHandler(cpStore, rt)
	return cpStore, rt, handler
}

// seedShare creates the minimal metadata store + local block store
// + share rows required for ShareHandler.Get / List / Status to
// succeed on the DB side. Returns the created share name.
func seedShare(t *testing.T, cpStore store.Store, name string) string {
	t.Helper()
	ctx := context.Background()

	metaStore := &models.MetadataStoreConfig{
		ID: uuid.New().String(), Name: "m-" + name, Type: "memory",
		CreatedAt: time.Now(),
	}
	if _, err := cpStore.CreateMetadataStore(ctx, metaStore); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}

	blockStore := &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "b-" + name, Kind: models.BlockStoreKindLocal, Type: "memory",
		CreatedAt: time.Now(),
	}
	if _, err := cpStore.CreateBlockStore(ctx, blockStore); err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	share := &models.Share{
		ID:                uuid.New().String(),
		Name:              "/" + name,
		MetadataStoreID:   metaStore.ID,
		LocalBlockStoreID: blockStore.ID,
		DefaultPermission: "read-write",
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	if _, err := cpStore.CreateShare(ctx, share); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	return share.Name
}

func withShareName(r *http.Request, name string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", name)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

func TestShareHandler_Status_OK(t *testing.T) {
	cpStore, _, handler := setupShareTestWithRuntime(t)
	shareName := seedShare(t, cpStore, "s-ok")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/shares"+shareName+"/status", nil)
	req = withShareName(req, "s-ok")
	w := httptest.NewRecorder()

	handler.Status(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var rep health.Report
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The share config exists but the runtime hasn't loaded it, so
	// the worst-of probe sees "share not found" and reports Unknown.
	if rep.Status != health.StatusUnknown {
		t.Errorf("Status.Status = %s, want unknown (runtime share not loaded)", rep.Status)
	}
}

func TestShareHandler_Status_NotFound(t *testing.T) {
	_, _, handler := setupShareTestWithRuntime(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/shares/missing/status", nil)
	req = withShareName(req, "missing")
	w := httptest.NewRecorder()

	handler.Status(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("Status(missing) = %d, want 404", w.Code)
	}
}

func TestShareHandler_List_IncludesStatus(t *testing.T) {
	cpStore, _, handler := setupShareTestWithRuntime(t)
	seedShare(t, cpStore, "s-list")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/shares", nil)
	w := httptest.NewRecorder()
	handler.List(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("List = %d, want 200", w.Code)
	}
	var resp []ShareResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) == 0 {
		t.Fatalf("empty list")
	}
	if !isValidHealthStatus(resp[0].Status.Status) {
		t.Errorf("resp[0].Status.Status = %q, want valid health.Status", resp[0].Status.Status)
	}
}

func TestShareHandler_Get_IncludesStatus(t *testing.T) {
	cpStore, _, handler := setupShareTestWithRuntime(t)
	seedShare(t, cpStore, "s-get")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/shares/s-get", nil)
	req = withShareName(req, "s-get")
	w := httptest.NewRecorder()
	handler.Get(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Get = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp ShareResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !isValidHealthStatus(resp.Status.Status) {
		t.Errorf("Get.Status.Status = %q, want valid health.Status", resp.Status.Status)
	}
}
