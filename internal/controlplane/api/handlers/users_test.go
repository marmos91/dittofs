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
	"github.com/marmos91/dittofs/internal/controlplane/api/auth"
	"github.com/marmos91/dittofs/internal/controlplane/api/middleware"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

func setupUserTest(t *testing.T) (store.Store, *auth.JWTService, *UserHandler) {
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

	jwtConfig := auth.JWTConfig{
		Secret: "test-secret-key-that-is-at-least-32-characters-long",
		Issuer: "test",
	}
	jwtService, err := auth.NewJWTService(jwtConfig)
	if err != nil {
		t.Fatalf("Failed to create JWT service: %v", err)
	}

	handler, err := NewUserHandler(cpStore, jwtService)
	if err != nil {
		t.Fatalf("Failed to create user handler: %v", err)
	}
	return cpStore, jwtService, handler
}

func TestUserHandler_Create(t *testing.T) {
	_, _, handler := setupUserTest(t)

	tests := []struct {
		name       string
		body       CreateUserRequest
		wantStatus int
	}{
		{
			name: "valid user",
			body: CreateUserRequest{
				Username: "newuser",
				Password: "password123",
			},
			wantStatus: http.StatusCreated,
		},
		{
			name: "with optional fields",
			body: CreateUserRequest{
				Username:    "fulluser",
				Password:    "password123",
				Email:       "test@example.com",
				DisplayName: "Test User",
				Role:        "admin",
			},
			wantStatus: http.StatusCreated,
		},
		{
			name: "missing username",
			body: CreateUserRequest{
				Password: "password123",
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "missing password",
			body: CreateUserRequest{
				Username: "nopassuser",
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "invalid role",
			body: CreateUserRequest{
				Username: "invalidrole",
				Password: "password123",
				Role:     "superadmin",
			},
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.body)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/users", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			handler.Create(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("Create() status = %d, want %d, body = %s", w.Code, tt.wantStatus, w.Body.String())
			}

			if tt.wantStatus == http.StatusCreated {
				var resp UserResponse
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("Failed to unmarshal response: %v", err)
				}
				if resp.Username != tt.body.Username {
					t.Errorf("Create() username = %s, want %s", resp.Username, tt.body.Username)
				}
				// New users must change password
				if !resp.MustChangePassword {
					t.Error("Expected must_change_password to be true for new user")
				}
			}
		})
	}
}

func TestUserHandler_Create_Duplicate(t *testing.T) {
	cpStore, _, handler := setupUserTest(t)
	ctx := context.Background()

	// Create a user first
	passwordHash, ntHash, _ := models.HashPasswordWithNT("password123")
	user := &models.User{
		ID:           uuid.New().String(),
		Username:     "existinguser",
		PasswordHash: passwordHash,
		NTHash:       ntHash,
		Enabled:      true,
		Role:         "user",
		CreatedAt:    time.Now(),
	}
	if _, err := cpStore.CreateUser(ctx, user); err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}

	// Try to create duplicate
	body, _ := json.Marshal(CreateUserRequest{
		Username: "existinguser",
		Password: "password123",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.Create(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("Create() status = %d, want %d", w.Code, http.StatusConflict)
	}
}

func TestUserHandler_List(t *testing.T) {
	cpStore, _, handler := setupUserTest(t)
	ctx := context.Background()

	// Create test users
	for i := 0; i < 3; i++ {
		passwordHash, ntHash, _ := models.HashPasswordWithNT("password")
		user := &models.User{
			ID:           uuid.New().String(),
			Username:     "listuser" + string(rune('a'+i)),
			PasswordHash: passwordHash,
			NTHash:       ntHash,
			Enabled:      true,
			Role:         "user",
			CreatedAt:    time.Now(),
		}
		if _, err := cpStore.CreateUser(ctx, user); err != nil {
			t.Fatalf("Failed to create user: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	w := httptest.NewRecorder()

	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("List() status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp []UserResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if len(resp) != 3 {
		t.Errorf("List() returned %d users, want 3", len(resp))
	}
}

func TestUserHandler_Get(t *testing.T) {
	cpStore, _, handler := setupUserTest(t)
	ctx := context.Background()

	// Create a test user
	passwordHash, ntHash, _ := models.HashPasswordWithNT("password")
	user := &models.User{
		ID:           uuid.New().String(),
		Username:     "getuser",
		PasswordHash: passwordHash,
		NTHash:       ntHash,
		Enabled:      true,
		Role:         "user",
		DisplayName:  "Get User",
		Email:        "get@example.com",
		CreatedAt:    time.Now(),
	}
	if _, err := cpStore.CreateUser(ctx, user); err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}

	tests := []struct {
		name       string
		username   string
		wantStatus int
	}{
		{
			name:       "existing user",
			username:   "getuser",
			wantStatus: http.StatusOK,
		},
		{
			name:       "non-existent user",
			username:   "nonexistent",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/users/"+tt.username, nil)

			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("username", tt.username)
			req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

			w := httptest.NewRecorder()
			handler.Get(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("Get() status = %d, want %d", w.Code, tt.wantStatus)
			}

			if tt.wantStatus == http.StatusOK {
				var resp UserResponse
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("Failed to unmarshal response: %v", err)
				}
				if resp.Username != tt.username {
					t.Errorf("Get() username = %s, want %s", resp.Username, tt.username)
				}
				if resp.DisplayName != "Get User" {
					t.Errorf("Get() display_name = %s, want 'Get User'", resp.DisplayName)
				}
			}
		})
	}
}

func TestUserHandler_Update(t *testing.T) {
	cpStore, _, handler := setupUserTest(t)
	ctx := context.Background()

	// Create a test user
	passwordHash, ntHash, _ := models.HashPasswordWithNT("password")
	user := &models.User{
		ID:           uuid.New().String(),
		Username:     "updateuser",
		PasswordHash: passwordHash,
		NTHash:       ntHash,
		Enabled:      true,
		Role:         "user",
		CreatedAt:    time.Now(),
	}
	if _, err := cpStore.CreateUser(ctx, user); err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}

	newEmail := "updated@example.com"
	newDisplayName := "Updated User"
	body, _ := json.Marshal(UpdateUserRequest{
		Email:       &newEmail,
		DisplayName: &newDisplayName,
	})

	req := httptest.NewRequest(http.MethodPut, "/api/v1/users/updateuser", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("username", "updateuser")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	handler.Update(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Update() status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp UserResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if resp.Email != newEmail {
		t.Errorf("Update() email = %s, want %s", resp.Email, newEmail)
	}
	if resp.DisplayName != newDisplayName {
		t.Errorf("Update() display_name = %s, want %s", resp.DisplayName, newDisplayName)
	}
}

func TestUserHandler_Update_Role(t *testing.T) {
	cpStore, _, handler := setupUserTest(t)
	ctx := context.Background()

	// Create a regular user
	passwordHash, ntHash, _ := models.HashPasswordWithNT("password")
	user := &models.User{
		ID:           uuid.New().String(),
		Username:     "promoteuser",
		PasswordHash: passwordHash,
		NTHash:       ntHash,
		Enabled:      true,
		Role:         "user",
		CreatedAt:    time.Now(),
	}
	if _, err := cpStore.CreateUser(ctx, user); err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}

	// Promote to admin
	newRole := "admin"
	body, _ := json.Marshal(UpdateUserRequest{
		Role: &newRole,
	})

	req := httptest.NewRequest(http.MethodPut, "/api/v1/users/promoteuser", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("username", "promoteuser")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	handler.Update(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Update() status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp UserResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if resp.Role != "admin" {
		t.Errorf("Update() role = %s, want admin", resp.Role)
	}
}

func TestUserHandler_Delete(t *testing.T) {
	cpStore, _, handler := setupUserTest(t)
	ctx := context.Background()

	// Create a test user
	passwordHash, ntHash, _ := models.HashPasswordWithNT("password")
	user := &models.User{
		ID:           uuid.New().String(),
		Username:     "deleteuser",
		PasswordHash: passwordHash,
		NTHash:       ntHash,
		Enabled:      true,
		Role:         "user",
		CreatedAt:    time.Now(),
	}
	if _, err := cpStore.CreateUser(ctx, user); err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/users/deleteuser", nil)

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("username", "deleteuser")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	handler.Delete(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("Delete() status = %d, want %d", w.Code, http.StatusNoContent)
	}

	// Verify user is deleted
	_, err := cpStore.GetUser(ctx, "deleteuser")
	if err != models.ErrUserNotFound {
		t.Errorf("Expected user to be deleted, got err: %v", err)
	}
}

func TestUserHandler_Delete_Admin(t *testing.T) {
	cpStore, _, handler := setupUserTest(t)
	ctx := context.Background()

	// Create admin user
	passwordHash, ntHash, _ := models.HashPasswordWithNT("password")
	user := &models.User{
		ID:           uuid.New().String(),
		Username:     models.AdminUsername,
		PasswordHash: passwordHash,
		NTHash:       ntHash,
		Enabled:      true,
		Role:         "admin",
		CreatedAt:    time.Now(),
	}
	if _, err := cpStore.CreateUser(ctx, user); err != nil {
		t.Fatalf("Failed to create admin user: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/users/admin", nil)

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("username", models.AdminUsername)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	handler.Delete(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("Delete() admin status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestUserHandler_ResetPassword(t *testing.T) {
	cpStore, _, handler := setupUserTest(t)
	ctx := context.Background()

	// Create a test user
	passwordHash, ntHash, _ := models.HashPasswordWithNT("oldpassword")
	user := &models.User{
		ID:                 uuid.New().String(),
		Username:           "resetuser",
		PasswordHash:       passwordHash,
		NTHash:             ntHash,
		Enabled:            true,
		Role:               "user",
		MustChangePassword: false,
		CreatedAt:          time.Now(),
	}
	if _, err := cpStore.CreateUser(ctx, user); err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}

	body, _ := json.Marshal(ChangePasswordRequest{
		NewPassword: "newpassword123",
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/resetuser/password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("username", "resetuser")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	handler.ResetPassword(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("ResetPassword() status = %d, want %d, body = %s", w.Code, http.StatusNoContent, w.Body.String())
	}

	// Verify password was changed and must_change_password is set
	updated, _ := cpStore.GetUser(ctx, "resetuser")
	if !updated.MustChangePassword {
		t.Error("Expected must_change_password to be true after reset")
	}

	// Verify new password works
	_, err := cpStore.ValidateCredentials(ctx, "resetuser", "newpassword123")
	if err != nil {
		t.Errorf("New password should work, got: %v", err)
	}
}

func TestUserHandler_ChangeOwnPassword(t *testing.T) {
	cpStore, jwtService, handler := setupUserTest(t)
	ctx := context.Background()

	// Create a test user
	passwordHash, ntHash, _ := models.HashPasswordWithNT("currentpassword")
	user := &models.User{
		ID:                 uuid.New().String(),
		Username:           "changepassuser",
		PasswordHash:       passwordHash,
		NTHash:             ntHash,
		Enabled:            true,
		Role:               "user",
		MustChangePassword: false,
		CreatedAt:          time.Now(),
	}
	if _, err := cpStore.CreateUser(ctx, user); err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}

	// Generate tokens
	tokenPair, err := jwtService.GenerateTokenPair(user)
	if err != nil {
		t.Fatalf("Failed to generate tokens: %v", err)
	}

	t.Run("with current password", func(t *testing.T) {
		body, _ := json.Marshal(ChangePasswordRequest{
			CurrentPassword: "currentpassword",
			NewPassword:     "newpassword123",
		})

		req := httptest.NewRequest(http.MethodPost, "/api/v1/users/me/password", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tokenPair.AccessToken)

		jwtMiddleware := middleware.JWTAuth(jwtService)
		w := httptest.NewRecorder()

		jwtMiddleware(http.HandlerFunc(handler.ChangeOwnPassword)).ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("ChangeOwnPassword() status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
		}

		// Verify new tokens are returned
		var resp LoginResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}
		if resp.AccessToken == "" {
			t.Error("Expected new access token")
		}
	})

	t.Run("wrong current password", func(t *testing.T) {
		body, _ := json.Marshal(ChangePasswordRequest{
			CurrentPassword: "wrongpassword",
			NewPassword:     "newpassword456",
		})

		req := httptest.NewRequest(http.MethodPost, "/api/v1/users/me/password", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tokenPair.AccessToken)

		jwtMiddleware := middleware.JWTAuth(jwtService)
		w := httptest.NewRecorder()

		jwtMiddleware(http.HandlerFunc(handler.ChangeOwnPassword)).ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("ChangeOwnPassword() status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
	})
}

func TestUserHandler_ChangeOwnPassword_MustChange(t *testing.T) {
	cpStore, jwtService, handler := setupUserTest(t)
	ctx := context.Background()

	// Create a user who must change password
	passwordHash, ntHash, _ := models.HashPasswordWithNT("temppassword")
	user := &models.User{
		ID:                 uuid.New().String(),
		Username:           "mustchangeuser",
		PasswordHash:       passwordHash,
		NTHash:             ntHash,
		Enabled:            true,
		Role:               "user",
		MustChangePassword: true,
		CreatedAt:          time.Now(),
	}
	if _, err := cpStore.CreateUser(ctx, user); err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}

	// Generate tokens
	tokenPair, err := jwtService.GenerateTokenPair(user)
	if err != nil {
		t.Fatalf("Failed to generate tokens: %v", err)
	}

	// User who must change password doesn't need to provide current password
	body, _ := json.Marshal(ChangePasswordRequest{
		NewPassword: "newpassword123",
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/me/password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tokenPair.AccessToken)

	jwtMiddleware := middleware.JWTAuth(jwtService)
	w := httptest.NewRecorder()

	jwtMiddleware(http.HandlerFunc(handler.ChangeOwnPassword)).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("ChangeOwnPassword() status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify must_change_password is now false
	updated, _ := cpStore.GetUser(ctx, "mustchangeuser")
	if updated.MustChangePassword {
		t.Error("Expected must_change_password to be false after changing password")
	}
}
