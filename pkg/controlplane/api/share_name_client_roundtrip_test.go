package api

import (
	"net/http/httptest"
	"testing"

	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestAPIClientAddressesTheShareItWasGiven pins the bundled client's half of the
// round trip: the share name handed to a client call has to be the share the
// server acts on.
//
// The client spells a name into a URL path segment, and the server reads it back
// out; only the two together decide which share is reached, so the assertion is
// on the name the server echoes after a real request, not on the segment the
// client produced. A name holding a percent is the case that separates the two
// spellings — sent with its leading slash stripped it is decoded twice and names
// a different share.
func TestAPIClientAddressesTheShareItWasGiven(t *testing.T) {
	router, jwtService, _, _ := newTestRouter(t, false)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	client := apiclient.New(srv.URL).WithToken(tokenFor(t, jwtService, models.RoleAdmin))

	id := uint32(1000)
	for _, name := range []string{
		"/export",
		"export",
		"///export",
		"/deep/nested",
		"/a%2Fb",
		"a%2Fb",
		"/100%done",
		"/space name",
	} {
		t.Run(name, func(t *testing.T) {
			want := "/" + trimLeadingSlashes(name)

			set, err := client.SetQuota(name, models.QuotaScopeUser, &id,
				&apiclient.UpsertQuotaRequest{LimitBytes: "1GiB"})
			if err != nil {
				t.Fatalf("SetQuota(%q): %v", name, err)
			}
			if set.ShareName != want {
				t.Fatalf("SetQuota(%q) acted on share %q, want %q", name, set.ShareName, want)
			}

			listed, err := client.ListQuotas(name)
			if err != nil {
				t.Fatalf("ListQuotas(%q): %v", name, err)
			}
			if len(listed) == 0 {
				t.Fatalf("ListQuotas(%q) reached no share holding the quota just set", name)
			}
			if listed[0].ShareName != want {
				t.Fatalf("ListQuotas(%q) read back share %q, want %q", name, listed[0].ShareName, want)
			}
		})
	}
}

func trimLeadingSlashes(s string) string {
	for len(s) > 0 && s[0] == '/' {
		s = s[1:]
	}
	return s
}
