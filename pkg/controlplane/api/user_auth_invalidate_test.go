package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/controlplane/api/auth"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// newInvalidateTestRouter builds a router whose runtime has an auth-cache
// invalidation counter attached at the same seam the NFS adapter subscribes to
// (OnAuthCacheInvalidate -> Handler.ClearAuthCache). The counter stands in for
// the handler because the v3 handler package transitively imports this one.
func newInvalidateTestRouter(t *testing.T) (http.Handler, *auth.JWTService, store.Store, *atomic.Int64) {
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

	rt := runtime.New(cpStore)

	var calls atomic.Int64
	rt.OnAuthCacheInvalidate(func() { calls.Add(1) })

	router := NewRouter(rt, jwtService, cpStore, false, Timeouts{Restore: 30 * time.Minute, DrainStall: 5 * time.Minute})
	return router, jwtService, cpStore, &calls
}

// seedUser creates a plain enabled user directly in the store.
func seedUser(t *testing.T, cpStore store.Store, username string) {
	t.Helper()
	uid := uint32(4242)
	gid := uint32(4242)
	if _, err := cpStore.CreateUserWithGroups(context.Background(), &models.User{
		Username:     username,
		PasswordHash: "x",
		Enabled:      true,
		Role:         string(models.RoleUser),
		UID:          &uid,
		GID:          &gid,
	}, nil); err != nil {
		t.Fatalf("seed user %q: %v", username, err)
	}
}

// TestUserMutationsInvalidateAuthCache verifies that every control-plane user
// mutation the NFS auth resolver can observe fires the auth-cache invalidation
// event. The NFSv3 handler caches a resolved auth context keyed
// Share+AuthFlavor+uid+gid+GIDs; without this event a client holding a cached
// positive context keeps access after the user record is disabled, its UID
// changed, its share grants rewritten, or the record deleted outright.
func TestUserMutationsInvalidateAuthCache(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
	}{
		{
			name:   "create user",
			method: http.MethodPost,
			path:   "/api/v1/users",
			body:   `{"username":"created","password":"s3cret-passphrase","uid":5001}`,
			want:   http.StatusCreated,
		},
		{
			name:   "disable user",
			method: http.MethodPut,
			path:   "/api/v1/users/target",
			body:   `{"enabled":false}`,
			want:   http.StatusOK,
		},
		{
			name:   "change uid",
			method: http.MethodPut,
			path:   "/api/v1/users/target",
			body:   `{"uid":6001}`,
			want:   http.StatusOK,
		},
		{
			name:   "rewrite share grants",
			method: http.MethodPut,
			path:   "/api/v1/users/target",
			body:   `{"share_permissions":{"nope":"rw"}}`,
			want:   http.StatusOK,
		},
		{
			name:   "delete user",
			method: http.MethodDelete,
			path:   "/api/v1/users/target",
			body:   "",
			want:   http.StatusNoContent,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, jwtService, cpStore, calls := newInvalidateTestRouter(t)
			seedUser(t, cpStore, "target")

			var body *strings.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			} else {
				body = strings.NewReader("")
			}
			req := httptest.NewRequest(tc.method, tc.path, body)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+tokenFor(t, jwtService, models.RoleAdmin))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("%s %s = %d, want %d (body=%q)", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
			}

			if got := calls.Load(); got == 0 {
				t.Errorf("%s %s did not fire the auth-cache invalidation event; "+
					"an NFSv3 client holding a cached auth context keeps the old authorization until the TTL expires",
					tc.method, tc.path)
			}
		})
	}
}

// TestFailedUserMutationDoesNotInvalidate verifies the event is tied to an
// actual store change: a mutation that never reached the store must not fire
// it, so the cache is not flushed on every rejected request.
func TestFailedUserMutationDoesNotInvalidate(t *testing.T) {
	router, jwtService, _, calls := newInvalidateTestRouter(t)

	// No such user: Update returns 404 without touching the record.
	req := httptest.NewRequest(http.MethodPut, "/api/v1/users/ghost", strings.NewReader(`{"enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tokenFor(t, jwtService, models.RoleAdmin))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("PUT unknown user = %d, want %d (body=%q)", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("failed mutation fired %d invalidation(s), want 0", got)
	}
}

// TestPasswordChangeDoesNotInvalidateAuthCache pins the deliberate exclusion of
// the password endpoints. The NFS auth resolver reads no credential material —
// AUTH_SYS carries a bare UID — so a password change cannot alter an already
// resolved auth context. Flushing on it would drop the whole cache on a routine
// self-service operation for no authorization benefit.
func TestPasswordChangeDoesNotInvalidateAuthCache(t *testing.T) {
	router, jwtService, cpStore, calls := newInvalidateTestRouter(t)
	seedUser(t, cpStore, "target")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/target/password",
		strings.NewReader(`{"new_password":"another-s3cret-passphrase"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tokenFor(t, jwtService, models.RoleAdmin))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("password reset = %d, want %d (body=%q)", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("password reset fired %d invalidation(s), want 0", got)
	}
}
