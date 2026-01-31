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

func setupGroupTest(t *testing.T) (store.Store, *GroupHandler) {
	t.Helper()

	// Create in-memory SQLite store
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

	handler := NewGroupHandler(cpStore)
	return cpStore, handler
}

func TestGroupHandler_Create(t *testing.T) {
	cpStore, handler := setupGroupTest(t)
	ctx := context.Background()

	tests := []struct {
		name       string
		body       CreateGroupRequest
		wantStatus int
	}{
		{
			name: "valid group",
			body: CreateGroupRequest{
				Name:        "developers",
				Description: "Development team",
			},
			wantStatus: http.StatusCreated,
		},
		{
			name: "missing name",
			body: CreateGroupRequest{
				Description: "No name",
			},
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.body)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/groups", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			handler.Create(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("Create() status = %d, want %d", w.Code, tt.wantStatus)
			}

			if tt.wantStatus == http.StatusCreated {
				var resp GroupResponse
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("Failed to unmarshal response: %v", err)
				}

				// Verify group was created
				group, err := cpStore.GetGroup(ctx, tt.body.Name)
				if err != nil {
					t.Fatalf("Group not found in store: %v", err)
				}
				if group.Name != tt.body.Name {
					t.Errorf("Group name = %s, want %s", group.Name, tt.body.Name)
				}
			}
		})
	}
}

func TestGroupHandler_List(t *testing.T) {
	cpStore, handler := setupGroupTest(t)
	ctx := context.Background()

	// Create some test groups
	groups := []string{"group1", "group2", "group3"}
	for i, name := range groups {
		gid := uint32(100 + i)
		group := &models.Group{
			ID:        uuid.New().String(),
			Name:      name,
			GID:       &gid,
			CreatedAt: time.Now(),
		}
		if _, err := cpStore.CreateGroup(ctx, group); err != nil {
			t.Fatalf("Failed to create test group: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/groups", nil)
	w := httptest.NewRecorder()

	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("List() status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp []GroupResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if len(resp) != len(groups) {
		t.Errorf("List() returned %d groups, want %d", len(resp), len(groups))
	}
}

func TestGroupHandler_Get(t *testing.T) {
	cpStore, handler := setupGroupTest(t)
	ctx := context.Background()

	// Create a test group
	gid := uint32(100)
	testGroup := &models.Group{
		ID:          uuid.New().String(),
		Name:        "testgroup",
		GID:         &gid,
		Description: "Test group",
		CreatedAt:   time.Now(),
	}
	if _, err := cpStore.CreateGroup(ctx, testGroup); err != nil {
		t.Fatalf("Failed to create test group: %v", err)
	}

	tests := []struct {
		name       string
		groupName  string
		wantStatus int
	}{
		{
			name:       "existing group",
			groupName:  "testgroup",
			wantStatus: http.StatusOK,
		},
		{
			name:       "non-existing group",
			groupName:  "nonexistent",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/groups/"+tt.groupName, nil)

			// Add chi URL params
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("name", tt.groupName)
			req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

			w := httptest.NewRecorder()
			handler.Get(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("Get() status = %d, want %d", w.Code, tt.wantStatus)
			}

			if tt.wantStatus == http.StatusOK {
				var resp GroupResponse
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("Failed to unmarshal response: %v", err)
				}
				if resp.Name != tt.groupName {
					t.Errorf("Get() name = %s, want %s", resp.Name, tt.groupName)
				}
			}
		})
	}
}

func TestGroupHandler_Update(t *testing.T) {
	cpStore, handler := setupGroupTest(t)
	ctx := context.Background()

	// Create a test group
	gid := uint32(100)
	testGroup := &models.Group{
		ID:          uuid.New().String(),
		Name:        "updategroup",
		GID:         &gid,
		Description: "Original description",
		CreatedAt:   time.Now(),
	}
	if _, err := cpStore.CreateGroup(ctx, testGroup); err != nil {
		t.Fatalf("Failed to create test group: %v", err)
	}

	newDesc := "Updated description"
	updateReq := UpdateGroupRequest{
		Description: &newDesc,
	}
	body, _ := json.Marshal(updateReq)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/groups/updategroup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "updategroup")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	handler.Update(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Update() status = %d, want %d", w.Code, http.StatusOK)
	}

	// Verify update
	updated, _ := cpStore.GetGroup(ctx, "updategroup")
	if updated.Description != newDesc {
		t.Errorf("Update() description = %s, want %s", updated.Description, newDesc)
	}
}

func TestGroupHandler_Delete(t *testing.T) {
	cpStore, handler := setupGroupTest(t)
	ctx := context.Background()

	// Create a test group
	gid := uint32(100)
	testGroup := &models.Group{
		ID:        uuid.New().String(),
		Name:      "deletegroup",
		GID:       &gid,
		CreatedAt: time.Now(),
	}
	if _, err := cpStore.CreateGroup(ctx, testGroup); err != nil {
		t.Fatalf("Failed to create test group: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/groups/deletegroup", nil)

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "deletegroup")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	handler.Delete(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("Delete() status = %d, want %d", w.Code, http.StatusNoContent)
	}

	// Verify deletion
	_, err := cpStore.GetGroup(ctx, "deletegroup")
	if err != models.ErrGroupNotFound {
		t.Errorf("Delete() group still exists")
	}
}

func TestGroupHandler_AddRemoveMember(t *testing.T) {
	cpStore, handler := setupGroupTest(t)
	ctx := context.Background()

	// Create a test group
	gid := uint32(100)
	testGroup := &models.Group{
		ID:        uuid.New().String(),
		Name:      "membergroup",
		GID:       &gid,
		CreatedAt: time.Now(),
	}
	if _, err := cpStore.CreateGroup(ctx, testGroup); err != nil {
		t.Fatalf("Failed to create test group: %v", err)
	}

	// Create a test user
	uid := uint32(1000)
	testUser := &models.User{
		ID:           uuid.New().String(),
		Username:     "testuser",
		PasswordHash: "hash",
		UID:          &uid,
		GID:          &uid,
		Enabled:      true,
		Role:         "user",
		CreatedAt:    time.Now(),
	}
	if _, err := cpStore.CreateUser(ctx, testUser); err != nil {
		t.Fatalf("Failed to create test user: %v", err)
	}

	// Test AddMember
	addReq := struct {
		Username string `json:"username"`
	}{Username: "testuser"}
	body, _ := json.Marshal(addReq)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/groups/membergroup/members", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "membergroup")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	handler.AddMember(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("AddMember() status = %d, want %d", w.Code, http.StatusNoContent)
	}

	// Verify membership
	members, err := cpStore.GetGroupMembers(ctx, "membergroup")
	if err != nil {
		t.Fatalf("Failed to get members: %v", err)
	}
	if len(members) != 1 || members[0].Username != "testuser" {
		t.Errorf("AddMember() did not add user correctly")
	}

	// Test RemoveMember
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/groups/membergroup/members/testuser", nil)
	rctx = chi.NewRouteContext()
	rctx.URLParams.Add("name", "membergroup")
	rctx.URLParams.Add("username", "testuser")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	w = httptest.NewRecorder()
	handler.RemoveMember(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("RemoveMember() status = %d, want %d", w.Code, http.StatusNoContent)
	}

	// Verify removal
	members, _ = cpStore.GetGroupMembers(ctx, "membergroup")
	if len(members) != 0 {
		t.Errorf("RemoveMember() did not remove user")
	}
}
