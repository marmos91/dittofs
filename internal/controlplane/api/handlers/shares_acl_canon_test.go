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
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// Refs #514 — REST layer surfaces AclFlagInheritedCanonicalization end to end.

// TestShareResponse_IncludesAclFlagInheritedCanonicalization verifies the JSON
// response always carries the field (no omitempty), matching the `enabled`
// pattern: `false` is operator-meaningful (canonicalization disabled).
func TestShareResponse_IncludesAclFlagInheritedCanonicalization(t *testing.T) {
	share := &models.Share{
		ID:                               "s1",
		Name:                             "/alice",
		AclFlagInheritedCanonicalization: false,
	}
	resp := shareToResponse(share)
	if resp.AclFlagInheritedCanonicalization {
		t.Fatalf("ShareResponse.AclFlagInheritedCanonicalization should mirror models.Share (false)")
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"acl_flag_inherited_canonicalization":false`) {
		t.Errorf("JSON missing acl_flag_inherited_canonicalization=false; got %s", b)
	}
}

// TestShareResponse_AclFlagInheritedCanonicalizationTrue exercises the true case.
func TestShareResponse_AclFlagInheritedCanonicalizationTrue(t *testing.T) {
	share := &models.Share{
		ID:                               "s1",
		Name:                             "/alice",
		AclFlagInheritedCanonicalization: true,
	}
	b, err := json.Marshal(shareToResponse(share))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"acl_flag_inherited_canonicalization":true`) {
		t.Errorf("JSON missing acl_flag_inherited_canonicalization=true; got %s", b)
	}
}

// shareACLCanonHandler wires an in-memory store and a runtime-less
// ShareHandler. Runtime-less is fine here because the create / update
// paths only need the store to round-trip the field; the runtime block
// is guarded by `if h.runtime != nil`.
func shareACLCanonHandler(t *testing.T) (store.Store, *ShareHandler) {
	t.Helper()
	cfg := store.Config{
		Type:   "sqlite",
		SQLite: store.SQLiteConfig{Path: ":memory:"},
	}
	cpStore, err := store.New(&cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return cpStore, NewShareHandler(cpStore, nil)
}

// seedMetaAndBlock creates the metadata + local block store rows required
// for a share Create request to succeed. Returns their names.
func seedMetaAndBlock(t *testing.T, cpStore store.Store) (string, string) {
	t.Helper()
	ctx := context.Background()

	meta := &models.MetadataStoreConfig{
		ID:        uuid.New().String(),
		Name:      "meta-aclcanon",
		Type:      "memory",
		CreatedAt: time.Now(),
	}
	if _, err := cpStore.CreateMetadataStore(ctx, meta); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}

	bs := &models.BlockStoreConfig{
		ID:        uuid.New().String(),
		Name:      "bs-aclcanon",
		Kind:      models.BlockStoreKindLocal,
		Type:      "memory",
		CreatedAt: time.Now(),
	}
	if _, err := cpStore.CreateBlockStore(ctx, bs); err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}
	return meta.Name, bs.Name
}

func decodeShareResp(t *testing.T, w *httptest.ResponseRecorder) ShareResponse {
	t.Helper()
	var resp ShareResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode ShareResponse: %v; body=%s", err, w.Body.String())
	}
	return resp
}

// TestShareHandler_Create_AclFlagInheritedCanonicalization_DefaultTrue —
// unset in request → DB row persists with true.
func TestShareHandler_Create_AclFlagInheritedCanonicalization_DefaultTrue(t *testing.T) {
	cpStore, handler := shareACLCanonHandler(t)
	metaName, bsName := seedMetaAndBlock(t, cpStore)

	body, _ := json.Marshal(CreateShareRequest{
		Name:            "/acldef",
		MetadataStoreID: metaName,
		LocalBlockStore: bsName,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.Create(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("Create = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	resp := decodeShareResp(t, w)
	if !resp.AclFlagInheritedCanonicalization {
		t.Errorf("Create default AclFlagInheritedCanonicalization = false, want true")
	}

	// DB round-trip.
	got, err := cpStore.GetShare(context.Background(), "/acldef")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if !got.AclFlagInheritedCanonicalization {
		t.Errorf("DB row AclFlagInheritedCanonicalization = false, want true (default)")
	}
}

// TestShareHandler_Create_AclFlagInheritedCanonicalization_ExplicitFalse —
// `false` in request → DB row persists with false.
//
// Note: store-layer guarantee comes from CreateShare's transactional
// override of GORM's default-coercion of zero-value bools (see
// pkg/controlplane/store/shares.go). No follow-up UpdateShare workaround
// is needed in the handler.
func TestShareHandler_Create_AclFlagInheritedCanonicalization_ExplicitFalse(t *testing.T) {
	cpStore, handler := shareACLCanonHandler(t)
	metaName, bsName := seedMetaAndBlock(t, cpStore)

	falseV := false
	body, _ := json.Marshal(CreateShareRequest{
		Name:                             "/aclfalse",
		MetadataStoreID:                  metaName,
		LocalBlockStore:                  bsName,
		AclFlagInheritedCanonicalization: &falseV,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.Create(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("Create = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	resp := decodeShareResp(t, w)
	if resp.AclFlagInheritedCanonicalization {
		t.Errorf("Create explicit-false AclFlagInheritedCanonicalization = true, want false")
	}

	got, err := cpStore.GetShare(context.Background(), "/aclfalse")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if got.AclFlagInheritedCanonicalization {
		t.Errorf("DB row AclFlagInheritedCanonicalization = true, want false")
	}
}

// withShareNameACL is a local copy of the chi route-context helper used by
// the integration tests; pulled in here so this file builds without the
// integration tag.
func withShareNameACL(r *http.Request, name string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", name)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// TestShareHandler_Update_AclFlagInheritedCanonicalization_TogglesFalse —
// PUT with explicit false flips a previously-true row.
func TestShareHandler_Update_AclFlagInheritedCanonicalization_TogglesFalse(t *testing.T) {
	cpStore, handler := shareACLCanonHandler(t)
	metaName, bsName := seedMetaAndBlock(t, cpStore)

	// Seed via Create so the share starts with AclFlagInheritedCanonicalization=true.
	createBody, _ := json.Marshal(CreateShareRequest{
		Name:            "/aclupdate",
		MetadataStoreID: metaName,
		LocalBlockStore: bsName,
	})
	cReq := httptest.NewRequest(http.MethodPost, "/api/v1/shares", bytes.NewReader(createBody))
	cReq.Header.Set("Content-Type", "application/json")
	cW := httptest.NewRecorder()
	handler.Create(cW, cReq)
	if cW.Code != http.StatusCreated {
		t.Fatalf("seed Create = %d, want 201; body=%s", cW.Code, cW.Body.String())
	}

	falseV := false
	updBody, _ := json.Marshal(UpdateShareRequest{
		AclFlagInheritedCanonicalization: &falseV,
	})
	uReq := httptest.NewRequest(http.MethodPut, "/api/v1/shares/aclupdate", bytes.NewReader(updBody))
	uReq.Header.Set("Content-Type", "application/json")
	uReq = withShareNameACL(uReq, "aclupdate")
	uW := httptest.NewRecorder()
	handler.Update(uW, uReq)
	if uW.Code != http.StatusOK {
		t.Fatalf("Update = %d, want 200; body=%s", uW.Code, uW.Body.String())
	}
	resp := decodeShareResp(t, uW)
	if resp.AclFlagInheritedCanonicalization {
		t.Errorf("Update response AclFlagInheritedCanonicalization = true, want false")
	}

	got, err := cpStore.GetShare(context.Background(), "/aclupdate")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if got.AclFlagInheritedCanonicalization {
		t.Errorf("DB row after Update = true, want false")
	}
}

// TestShareHandler_Update_AclFlagInheritedCanonicalization_NilLeavesUnchanged —
// PUT without the field leaves the existing value in place.
func TestShareHandler_Update_AclFlagInheritedCanonicalization_NilLeavesUnchanged(t *testing.T) {
	cpStore, handler := shareACLCanonHandler(t)
	metaName, bsName := seedMetaAndBlock(t, cpStore)

	falseV := false
	createBody, _ := json.Marshal(CreateShareRequest{
		Name:                             "/aclnochange",
		MetadataStoreID:                  metaName,
		LocalBlockStore:                  bsName,
		AclFlagInheritedCanonicalization: &falseV,
	})
	cReq := httptest.NewRequest(http.MethodPost, "/api/v1/shares", bytes.NewReader(createBody))
	cReq.Header.Set("Content-Type", "application/json")
	cW := httptest.NewRecorder()
	handler.Create(cW, cReq)
	if cW.Code != http.StatusCreated {
		t.Fatalf("seed Create = %d, want 201; body=%s", cW.Code, cW.Body.String())
	}

	// PUT with an unrelated field; AclFlagInheritedCanonicalization stays nil.
	readOnly := true
	updBody, _ := json.Marshal(UpdateShareRequest{ReadOnly: &readOnly})
	uReq := httptest.NewRequest(http.MethodPut, "/api/v1/shares/aclnochange", bytes.NewReader(updBody))
	uReq.Header.Set("Content-Type", "application/json")
	uReq = withShareNameACL(uReq, "aclnochange")
	uW := httptest.NewRecorder()
	handler.Update(uW, uReq)
	if uW.Code != http.StatusOK {
		t.Fatalf("Update = %d, want 200; body=%s", uW.Code, uW.Body.String())
	}
	resp := decodeShareResp(t, uW)
	if resp.AclFlagInheritedCanonicalization {
		t.Errorf("Update with nil AclFlag... unexpectedly flipped to true (was false)")
	}
}
