package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/marmos91/dittofs/internal/controlplane/api/auth"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func createTestJWTService(t *testing.T) *auth.JWTService {
	t.Helper()
	cfg := auth.JWTConfig{
		Secret: "test-secret-key-that-is-at-least-32-characters-long",
		Issuer: "test",
	}
	svc, err := auth.NewJWTService(cfg)
	if err != nil {
		t.Fatalf("failed to create JWT service: %v", err)
	}
	return svc
}

func TestGetClaimsFromContext(t *testing.T) {
	t.Run("no claims in context", func(t *testing.T) {
		ctx := context.Background()
		claims := GetClaimsFromContext(ctx)
		if claims != nil {
			t.Error("expected nil claims for empty context")
		}
	})

	t.Run("claims present in context", func(t *testing.T) {
		expectedClaims := &auth.Claims{
			UserID:   "user-123",
			Username: "testuser",
			Role:     "admin",
		}
		ctx := context.WithValue(context.Background(), claimsContextKey, expectedClaims)
		claims := GetClaimsFromContext(ctx)
		if claims == nil {
			t.Fatal("expected claims to be present")
		}
		if claims.UserID != expectedClaims.UserID {
			t.Errorf("expected UserID %s, got %s", expectedClaims.UserID, claims.UserID)
		}
	})

	t.Run("wrong type in context", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), claimsContextKey, "not-claims")
		claims := GetClaimsFromContext(ctx)
		if claims != nil {
			t.Error("expected nil claims for wrong type")
		}
	})
}

func TestExtractBearerToken(t *testing.T) {
	tests := []struct {
		name        string
		authHeader  string
		wantToken   string
		wantSuccess bool
	}{
		{"empty header", "", "", false},
		{"bearer token", "Bearer abc123", "abc123", true},
		{"bearer lowercase", "bearer abc123", "abc123", true},
		{"BEARER uppercase", "BEARER abc123", "abc123", true},
		{"missing token", "Bearer", "", false},
		{"wrong scheme", "Basic abc123", "", false},
		{"no space", "Bearerabc123", "", false},
		{"token with spaces", "Bearer token with spaces", "token with spaces", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			token, ok := extractBearerToken(req)
			if ok != tt.wantSuccess {
				t.Errorf("extractBearerToken() success = %v, want %v", ok, tt.wantSuccess)
			}
			if token != tt.wantToken {
				t.Errorf("extractBearerToken() token = %q, want %q", token, tt.wantToken)
			}
		})
	}
}

func TestJWTAuth(t *testing.T) {
	jwtService := createTestJWTService(t)

	// Generate a valid token
	testUser := &models.User{ID: "user-123", Username: "testuser", Role: "user"}
	tokens, err := jwtService.GenerateTokenPair(testUser)
	if err != nil {
		t.Fatalf("failed to generate tokens: %v", err)
	}

	t.Run("missing authorization header", func(t *testing.T) {
		handler := JWTAuth(jwtService)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("handler should not be called")
		}))

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("expected status %d, got %d", http.StatusUnauthorized, rr.Code)
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		handler := JWTAuth(jwtService)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("handler should not be called")
		}))

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer invalid-token")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("expected status %d, got %d", http.StatusUnauthorized, rr.Code)
		}
	})

	t.Run("valid token", func(t *testing.T) {
		var capturedClaims *auth.Claims
		handler := JWTAuth(jwtService)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedClaims = GetClaimsFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
		}
		if capturedClaims == nil {
			t.Fatal("expected claims to be set in context")
		}
		if capturedClaims.Username != "testuser" {
			t.Errorf("expected username %q, got %q", "testuser", capturedClaims.Username)
		}
	})
}

func TestRequireAdmin(t *testing.T) {
	t.Run("no claims in context", func(t *testing.T) {
		handler := RequireAdmin()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("handler should not be called")
		}))

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("expected status %d, got %d", http.StatusUnauthorized, rr.Code)
		}
	})

	t.Run("non-admin user", func(t *testing.T) {
		claims := &auth.Claims{UserID: "user-123", Username: "testuser", Role: "user"}
		ctx := context.WithValue(context.Background(), claimsContextKey, claims)

		handler := RequireAdmin()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("handler should not be called")
		}))

		req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Errorf("expected status %d, got %d", http.StatusForbidden, rr.Code)
		}
	})

	t.Run("admin user", func(t *testing.T) {
		claims := &auth.Claims{UserID: "admin-123", Username: "admin", Role: "admin"}
		ctx := context.WithValue(context.Background(), claimsContextKey, claims)

		handlerCalled := false
		handler := RequireAdmin()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalled = true
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
		}
		if !handlerCalled {
			t.Error("expected handler to be called")
		}
	})
}

func TestRequirePasswordChange(t *testing.T) {
	t.Run("no claims in context", func(t *testing.T) {
		handler := RequirePasswordChange()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("handler should not be called")
		}))

		req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("expected status %d, got %d", http.StatusUnauthorized, rr.Code)
		}
	})

	t.Run("user must change password - blocked", func(t *testing.T) {
		claims := &auth.Claims{UserID: "user-123", Username: "testuser", Role: "user", MustChangePassword: true}
		ctx := context.WithValue(context.Background(), claimsContextKey, claims)

		handler := RequirePasswordChange("/api/v1/users/me/password")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("handler should not be called")
		}))

		req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil).WithContext(ctx)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Errorf("expected status %d, got %d", http.StatusForbidden, rr.Code)
		}
	})

	t.Run("user must change password - allowed path", func(t *testing.T) {
		claims := &auth.Claims{UserID: "user-123", Username: "testuser", Role: "user", MustChangePassword: true}
		ctx := context.WithValue(context.Background(), claimsContextKey, claims)

		handlerCalled := false
		handler := RequirePasswordChange("/api/v1/users/me/password")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalled = true
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest(http.MethodPost, "/api/v1/users/me/password", nil).WithContext(ctx)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
		}
		if !handlerCalled {
			t.Error("expected handler to be called")
		}
	})

	t.Run("user does not need password change", func(t *testing.T) {
		claims := &auth.Claims{UserID: "user-123", Username: "testuser", Role: "user", MustChangePassword: false}
		ctx := context.WithValue(context.Background(), claimsContextKey, claims)

		handlerCalled := false
		handler := RequirePasswordChange()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalled = true
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil).WithContext(ctx)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
		}
		if !handlerCalled {
			t.Error("expected handler to be called")
		}
	})

	t.Run("path normalization with trailing slash", func(t *testing.T) {
		claims := &auth.Claims{UserID: "user-123", Username: "testuser", Role: "user", MustChangePassword: true}
		ctx := context.WithValue(context.Background(), claimsContextKey, claims)

		handlerCalled := false
		handler := RequirePasswordChange("/api/v1/users/me/password/")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalled = true
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest(http.MethodPost, "/api/v1/users/me/password", nil).WithContext(ctx)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if !handlerCalled {
			t.Error("expected handler to be called with normalized path")
		}
	})
}

func TestOptionalJWTAuth(t *testing.T) {
	jwtService := createTestJWTService(t)

	tokens, err := jwtService.GenerateTokenPair(&models.User{ID: "user-123", Username: "testuser", Role: "user"})
	if err != nil {
		t.Fatalf("failed to generate tokens: %v", err)
	}

	t.Run("no authorization header", func(t *testing.T) {
		var capturedClaims *auth.Claims
		handler := OptionalJWTAuth(jwtService)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedClaims = GetClaimsFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
		}
		if capturedClaims != nil {
			t.Error("expected no claims without auth header")
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		var capturedClaims *auth.Claims
		handler := OptionalJWTAuth(jwtService)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedClaims = GetClaimsFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer invalid-token")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
		}
		if capturedClaims != nil {
			t.Error("expected no claims with invalid token")
		}
	})

	t.Run("valid token", func(t *testing.T) {
		var capturedClaims *auth.Claims
		handler := OptionalJWTAuth(jwtService)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedClaims = GetClaimsFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, rr.Code)
		}
		if capturedClaims == nil {
			t.Fatal("expected claims to be set")
		}
		if capturedClaims.Username != "testuser" {
			t.Errorf("expected username %q, got %q", "testuser", capturedClaims.Username)
		}
	})
}
