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

// TestShareHandler_Disable_NotFound verifies Disable returns 404 for shares
// the runtime does not know about (not-yet-loaded or truly missing). The
// DB-only seedShare test fixture leaves the runtime registry empty, so every
// Disable call naturally exercises the not-found path — which is all the
// integration layer can reasonably cover without wiring a full runtime with
// real block/metadata stores.
func TestShareHandler_Disable_NotFound(t *testing.T) {
	cpStore, _, handler := setupShareTestWithRuntime(t)
	seedShare(t, cpStore, "s-dis")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares/s-dis/disable", nil)
	req = withShareName(req, "s-dis")
	w := httptest.NewRecorder()
	handler.Disable(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("Disable runtime-unknown = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestShareHandler_Enable_NotFound mirrors the Disable path.
func TestShareHandler_Enable_NotFound(t *testing.T) {
	cpStore, _, handler := setupShareTestWithRuntime(t)
	seedShare(t, cpStore, "s-en")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares/s-en/enable", nil)
	req = withShareName(req, "s-en")
	w := httptest.NewRecorder()
	handler.Enable(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("Enable runtime-unknown = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestShareHandler_Get_IncludesEnabledField verifies at the integration
// layer that the `enabled` JSON field is always present and mirrors the
// DB row.
func TestShareHandler_Get_IncludesEnabledField(t *testing.T) {
	cpStore, _, handler := setupShareTestWithRuntime(t)
	seedShare(t, cpStore, "s-enabled-json")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/shares/s-enabled-json", nil)
	req = withShareName(req, "s-enabled-json")
	w := httptest.NewRecorder()
	handler.Get(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Get = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// Decode raw so we can assert the key is present.
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["enabled"]; !ok {
		t.Errorf("ShareResponse JSON missing `enabled` key: %v", raw)
	}
}

// TestShareHandler_Update_TrashConfig verifies that a PUT carrying the
// per-share recycle-bin knobs (#190) returns 200 and that the new policy is
// persisted to the DB row. The share is DB-only here (not loaded into the
// runtime), so the live SetShareTrashConfig path logs a benign warning and
// the handler still succeeds — exactly the not-yet-loaded contract the other
// share tests rely on.
func TestShareHandler_Update_TrashConfig(t *testing.T) {
	cpStore, _, handler := setupShareTestWithRuntime(t)
	seedShare(t, cpStore, "s-trash")

	body := []byte(`{"trash_enabled":true,"trash_retention_days":7,"trash_exclude_patterns":["*.tmp"]}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/shares/s-trash", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withShareName(req, "s-trash")
	w := httptest.NewRecorder()

	handler.Update(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Update = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// Assert the policy was persisted to the DB row.
	got, err := cpStore.GetShare(context.Background(), "/s-trash")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if !got.TrashEnabled {
		t.Errorf("TrashEnabled = false, want true")
	}
	if got.TrashRetentionDays != 7 {
		t.Errorf("TrashRetentionDays = %d, want 7", got.TrashRetentionDays)
	}
	patterns := got.GetTrashExcludePatterns()
	if len(patterns) != 1 || patterns[0] != "*.tmp" {
		t.Errorf("TrashExcludePatterns = %v, want [*.tmp]", patterns)
	}
}

// TestShareHandler_Update_TrashRejectsNegative verifies that negative
// retention/limits are rejected with 400 (problem+json) per the handler's
// standard validation style.
func TestShareHandler_Update_TrashRejectsNegative(t *testing.T) {
	cpStore, _, handler := setupShareTestWithRuntime(t)
	seedShare(t, cpStore, "s-trash-neg")

	body := []byte(`{"trash_retention_days":-1}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/shares/s-trash-neg", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withShareName(req, "s-trash-neg")
	w := httptest.NewRecorder()

	handler.Update(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("Update(negative retention) = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestShareHandler_Update_ResolvesBlockStoreNameToID verifies the root-cause
// fix for #1312: PUT /shares with a local/remote block store *name* must
// persist the canonical UUID, not the raw name. Previously the name was
// stored verbatim, so on restart GetBlockStoreByID(name) failed and the share
// never loaded.
func TestShareHandler_Update_ResolvesBlockStoreNameToID(t *testing.T) {
	cpStore, _, handler := setupShareTestWithRuntime(t)
	seedShare(t, cpStore, "s-bsname")
	ctx := context.Background()

	// Create a new local + remote block store referenced by NAME below.
	newLocal := &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "fresh-local", Kind: models.BlockStoreKindLocal, Type: "memory", CreatedAt: time.Now(),
	}
	if _, err := cpStore.CreateBlockStore(ctx, newLocal); err != nil {
		t.Fatalf("CreateBlockStore(local): %v", err)
	}
	newRemote := &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "fresh-remote", Kind: models.BlockStoreKindRemote, Type: "memory", CreatedAt: time.Now(),
	}
	if _, err := cpStore.CreateBlockStore(ctx, newRemote); err != nil {
		t.Fatalf("CreateBlockStore(remote): %v", err)
	}

	body := []byte(`{"local_block_store_id":"fresh-local","remote_block_store_id":"fresh-remote"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/shares/s-bsname", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withShareName(req, "s-bsname")
	w := httptest.NewRecorder()

	handler.Update(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Update = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	got, err := cpStore.GetShare(ctx, "/s-bsname")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if got.LocalBlockStoreID != newLocal.ID {
		t.Errorf("LocalBlockStoreID = %q, want canonical UUID %q (not the name)", got.LocalBlockStoreID, newLocal.ID)
	}
	if got.RemoteBlockStoreID == nil || *got.RemoteBlockStoreID != newRemote.ID {
		t.Errorf("RemoteBlockStoreID = %v, want canonical UUID %q (not the name)", got.RemoteBlockStoreID, newRemote.ID)
	}
}

// TestShareHandler_Update_RejectsUnknownBlockStore verifies an unknown block
// store name/UUID is rejected with 400 rather than silently persisted.
func TestShareHandler_Update_RejectsUnknownBlockStore(t *testing.T) {
	cpStore, _, handler := setupShareTestWithRuntime(t)
	seedShare(t, cpStore, "s-bsunknown")

	body := []byte(`{"local_block_store_id":"does-not-exist"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/shares/s-bsunknown", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withShareName(req, "s-bsunknown")
	w := httptest.NewRecorder()

	handler.Update(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("Update(unknown block store) = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestShareHandler_Update_RejectsWrongKindBlockStore verifies a UUID that
// resolves to the wrong kind (a remote store handed to local_block_store_id) is
// rejected with 400 rather than persisted — otherwise the share would fail to
// load at the next restart (#1312).
func TestShareHandler_Update_RejectsWrongKindBlockStore(t *testing.T) {
	cpStore, _, handler := setupShareTestWithRuntime(t)
	seedShare(t, cpStore, "s-bskind")
	ctx := context.Background()

	remote := &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "kind-remote", Kind: models.BlockStoreKindRemote, Type: "memory", CreatedAt: time.Now(),
	}
	if _, err := cpStore.CreateBlockStore(ctx, remote); err != nil {
		t.Fatalf("CreateBlockStore(remote): %v", err)
	}

	// Hand the remote store's UUID to the local tier — must be refused.
	body := []byte(`{"local_block_store_id":"` + remote.ID + `"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/shares/s-bskind", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withShareName(req, "s-bskind")
	w := httptest.NewRecorder()

	handler.Update(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("Update(wrong-kind block store) = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// seedStores creates a metadata store + local block store (no share) and
// returns their names, so a Create request can reference real stores and reach
// the default_permission validation rather than failing the store-existence
// checks first.
func seedStores(t *testing.T, cpStore store.Store, name string) (metaName, blockName string) {
	t.Helper()
	ctx := context.Background()

	metaStore := &models.MetadataStoreConfig{
		ID: uuid.New().String(), Name: "m-" + name, Type: "memory", CreatedAt: time.Now(),
	}
	if _, err := cpStore.CreateMetadataStore(ctx, metaStore); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	blockStore := &models.BlockStoreConfig{
		ID: uuid.New().String(), Name: "b-" + name, Kind: models.BlockStoreKindLocal, Type: "memory", CreatedAt: time.Now(),
	}
	if _, err := cpStore.CreateBlockStore(ctx, blockStore); err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}
	return metaStore.Name, blockStore.Name
}

// TestShareHandler_Create_RejectsInvalidDefaultPermission verifies an
// unrecognized default_permission is rejected with 400 rather than stored and
// echoed back — ParseSharePermission silently maps unknown strings to
// PermissionNone (total deny), so a typo like "read_write" would look granted
// while denying every unknown-UID client (#1180).
func TestShareHandler_Create_RejectsInvalidDefaultPermission(t *testing.T) {
	cpStore, _, handler := setupShareTestWithRuntime(t)
	metaName, blockName := seedStores(t, cpStore, "s-perm-bad")

	body, _ := json.Marshal(CreateShareRequest{
		Name:              "/perm-bad",
		MetadataStoreID:   metaName,
		LocalBlockStore:   blockName,
		DefaultPermission: "read_write", // underscore — the valid token is "read-write"
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.Create(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("Create(invalid default_permission) = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestShareHandler_Update_RejectsInvalidDefaultPermission is the update-path
// counterpart of the create check (#1180).
func TestShareHandler_Update_RejectsInvalidDefaultPermission(t *testing.T) {
	cpStore, _, handler := setupShareTestWithRuntime(t)
	seedShare(t, cpStore, "s-perm-upd")

	body := []byte(`{"default_permission":"read_write"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/shares/s-perm-upd", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withShareName(req, "s-perm-upd")
	w := httptest.NewRecorder()

	handler.Update(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("Update(invalid default_permission) = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}
