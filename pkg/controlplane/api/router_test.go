package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/controlplane/api/auth"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// newTestRouter builds a router + JWT service backed by an in-memory store,
// returning the store so tests can seed fixtures (e.g. adapters).
func newTestRouter(t *testing.T, pprofEnabled bool) (http.Handler, *auth.JWTService, store.Store) {
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

	router := NewRouter(runtime.New(cpStore), jwtService, cpStore, pprofEnabled, Timeouts{Restore: 30 * time.Minute, DrainStall: 5 * time.Minute})
	return router, jwtService, cpStore
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
	router, jwtService, _ := newTestRouter(t, true)

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

// TestAdapterPortsAuthenticatedNonAdmin verifies the authz split on the
// adapter read routes: the full listing stays admin/operator-only, while the
// lean port-discovery endpoint is reachable by any authenticated user (so a
// plain share-user can find the port to mount) and never exposes adapter
// Config.
func TestAdapterPortsAuthenticatedNonAdmin(t *testing.T) {
	router, jwtService, cpStore := newTestRouter(t, false)

	if _, err := cpStore.CreateAdapter(context.Background(), &models.AdapterConfig{
		ID:        "nfs-adapter",
		Type:      "nfs",
		Enabled:   true,
		Port:      12049,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed adapter: %v", err)
	}

	userToken := "Bearer " + tokenFor(t, jwtService, models.RoleUser)

	// Full listing stays role-gated: a plain user is forbidden.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/adapters", nil)
	req.Header.Set("Authorization", userToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("GET /adapters as user = %d, want %d", rec.Code, http.StatusForbidden)
	}

	// Port discovery is open to any authenticated user and returns the port.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/adapters/ports", nil)
	req.Header.Set("Authorization", userToken)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /adapters/ports as user = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"port":12049`) {
		t.Errorf("ports response missing port: %q", body)
	}
	// Config must never leak through the unprivileged endpoint.
	if body := rec.Body.String(); strings.Contains(body, `"config"`) {
		t.Errorf("ports response leaked config: %q", body)
	}
}

// TestPprofDisabledNotMounted verifies that with pprof disabled the route is
// absent entirely (404), not merely auth-gated.
func TestPprofDisabledNotMounted(t *testing.T) {
	router, jwtService, _ := newTestRouter(t, false)

	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/goroutine?debug=1", nil)
	req.Header.Set("Authorization", "Bearer "+tokenFor(t, jwtService, models.RoleAdmin))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (pprof should be unmounted when disabled)", rec.Code, http.StatusNotFound)
	}
}
