package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// doQuota issues an authenticated request against a quota route and returns the
// recorder.
func doQuota(t *testing.T, router http.Handler, token, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	return rec
}

// TestQuotaRoutesBindScope exercises set -> get -> list -> remove for every
// supported scope. The default-user scope has no {id} path segment, so its
// route must still bind {scope}: a route that hard-codes the literal leaves
// chi.URLParam(r, "scope") empty and the handler rejects its own scope.
func TestQuotaRoutesBindScope(t *testing.T) {
	router, jwtService, _ := newTestRouter(t, false)
	token := tokenFor(t, jwtService, models.RoleAdmin)

	cases := []struct {
		scope string
		path  string
		id    string
	}{
		{models.QuotaScopeUser, "/api/v1/shares/tiered/quotas/user/1000", "1000"},
		{models.QuotaScopeGroup, "/api/v1/shares/tiered/quotas/group/2000", "2000"},
		{models.QuotaScopeDefaultUser, "/api/v1/shares/tiered/quotas/default-user", ""},
	}

	for _, tc := range cases {
		t.Run(tc.scope, func(t *testing.T) {
			rec := doQuota(t, router, token, http.MethodPut, tc.path, `{"limit_bytes":"1GiB"}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("PUT %s = %d, want 200 (body=%q)", tc.path, rec.Code, rec.Body.String())
			}
			var got struct {
				Scope      string  `json:"scope"`
				IdentityID *uint32 `json:"identity_id"`
				LimitBytes string  `json:"limit_bytes"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode PUT response: %v", err)
			}
			if got.Scope != tc.scope {
				t.Errorf("scope = %q, want %q", got.Scope, tc.scope)
			}
			if tc.id == "" {
				if got.IdentityID != nil {
					t.Errorf("identity_id = %d, want nil for %s", *got.IdentityID, tc.scope)
				}
			} else if got.IdentityID == nil || fmt.Sprint(*got.IdentityID) != tc.id {
				t.Errorf("identity_id = %v, want %s", got.IdentityID, tc.id)
			}

			if rec := doQuota(t, router, token, http.MethodGet, tc.path, ""); rec.Code != http.StatusOK {
				t.Errorf("GET %s = %d, want 200 (body=%q)", tc.path, rec.Code, rec.Body.String())
			}
		})
	}

	// All three land in the listing.
	rec := doQuota(t, router, token, http.MethodGet, "/api/v1/shares/tiered/quotas", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET quotas = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	for _, tc := range cases {
		if !strings.Contains(rec.Body.String(), `"scope":"`+tc.scope+`"`) {
			t.Errorf("listing missing scope %q: %s", tc.scope, rec.Body.String())
		}
	}

	for _, tc := range cases {
		if rec := doQuota(t, router, token, http.MethodDelete, tc.path, ""); rec.Code != http.StatusNoContent {
			t.Errorf("DELETE %s = %d, want 204 (body=%q)", tc.path, rec.Code, rec.Body.String())
		}
	}
}

// TestQuotaScopedRouteRequiresID verifies that omitting the {id} segment on a
// user/group quota reports the missing id rather than the scope.
func TestQuotaScopedRouteRequiresID(t *testing.T) {
	router, jwtService, _ := newTestRouter(t, false)
	token := tokenFor(t, jwtService, models.RoleAdmin)

	rec := doQuota(t, router, token, http.MethodPut, "/api/v1/shares/tiered/quotas/user", `{"limit_bytes":"1GiB"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT without id = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "identity id is required") {
		t.Errorf("body = %q, want an identity-id complaint", rec.Body.String())
	}
}

// TestQuotaUnknownScopeRejected keeps the scope enum enforced.
func TestQuotaUnknownScopeRejected(t *testing.T) {
	router, jwtService, _ := newTestRouter(t, false)
	token := tokenFor(t, jwtService, models.RoleAdmin)

	rec := doQuota(t, router, token, http.MethodPut, "/api/v1/shares/tiered/quotas/wheel/7", `{"limit_bytes":"1GiB"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT unknown scope = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Invalid scope: wheel") {
		t.Errorf("body = %q, want the offending scope echoed", rec.Body.String())
	}
}
