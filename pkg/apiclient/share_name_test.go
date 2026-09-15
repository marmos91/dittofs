package apiclient

import (
	"net/url"
	"testing"
)

// TestNormalizeShareNameForAPI_SpellsANameTheServerReadsBack pins the round trip
// this helper exists for: the segment it produces, once escaped, must name the
// same share on the server as the name that went in.
//
// The server's half is metadata.NormalizeShareNameFromURL applied to the segment
// a router hands a handler, which is the escaped one only while the request path
// differs from the default encoding of its decoded form. serverReads reproduces
// both steps, so a spelling that loses the name fails here rather than at a 404.
func TestNormalizeShareNameForAPI_SpellsANameTheServerReadsBack(t *testing.T) {
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
			segment := url.PathEscape(normalizeShareNameForAPI(name))
			got, err := serverReads(segment)
			if err != nil {
				t.Fatalf("share %q sent as %q: %v", name, segment, err)
			}
			want := "/" + trimLeadingSlashes(name)
			if got != want {
				t.Fatalf("share %q sent as %q arrives as %q, want %q", name, segment, got, want)
			}
		})
	}
}

// serverReads mirrors the server side: parse the request URI the way net/http
// does, take the segment the router would match on, and apply the seam's decode.
func serverReads(segment string) (string, error) {
	u, err := url.ParseRequestURI("/api/v1/shares/" + segment)
	if err != nil {
		return "", err
	}
	routed := u.RawPath
	if routed == "" {
		routed = u.Path
	}
	param := routed[len("/api/v1/shares/"):]
	decoded, err := url.PathUnescape(param)
	if err != nil {
		decoded = param
	}
	return "/" + trimLeadingSlashes(decoded), nil
}

func trimLeadingSlashes(s string) string {
	for len(s) > 0 && s[0] == '/' {
		s = s[1:]
	}
	return s
}
