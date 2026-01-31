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

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/controlplane/api/auth"
	"github.com/marmos91/dittofs/internal/controlplane/api/middleware"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

func setupAuthTest(t *testing.T) (store.Store, *auth.JWTService, *AuthHandler) {
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

	// Create JWT service
	jwtConfig := auth.JWTConfig{
		Secret: "test-secret-key-that-is-at-least-32-characters-long",
		Issuer: "test",
	}
	jwtService, err := auth.NewJWTService(jwtConfig)
	if err != nil {
		t.Fatalf("Failed to create JWT service: %v", err)
	}

	handler := NewAuthHandler(cpStore, jwtService)
	return cpStore, jwtService, handler
}

func createTestUser(t *testing.T, cpStore store.Store, username, password string, enabled bool) *models.User {
	t.Helper()
	ctx := context.Background()

	passwordHash, ntHash, err := models.HashPasswordWithNT(password)
	if err != nil {
		t.Fatalf("Failed to hash password: %v", err)
	}

	uid := uint32(1000)
	user := &models.User{
		ID:           uuid.New().String(),
		Username:     username,
		PasswordHash: passwordHash,
		NTHash:       ntHash,
		Enabled:      true, // Create with true first (GORM default handling)
		Role:         "user",
		CreatedAt:    time.Now(),
	}

	if uid > 0 {
		user.UID = &uid
		user.GID = &uid
	}

	if _, err := cpStore.CreateUser(ctx, user); err != nil {
		t.Fatalf("Failed to create test user: %v", err)
	}

	// If disabled, update the user after creation (GORM zero-value workaround)
	if !enabled {
		user.Enabled = false
		if err := cpStore.UpdateUser(ctx, user); err != nil {
			t.Fatalf("Failed to disable user: %v", err)
		}
	}

	return user
}

func TestAuthHandler_Login(t *testing.T) {
	cpStore, _, handler := setupAuthTest(t)

	// Create a test user
	createTestUser(t, cpStore, "testuser", "password123", true)

	tests := []struct {
		name       string
		body       LoginRequest
		wantStatus int
	}{
		{
			name:       "valid credentials",
			body:       LoginRequest{Username: "testuser", Password: "password123"},
			wantStatus: http.StatusOK,
		},
		{
			name:       "invalid password",
			body:       LoginRequest{Username: "testuser", Password: "wrongpassword"},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "non-existent user",
			body:       LoginRequest{Username: "nonexistent", Password: "password123"},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing username",
			body:       LoginRequest{Password: "password123"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing password",
			body:       LoginRequest{Username: "testuser"},
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.body)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			handler.Login(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("Login() status = %d, want %d, body = %s", w.Code, tt.wantStatus, w.Body.String())
			}

			if tt.wantStatus == http.StatusOK {
				var resp LoginResponse
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("Failed to unmarshal response: %v", err)
				}
				if resp.AccessToken == "" {
					t.Error("Expected access token to be set")
				}
				if resp.RefreshToken == "" {
					t.Error("Expected refresh token to be set")
				}
				if resp.User.Username != tt.body.Username {
					t.Errorf("Expected username %s, got %s", tt.body.Username, resp.User.Username)
				}
			}
		})
	}
}

func TestAuthHandler_Login_DisabledUser(t *testing.T) {
	cpStore, _, handler := setupAuthTest(t)

	// Create a disabled user
	createTestUser(t, cpStore, "disableduser", "password123", false)

	body, _ := json.Marshal(LoginRequest{Username: "disableduser", Password: "password123"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.Login(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("Login() status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestAuthHandler_Refresh(t *testing.T) {
	cpStore, jwtService, handler := setupAuthTest(t)

	// Create a test user
	user := createTestUser(t, cpStore, "testuser", "password123", true)

	// Generate initial tokens
	tokenPair, err := jwtService.GenerateTokenPair(user)
	if err != nil {
		t.Fatalf("Failed to generate token pair: %v", err)
	}

	tests := []struct {
		name         string
		refreshToken string
		wantStatus   int
	}{
		{
			name:         "valid refresh token",
			refreshToken: tokenPair.RefreshToken,
			wantStatus:   http.StatusOK,
		},
		{
			name:         "invalid refresh token",
			refreshToken: "invalid-token",
			wantStatus:   http.StatusUnauthorized,
		},
		{
			name:         "empty refresh token",
			refreshToken: "",
			wantStatus:   http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(RefreshRequest{RefreshToken: tt.refreshToken})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			handler.Refresh(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("Refresh() status = %d, want %d, body = %s", w.Code, tt.wantStatus, w.Body.String())
			}

			if tt.wantStatus == http.StatusOK {
				var resp LoginResponse
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("Failed to unmarshal response: %v", err)
				}
				if resp.AccessToken == "" {
					t.Error("Expected new access token")
				}
			}
		})
	}
}

func TestAuthHandler_Refresh_DisabledUser(t *testing.T) {
	cpStore, jwtService, handler := setupAuthTest(t)
	ctx := context.Background()

	// Create and then disable a user
	user := createTestUser(t, cpStore, "testuser", "password123", true)

	// Generate tokens while user is enabled
	tokenPair, err := jwtService.GenerateTokenPair(user)
	if err != nil {
		t.Fatalf("Failed to generate token pair: %v", err)
	}

	// Disable the user
	user.Enabled = false
	if err := cpStore.UpdateUser(ctx, user); err != nil {
		t.Fatalf("Failed to disable user: %v", err)
	}

	// Try to refresh
	body, _ := json.Marshal(RefreshRequest{RefreshToken: tokenPair.RefreshToken})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.Refresh(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("Refresh() status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestAuthHandler_Me(t *testing.T) {
	cpStore, jwtService, handler := setupAuthTest(t)

	// Create a test user
	user := createTestUser(t, cpStore, "testuser", "password123", true)

	// Generate tokens
	tokenPair, err := jwtService.GenerateTokenPair(user)
	if err != nil {
		t.Fatalf("Failed to generate token pair: %v", err)
	}

	t.Run("authenticated user", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
		req.Header.Set("Authorization", "Bearer "+tokenPair.AccessToken)

		// Use middleware to inject claims into context
		jwtMiddleware := middleware.JWTAuth(jwtService)
		w := httptest.NewRecorder()

		jwtMiddleware(http.HandlerFunc(handler.Me)).ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Me() status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
		}

		var resp UserResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}
		if resp.Username != "testuser" {
			t.Errorf("Me() username = %s, want testuser", resp.Username)
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
		w := httptest.NewRecorder()

		handler.Me(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("Me() status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
	})
}
