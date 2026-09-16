package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestShareNameSpellingsReachTheShareTheyName drives the production router to
// pin which share a URL actually addresses.
//
// It goes through NewRouter rather than a router assembled for the test,
// because what a spelling resolves to is decided by the route tree and the
// middleware above it, not by the seam alone: a test router that omits either
// answers a question about itself. The quota routes carry the assertion because
// they need no share to exist and echo the resolved name back, so the check is
// on the share the request reached rather than on a string.
func TestShareNameSpellingsReachTheShareTheyName(t *testing.T) {
	router, jwtService, _, _ := newTestRouter(t, false)
	token := tokenFor(t, jwtService, models.RoleAdmin)

	tests := []struct {
		name    string
		segment string
		want    string
	}{
		{"a plain name", "export", "/export"},
		{"an escaped leading slash", "%2Fexport", "/export"},
		{"an escaped separator inside the name", "deep%2Fnested", "/deep/nested"},
		// The next two are one name, "/a%2Fb", in two spellings. Escaped whole
		// it reaches its share: the escaped leading slash keeps the request path
		// distinguishable from the default encoding of its decoded form, which
		// is what makes a router match on the escaped one.
		{"a literal percent, escaped whole", "%2Fa%252Fb", "/a%2Fb"},
		// With that leading slash stripped, every remaining escape is one the
		// default encoder reproduces, so the segment arrives already decoded and
		// the seam's decode is the second one.
		{"a literal percent, leading slash stripped", "a%252Fb", "/a/b"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := "/api/v1/shares/" + tc.segment + "/quotas"
			if rec := doAuthedRequest(t, router, token, http.MethodPut,
				path+"/user/1000", `{"limit_bytes":"1GiB"}`); rec.Code != http.StatusOK {
				t.Fatalf("PUT %s = %d, want 200 (body=%q)", path+"/user/1000", rec.Code, rec.Body.String())
			}

			rec := doAuthedRequest(t, router, token, http.MethodGet, path, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200 (body=%q)", path, rec.Code, rec.Body.String())
			}
			var listed []struct {
				ShareName string `json:"share_name"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
				t.Fatalf("decode %s: %v", path, err)
			}
			if len(listed) == 0 {
				t.Fatalf("GET %s reached no share", path)
			}
			if listed[0].ShareName != tc.want {
				t.Fatalf("a request for %q addressed share %q, want %q", tc.segment, listed[0].ShareName, tc.want)
			}
		})
	}
}
