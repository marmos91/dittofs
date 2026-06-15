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

// setupShareNFSConfigTest builds a GORM sqlite store with one share and returns
// a handler (runtime nil — the handler tolerates a nil runtime, persistence is
// still exercised).
func setupShareNFSConfigTest(t *testing.T) (*store.GORMStore, *ShareNFSConfigHandler, string) {
	t.Helper()

	cpStore, err := store.New(&store.Config{
		Type:   "sqlite",
		SQLite: store.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	ctx := context.Background()

	metaStore := &models.MetadataStoreConfig{ID: uuid.New().String(), Name: "m", Type: "memory"}
	if _, err := cpStore.CreateMetadataStore(ctx, metaStore); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	localBlockStore := &models.BlockStoreConfig{ID: uuid.New().String(), Name: "l", Kind: models.BlockStoreKindLocal, Type: "fs"}
	if _, err := cpStore.CreateBlockStore(ctx, localBlockStore); err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}
	share := &models.Share{
		ID:                uuid.New().String(),
		Name:              "/export",
		MetadataStoreID:   metaStore.ID,
		LocalBlockStoreID: localBlockStore.ID,
		CreatedAt:         time.Now(),
	}
	if _, err := cpStore.CreateShare(ctx, share); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	handler := NewShareNFSConfigHandler(struct {
		store.ShareStore
		store.NetgroupStore
	}{cpStore, cpStore}, nil)

	return cpStore, handler, share.ID
}

// doRequest runs a request against a handler func and returns the recorder.
func doRequest(t *testing.T, h func(http.ResponseWriter, *http.Request), method, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/api/v1/shares/export/adapters/nfs/config", nil)
	} else {
		r = httptest.NewRequest(method, "/api/v1/shares/export/adapters/nfs/config", bytes.NewReader([]byte(body)))
		r.Header.Set("Content-Type", "application/json")
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "export")
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func TestShareNFSConfig_GetDefaults(t *testing.T) {
	_, handler, _ := setupShareNFSConfigTest(t)

	w := doRequest(t, handler.Get, http.MethodGet, "")
	if w.Code != http.StatusOK {
		t.Fatalf("Get() status = %d, want 200, body = %s", w.Code, w.Body.String())
	}

	var resp ShareNFSConfigResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Netgroup != "" {
		t.Errorf("default Netgroup = %q, want empty", resp.Netgroup)
	}
	if !resp.AllowAuthSys {
		t.Errorf("default AllowAuthSys = false, want true")
	}
}

func TestShareNFSConfig_GetShareNotFound(t *testing.T) {
	_, handler, _ := setupShareNFSConfigTest(t)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/shares/missing/adapters/nfs/config", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "missing")
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	handler.Get(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("Get(missing share) status = %d, want 404", w.Code)
	}
}

func TestShareNFSConfig_PatchAssociatesNetgroup(t *testing.T) {
	cpStore, handler, shareID := setupShareNFSConfigTest(t)
	ctx := context.Background()

	if _, err := cpStore.CreateNetgroup(ctx, &models.Netgroup{Name: "office"}); err != nil {
		t.Fatalf("CreateNetgroup: %v", err)
	}

	w := doRequest(t, handler.Patch, http.MethodPatch, `{"netgroup":"office"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("Patch() status = %d, want 200, body = %s", w.Code, w.Body.String())
	}

	var resp ShareNFSConfigResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Netgroup != "office" {
		t.Errorf("Netgroup = %q, want office", resp.Netgroup)
	}

	// Verify persistence: stored config holds the netgroup ID.
	cfg, err := cpStore.GetShareAdapterConfig(ctx, shareID, "nfs")
	if err != nil || cfg == nil {
		t.Fatalf("GetShareAdapterConfig: cfg=%v err=%v", cfg, err)
	}
	var opts models.NFSExportOptions
	if err := cfg.ParseConfig(&opts); err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if opts.NetgroupID == nil || *opts.NetgroupID == "" {
		t.Errorf("persisted NetgroupID = %v, want non-empty", opts.NetgroupID)
	}
}

func TestShareNFSConfig_PatchClearsNetgroup(t *testing.T) {
	cpStore, handler, shareID := setupShareNFSConfigTest(t)
	ctx := context.Background()

	ngID, err := cpStore.CreateNetgroup(ctx, &models.Netgroup{Name: "office"})
	if err != nil {
		t.Fatalf("CreateNetgroup: %v", err)
	}
	opts := models.DefaultNFSExportOptions()
	opts.NetgroupID = &ngID
	cfg := &models.ShareAdapterConfig{ShareID: shareID, AdapterType: "nfs"}
	if err := cfg.SetConfig(opts); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if err := cpStore.SetShareAdapterConfig(ctx, cfg); err != nil {
		t.Fatalf("SetShareAdapterConfig: %v", err)
	}

	// Clear via explicit empty string.
	w := doRequest(t, handler.Patch, http.MethodPatch, `{"netgroup":""}`)
	if w.Code != http.StatusOK {
		t.Fatalf("Patch(clear) status = %d, want 200, body = %s", w.Code, w.Body.String())
	}

	var resp ShareNFSConfigResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Netgroup != "" {
		t.Errorf("Netgroup = %q, want empty after clear", resp.Netgroup)
	}

	stored, err := cpStore.GetShareAdapterConfig(ctx, shareID, "nfs")
	if err != nil || stored == nil {
		t.Fatalf("GetShareAdapterConfig: cfg=%v err=%v", stored, err)
	}
	var storedOpts models.NFSExportOptions
	if err := stored.ParseConfig(&storedOpts); err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if storedOpts.NetgroupID != nil {
		t.Errorf("persisted NetgroupID = %v, want nil after clear", storedOpts.NetgroupID)
	}
}

func TestShareNFSConfig_PatchUnknownNetgroup(t *testing.T) {
	_, handler, _ := setupShareNFSConfigTest(t)

	w := doRequest(t, handler.Patch, http.MethodPatch, `{"netgroup":"does-not-exist"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("Patch(unknown netgroup) status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
}

func TestShareNFSConfig_PatchOtherFields(t *testing.T) {
	cpStore, handler, shareID := setupShareNFSConfigTest(t)
	ctx := context.Background()

	w := doRequest(t, handler.Patch, http.MethodPatch, `{"squash":"all_to_guest","allow_auth_sys":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("Patch() status = %d, want 200, body = %s", w.Code, w.Body.String())
	}

	cfg, err := cpStore.GetShareAdapterConfig(ctx, shareID, "nfs")
	if err != nil || cfg == nil {
		t.Fatalf("GetShareAdapterConfig: cfg=%v err=%v", cfg, err)
	}
	var opts models.NFSExportOptions
	if err := cfg.ParseConfig(&opts); err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if opts.Squash != "all_to_guest" {
		t.Errorf("Squash = %q, want all_to_guest", opts.Squash)
	}
	if opts.AllowAuthSys {
		t.Errorf("AllowAuthSys = true, want false")
	}
}

// TestShareNFSConfig_PatchRejectsInvalidSquash verifies an unrecognized squash
// value is rejected with 400 rather than silently persisted and then falling
// back to the default at use time (#1181). "root"/"all" are common mistakes —
// the real enum is none|root_to_admin|root_to_guest|all_to_admin|all_to_guest.
func TestShareNFSConfig_PatchRejectsInvalidSquash(t *testing.T) {
	_, handler, _ := setupShareNFSConfigTest(t)

	for _, bad := range []string{"root", "all", "read_write"} {
		w := doRequest(t, handler.Patch, http.MethodPatch, `{"squash":"`+bad+`"}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("Patch(squash=%q) status = %d, want 400, body = %s", bad, w.Code, w.Body.String())
		}
	}
}
