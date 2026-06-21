//go:build integration

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

func setupSIDMappingTest(t *testing.T) (*store.GORMStore, *SIDMappingHandler) {
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

	handler := NewSIDMappingHandler(cpStore)
	return cpStore, handler
}

func TestListSIDMappings_Handler(t *testing.T) {
	cpStore, handler := setupSIDMappingTest(t)
	ctx := context.Background()

	if _, err := cpStore.AllocateSIDMapping(ctx, "S-1-5-21-1-2-3-1107", 70001, false, "alice"); err != nil {
		t.Fatalf("AllocateSIDMapping: %v", err)
	}
	if _, err := cpStore.AllocateSIDMapping(ctx, "S-1-5-21-1-2-3-1108", 80001, true, "engineers"); err != nil {
		t.Fatalf("AllocateSIDMapping: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sid-mappings", nil)
	w := httptest.NewRecorder()

	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("List() status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp []SIDMappingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	if len(resp) != 2 {
		t.Fatalf("List() returned %d mappings, want 2", len(resp))
	}

	byID := map[string]SIDMappingResponse{}
	for _, m := range resp {
		byID[m.SID] = m
	}
	if u := byID["S-1-5-21-1-2-3-1107"]; u.UnixID != 70001 || u.IsGroup || u.DisplayName != "alice" {
		t.Errorf("user mapping = %+v, want unix=70001 group=false name=alice", u)
	}
	if g := byID["S-1-5-21-1-2-3-1108"]; g.UnixID != 80001 || !g.IsGroup || g.DisplayName != "engineers" {
		t.Errorf("group mapping = %+v, want unix=80001 group=true name=engineers", g)
	}
}

func TestListSIDMappings_Empty(t *testing.T) {
	_, handler := setupSIDMappingTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sid-mappings", nil)
	w := httptest.NewRecorder()

	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("List() status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp []SIDMappingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	if len(resp) != 0 {
		t.Errorf("List() returned %d mappings, want 0", len(resp))
	}
}

func deleteSIDRequest(t *testing.T, handler *SIDMappingHandler, sid string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sid-mappings/"+sid, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("sid", sid)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	handler.Delete(w, req)
	return w
}

func TestDeleteSIDMapping_OK(t *testing.T) {
	cpStore, handler := setupSIDMappingTest(t)
	ctx := context.Background()

	const sid = "S-1-5-21-1-2-3-1107"
	if _, err := cpStore.AllocateSIDMapping(ctx, sid, 70001, false, "alice"); err != nil {
		t.Fatalf("AllocateSIDMapping: %v", err)
	}

	w := deleteSIDRequest(t, handler, sid)
	if w.Code != http.StatusNoContent {
		t.Errorf("Delete() status = %d, want %d, body = %s", w.Code, http.StatusNoContent, w.Body.String())
	}

	if _, err := cpStore.GetSIDMapping(ctx, sid); err == nil {
		t.Errorf("expected SID mapping to be deleted, but it still exists")
	}
}

func TestDeleteSIDMapping_NotFound(t *testing.T) {
	_, handler := setupSIDMappingTest(t)

	w := deleteSIDRequest(t, handler, "S-1-5-21-9-9-9-9999")
	if w.Code != http.StatusNotFound {
		t.Errorf("Delete(missing) status = %d, want %d, body = %s", w.Code, http.StatusNotFound, w.Body.String())
	}
}
