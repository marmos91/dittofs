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
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	return rec
}

// TestQuotaRoutesBindScope sets and reads back a quota for every supported
// scope, then checks all three appear in the listing and can be removed. The
// default-user scope has no {id} path segment, so its route must still bind
// {scope}: a route that hard-codes the literal leaves chi.URLParam(r, "scope")
// empty and the handler rejects its own scope.
func TestQuotaRoutesBindScope(t *testing.T) {
	router, jwtService, _ := newTestRouter(t, false)
	token := tokenFor(t, jwtService, models.RoleAdmin)

	cases := []struct {
		scope  string
		path   string
		wantID string
	}{
		{models.QuotaScopeUser, "/api/v1/shares/tiered/quotas/user/1000", "1000"},
		{models.QuotaScopeGroup, "/api/v1/shares/tiered/quotas/group/2000", "2000"},
		{models.QuotaScopeDefaultUser, "/api/v1/shares/tiered/quotas/default-user", "none"},
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
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode PUT response: %v", err)
			}
			if got.Scope != tc.scope {
				t.Errorf("scope = %q, want %q", got.Scope, tc.scope)
			}
			gotID := "none"
			if got.IdentityID != nil {
				gotID = fmt.Sprint(*got.IdentityID)
			}
			if gotID != tc.wantID {
				t.Errorf("identity_id = %s, want %s", gotID, tc.wantID)
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

// TestQuotaRouteRejectsBadTarget covers the two ways a {scope}[/{id}] path can
// be wrong: a user/group quota missing its id must complain about the id, and
// an unrecognised scope must be rejected with the offending value echoed.
func TestQuotaRouteRejectsBadTarget(t *testing.T) {
	router, jwtService, _ := newTestRouter(t, false)
	token := tokenFor(t, jwtService, models.RoleAdmin)

	cases := []struct {
		name     string
		path     string
		wantBody string
	}{
		{"missing id", "/api/v1/shares/tiered/quotas/user", "identity id is required"},
		{"unknown scope", "/api/v1/shares/tiered/quotas/wheel/7", "Invalid scope: wheel"},
		{"id on default-user", "/api/v1/shares/tiered/quotas/default-user/5", "takes no identity id"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doQuota(t, router, token, http.MethodPut, tc.path, `{"limit_bytes":"1GiB"}`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT %s = %d, want 400 (body=%q)", tc.path, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body = %q, want it to mention %q", rec.Body.String(), tc.wantBody)
			}
		})
	}
}
