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

func setupNetgroupTest(t *testing.T) (store.Store, *NetgroupHandler) {
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

	handler := NewNetgroupHandler(cpStore)
	return cpStore, handler
}

func TestCreateNetgroup_OK(t *testing.T) {
	_, handler := setupNetgroupTest(t)

	body, _ := json.Marshal(CreateNetgroupRequest{Name: "test-netgroup"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/netgroups", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("Create() status = %d, want %d, body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp NetgroupResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	if resp.Name != "test-netgroup" {
		t.Errorf("Name = %s, want test-netgroup", resp.Name)
	}
	if resp.ID == "" {
		t.Error("Expected non-empty ID")
	}
}

func TestCreateNetgroup_Duplicate(t *testing.T) {
	cpStore, handler := setupNetgroupTest(t)
	ctx := context.Background()

	// Create first
	cpStore.CreateNetgroup(ctx, &models.Netgroup{Name: "dup-test"})

	// Try to create duplicate via handler
	body, _ := json.Marshal(CreateNetgroupRequest{Name: "dup-test"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/netgroups", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.Create(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("Create(duplicate) status = %d, want %d", w.Code, http.StatusConflict)
	}
}

func TestListNetgroups_Handler(t *testing.T) {
	cpStore, handler := setupNetgroupTest(t)
	ctx := context.Background()

	cpStore.CreateNetgroup(ctx, &models.Netgroup{Name: "ng-a"})
	cpStore.CreateNetgroup(ctx, &models.Netgroup{Name: "ng-b"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/netgroups", nil)
	w := httptest.NewRecorder()

	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("List() status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp []NetgroupResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 2 {
		t.Errorf("List() returned %d netgroups, want 2", len(resp))
	}
}

func TestGetNetgroup_Handler(t *testing.T) {
	cpStore, handler := setupNetgroupTest(t)
	ctx := context.Background()

	cpStore.CreateNetgroup(ctx, &models.Netgroup{Name: "get-test"})
	cpStore.AddNetgroupMember(ctx, "get-test", &models.NetgroupMember{
		Type: "ip", Value: "1.2.3.4",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/netgroups/get-test", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "get-test")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	handler.Get(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Get() status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp NetgroupResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Name != "get-test" {
		t.Errorf("Name = %s, want get-test", resp.Name)
	}
	if len(resp.Members) != 1 {
		t.Errorf("Expected 1 member, got %d", len(resp.Members))
	}
}

func TestDeleteNetgroup_OK(t *testing.T) {
	cpStore, handler := setupNetgroupTest(t)
	ctx := context.Background()

	cpStore.CreateNetgroup(ctx, &models.Netgroup{Name: "del-test"})

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/netgroups/del-test", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "del-test")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	handler.Delete(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("Delete() status = %d, want %d", w.Code, http.StatusNoContent)
	}
}

func TestDeleteNetgroup_InUse(t *testing.T) {
	cpStore, handler := setupNetgroupTest(t)
	ctx := context.Background()

	// Create netgroup
	ng := &models.Netgroup{Name: "in-use-ng"}
	ngID, _ := cpStore.CreateNetgroup(ctx, ng)

	// Create required stores and share that references the netgroup
	metaStore := &models.MetadataStoreConfig{
		ID: uuid.New().String(), Name: "m-store", Type: "memory",
	}
	cpStore.CreateMetadataStore(ctx, metaStore)

	payloadStore := &models.PayloadStoreConfig{
		ID: uuid.New().String(), Name: "p-store", Type: "memory",
	}
	cpStore.CreatePayloadStore(ctx, payloadStore)

	share := &models.Share{
		ID:              uuid.New().String(),
		Name:            "/shared",
		MetadataStoreID: metaStore.ID,
		PayloadStoreID:  payloadStore.ID,
		NetgroupID:      &ngID,
		CreatedAt:       time.Now(),
	}
	cpStore.CreateShare(ctx, share)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/netgroups/in-use-ng", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "in-use-ng")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	handler.Delete(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("Delete(in-use) status = %d, want %d, body = %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestAddMember_IP(t *testing.T) {
	cpStore, handler := setupNetgroupTest(t)
	ctx := context.Background()

	cpStore.CreateNetgroup(ctx, &models.Netgroup{Name: "add-ip-test"})

	body, _ := json.Marshal(AddNetgroupMemberRequest{
		Type:  "ip",
		Value: "192.168.1.50",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/netgroups/add-ip-test/members", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "add-ip-test")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	handler.AddMember(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("AddMember(ip) status = %d, want %d, body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp NetgroupMemberResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Type != "ip" {
		t.Errorf("Type = %s, want ip", resp.Type)
	}
	if resp.Value != "192.168.1.50" {
		t.Errorf("Value = %s, want 192.168.1.50", resp.Value)
	}
}

func TestAddMember_InvalidType(t *testing.T) {
	cpStore, handler := setupNetgroupTest(t)
	ctx := context.Background()

	cpStore.CreateNetgroup(ctx, &models.Netgroup{Name: "bad-type-test"})

	body, _ := json.Marshal(AddNetgroupMemberRequest{
		Type:  "foobar",
		Value: "something",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/netgroups/bad-type-test/members", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "bad-type-test")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	handler.AddMember(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("AddMember(invalid type) status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestAddMember_InvalidCIDR(t *testing.T) {
	cpStore, handler := setupNetgroupTest(t)
	ctx := context.Background()

	cpStore.CreateNetgroup(ctx, &models.Netgroup{Name: "bad-cidr-test"})

	body, _ := json.Marshal(AddNetgroupMemberRequest{
		Type:  "cidr",
		Value: "not-a-cidr",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/netgroups/bad-cidr-test/members", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "bad-cidr-test")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	handler.AddMember(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("AddMember(invalid cidr) status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestRemoveMember_OK(t *testing.T) {
	cpStore, handler := setupNetgroupTest(t)
	ctx := context.Background()

	cpStore.CreateNetgroup(ctx, &models.Netgroup{Name: "rm-member-test"})

	member := &models.NetgroupMember{
		ID: uuid.New().String(), Type: "ip", Value: "10.0.0.1",
	}
	cpStore.AddNetgroupMember(ctx, "rm-member-test", member)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/netgroups/rm-member-test/members/"+member.ID, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "rm-member-test")
	rctx.URLParams.Add("id", member.ID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()

	handler.RemoveMember(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("RemoveMember() status = %d, want %d", w.Code, http.StatusNoContent)
	}
}
