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
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
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
	localBlockStore := &models.BlockStoreConfig{ID: uuid.New().String(), Name: "l", Type: "fs"}
	if _, err := cpStore.CreateBlockStore(ctx, localBlockStore); err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}
	share := &models.Share{
		ID:              uuid.New().String(),
		Name:            "/export",
		MetadataStoreID: metaStore.ID,
		BlockStoreID:    localBlockStore.ID,
		CreatedAt:       time.Now(),
	}
	if _, err := cpStore.CreateShare(ctx, share); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	return cpStore, nfsConfigHandler(cpStore, nil), share.ID
}

// nfsConfigHandler builds a ShareNFSConfigHandler over cpStore, which satisfies
// both halves of the handler's composite store interface.
func nfsConfigHandler(cpStore *store.GORMStore, rt *runtime.Runtime) *ShareNFSConfigHandler {
	return NewShareNFSConfigHandler(struct {
		store.ShareStore
		store.NetgroupStore
	}{cpStore, cpStore}, rt)
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

// TestShareNFSConfig_PatchRequireKerberosNeedsKerberos pins the config-time
// guard: require_kerberos on a server without Kerberos leaves the share
// reachable by no auth flavor at all, and would force SECINFO to answer with a
// zero-length flavor list.
func TestShareNFSConfig_PatchRequireKerberosNeedsKerberos(t *testing.T) {
	cpStore, _, shareID := setupShareNFSConfigTest(t)
	rt := runtime.New(nil)
	handler := nfsConfigHandler(cpStore, rt)

	w := doRequest(t, handler.Patch, http.MethodPatch, `{"require_kerberos":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("Patch(require_kerberos) with Kerberos disabled status = %d, want 400, body = %s",
			w.Code, w.Body.String())
	}

	// Nothing was persisted by the refused request.
	cfg, err := cpStore.GetShareAdapterConfig(context.Background(), shareID, "nfs")
	if err != nil {
		t.Fatalf("GetShareAdapterConfig: %v", err)
	}
	if cfg != nil {
		var opts models.NFSExportOptions
		if err := cfg.ParseConfig(&opts); err != nil {
			t.Fatalf("ParseConfig: %v", err)
		}
		if opts.RequireKerberos {
			t.Errorf("refused request persisted require_kerberos=true")
		}
	}

	// With Kerberos configured the same request is accepted.
	rt.SetKerberosEnabled(true)
	w = doRequest(t, handler.Patch, http.MethodPatch, `{"require_kerberos":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("Patch(require_kerberos) with Kerberos enabled status = %d, want 200, body = %s",
			w.Code, w.Body.String())
	}
	var resp ShareNFSConfigResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.RequireKerberos {
		t.Errorf("RequireKerberos = false, want true")
	}
}

// setupShareNFSConfigTestWithRuntime is setupShareNFSConfigTest with a real
// Runtime holding a registered share, so a test can observe what the running
// adapter would read rather than only what was persisted.
func setupShareNFSConfigTestWithRuntime(t *testing.T) (*runtime.Runtime, *ShareNFSConfigHandler) {
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
	blockStore := &models.BlockStoreConfig{ID: uuid.New().String(), Name: "l", Type: "memory"}
	if _, err := cpStore.CreateBlockStore(ctx, blockStore); err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}
	share := &models.Share{
		ID:              uuid.New().String(),
		Name:            "/export",
		MetadataStoreID: metaStore.ID,
		BlockStoreID:    blockStore.ID,
		CreatedAt:       time.Now(),
	}
	if _, err := cpStore.CreateShare(ctx, share); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	rt := runtime.New(cpStore)
	rt.RegisterShareForTesting("/export")

	return rt, nfsConfigHandler(cpStore, rt)
}

// TestShareNFSConfig_PatchPushesExportPolicyToRunningShare covers the seam
// between persisting a config change and the adapter enforcing it.
//
// The export auth-flavor fields are read from the running share on the request
// path — the MNT auth gates and advertised flavor list, and the v4 auth check —
// so persisting them alone left the adapter enforcing the previous values until
// a restart. AllowAuthSys and RequireKerberos are security controls, so a
// tightened export kept accepting the flavor it now forbade while this endpoint
// reported success.
//
// Asserting persistence alone cannot catch that: the store is written correctly
// either way.
func TestShareNFSConfig_PatchPushesExportPolicyToRunningShare(t *testing.T) {
	rt, handler := setupShareNFSConfigTestWithRuntime(t)

	before, err := rt.GetShare("/export")
	if err != nil {
		t.Fatalf("GetShare before: %v", err)
	}
	if !before.AllowAuthSys {
		t.Fatalf("fixture starts with AllowAuthSys=false; the test cannot show the change")
	}

	w := doRequest(t, handler.Patch, http.MethodPatch, `{"allow_auth_sys":false,"min_kerberos_level":"krb5p"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("Patch() status = %d, want 200, body = %s", w.Code, w.Body.String())
	}

	after, err := rt.GetShare("/export")
	if err != nil {
		t.Fatalf("GetShare after: %v", err)
	}
	if after.AllowAuthSys {
		t.Error("running share still has AllowAuthSys=true; the export keeps accepting a flavor it now forbids")
	}
	if after.MinKerberosLevel != models.KerberosLevelKrb5p {
		t.Errorf("running share MinKerberosLevel = %q, want %q", after.MinKerberosLevel, models.KerberosLevelKrb5p)
	}
}

// TestShareNFSConfig_PatchLeavesUnsetExportFieldsAlone guards the partial-update
// shape: a PATCH naming one field must not reset the others to their zero value.
func TestShareNFSConfig_PatchLeavesUnsetExportFieldsAlone(t *testing.T) {
	rt, handler := setupShareNFSConfigTestWithRuntime(t)

	w := doRequest(t, handler.Patch, http.MethodPatch, `{"min_kerberos_level":"krb5i"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("Patch() status = %d, want 200, body = %s", w.Code, w.Body.String())
	}

	after, err := rt.GetShare("/export")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if !after.AllowAuthSys {
		t.Error("AllowAuthSys was reset to false by a PATCH that did not name it")
	}
	if after.MinKerberosLevel != models.KerberosLevelKrb5i {
		t.Errorf("MinKerberosLevel = %q, want krb5i", after.MinKerberosLevel)
	}
}

// TestShareNFSConfig_PatchPushesRequireKerberosToRunningShare covers the field
// the tests above leave out. Every field in NFSExportPolicyUpdate is assigned by
// hand, so a mis-mapped struct member would otherwise go unnoticed.
//
// The share starts with RequireKerberos set on the running share, because the
// handler refuses to turn it ON without Kerberos configured — clearing it is the
// direction this fixture can exercise.
func TestShareNFSConfig_PatchPushesRequireKerberosToRunningShare(t *testing.T) {
	rt, handler := setupShareNFSConfigTestWithRuntime(t)

	if err := rt.SetExportAuthPolicyForTesting("/export", true, true); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}
	before, err := rt.GetShare("/export")
	if err != nil {
		t.Fatalf("GetShare before: %v", err)
	}
	if !before.RequireKerberos {
		t.Fatal("fixture did not start with RequireKerberos=true; the test cannot show the change")
	}

	w := doRequest(t, handler.Patch, http.MethodPatch, `{"require_kerberos":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("Patch() status = %d, want 200, body = %s", w.Code, w.Body.String())
	}

	after, err := rt.GetShare("/export")
	if err != nil {
		t.Fatalf("GetShare after: %v", err)
	}
	if after.RequireKerberos {
		t.Error("running share still has RequireKerberos=true after it was cleared")
	}
}

// TestShareNFSConfig_PatchPushesDisableReaddirplusToRunningShare covers the
// fourth field. READDIRPLUS is downgraded per request by reading this flag off
// the running share, so persisting it alone leaves the downgrade inactive.
func TestShareNFSConfig_PatchPushesDisableReaddirplusToRunningShare(t *testing.T) {
	rt, handler := setupShareNFSConfigTestWithRuntime(t)

	before, err := rt.GetShare("/export")
	if err != nil {
		t.Fatalf("GetShare before: %v", err)
	}
	if before.DisableReaddirplus {
		t.Fatal("fixture already has DisableReaddirplus=true; the test cannot show the change")
	}

	w := doRequest(t, handler.Patch, http.MethodPatch, `{"disable_readdirplus":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("Patch() status = %d, want 200, body = %s", w.Code, w.Body.String())
	}

	after, err := rt.GetShare("/export")
	if err != nil {
		t.Fatalf("GetShare after: %v", err)
	}
	if !after.DisableReaddirplus {
		t.Error("running share still has DisableReaddirplus=false; the READDIRPLUS downgrade stays inactive")
	}
}
