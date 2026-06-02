package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/controlplane/api/auth"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// newTestRouter builds a router + JWT service backed by an in-memory store.
func newTestRouter(t *testing.T, pprofEnabled bool) (http.Handler, *auth.JWTService) {
	t.Helper()

	cpStore, err := store.New(&store.Config{
		Type:   "sqlite",
		SQLite: store.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}

	jwtService, err := auth.NewJWTService(auth.JWTConfig{
		Secret:               "test-secret-key-for-testing-only-32chars",
		Issuer:               "dittofs",
		AccessTokenDuration:  15 * time.Minute,
		RefreshTokenDuration: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("create jwt service: %v", err)
	}

	router := NewRouter(nil, jwtService, cpStore, pprofEnabled, 30*time.Minute)
	return router, jwtService
}

// tokenFor mints an access token for a user with the given role.
func tokenFor(t *testing.T, jwtService *auth.JWTService, role models.UserRole) string {
	t.Helper()
	return tokenForUser(t, jwtService, role, false)
}

// tokenForUser mints an access token, optionally flagging MustChangePassword.
func tokenForUser(t *testing.T, jwtService *auth.JWTService, role models.UserRole, mustChange bool) string {
	t.Helper()
	pair, err := jwtService.GenerateTokenPair(&models.User{
		ID:                 "test-user-id",
		Username:           "tester",
		Role:               string(role),
		MustChangePassword: mustChange,
	})
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	return pair.AccessToken
}

// TestPprofRequiresAdminAuth verifies the /debug/pprof endpoints reject
// unauthenticated and non-admin requests and only serve admins. pprof dumps
// can leak in-memory secrets and are DoS vectors, so they must not be open.
func TestPprofRequiresAdminAuth(t *testing.T) {
	router, jwtService := newTestRouter(t, true)

	cases := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{"no token -> 401", "", http.StatusUnauthorized},
		{"bad token -> 401", "Bearer not-a-real-token", http.StatusUnauthorized},
		{"operator -> 403", "Bearer " + tokenFor(t, jwtService, models.RoleOperator), http.StatusForbidden},
		{"user -> 403", "Bearer " + tokenFor(t, jwtService, models.RoleUser), http.StatusForbidden},
		{"must-change-password admin -> 403", "Bearer " + tokenForUser(t, jwtService, models.RoleAdmin, true), http.StatusForbidden},
		{"admin -> 200", "Bearer " + tokenFor(t, jwtService, models.RoleAdmin), http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/debug/pprof/goroutine?debug=1", nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body=%q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestPprofDisabledNotMounted verifies that with pprof disabled the route is
// absent entirely (404), not merely auth-gated.
func TestPprofDisabledNotMounted(t *testing.T) {
	router, jwtService := newTestRouter(t, false)

	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/goroutine?debug=1", nil)
	req.Header.Set("Authorization", "Bearer "+tokenFor(t, jwtService, models.RoleAdmin))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (pprof should be unmounted when disabled)", rec.Code, http.StatusNotFound)
	}
}
