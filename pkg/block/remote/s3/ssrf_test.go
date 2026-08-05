package s3

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

// localhostURL rewrites a test server's URL to reach it through the DNS name
// "localhost" instead of the loopback literal it binds, so the dial is driven
// by name resolution rather than by a literal address in the URL.
func localhostURL(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse %q: %v", srv.URL, err)
	}
	u.Host = net.JoinHostPort("localhost", u.Port())
	return u.String()
}

// TestDialGuard_RefusesNameResolvingToBlockedAddress covers the DNS-rebinding
// bypass: the guard must act on the address actually being connected to, not
// on whatever the endpoint resolved to when the config was checked. The server
// is real and reachable, so without the dial guard this request succeeds.
func TestDialGuard_RefusesNameResolvingToBlockedAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := newHTTPClient(false).Get(localhostURL(t, srv))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("dial to a name resolving to loopback succeeded, want refusal")
	}
	if !errors.Is(err, ErrUnsafeEndpoint) {
		t.Fatalf("want ErrUnsafeEndpoint, got %v", err)
	}
}

// TestDialGuard_EscapeHatchAllowsPrivate verifies the allow_private_endpoint
// opt-out still reaches a private/loopback object store (MinIO, Localstack),
// and doubles as the proof that the guarded client can complete a real
// connection end to end rather than refusing everything.
func TestDialGuard_EscapeHatchAllowsPrivate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := newHTTPClient(true).Get(localhostURL(t, srv))
	if err != nil {
		t.Fatalf("allow_private_endpoint dial: want success, got %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
}

// TestRedirect_NotFollowed covers the redirect-follow bypass. The client runs
// with the private opt-out enabled, so the dial guard would happily connect to
// the redirect target; only the redirect policy stops it. The target counts
// its own hits, so the refusal is proven by the target never being contacted
// rather than by the shape of an error.
func TestRedirect_NotFollowed(t *testing.T) {
	// Counted atomically: the handler runs on the server's goroutine, and a
	// followed redirect must fail the assertion below rather than the race
	// detector.
	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	resp, err := newHTTPClient(true).Get(redirector.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if hits := targetHits.Load(); hits != 0 {
		t.Fatalf("redirect target was contacted %d time(s), want 0", hits)
	}
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status: want the 302 surfaced to the caller, got %d", resp.StatusCode)
	}
}

// TestCheckDialAddress verifies the guard's verdict on the addresses a dial can
// present, including the IPv4-mapped IPv6 forms that report false for every
// IPv4 classification unless they are unmapped first.
func TestCheckDialAddress(t *testing.T) {
	cases := []struct {
		name         string
		address      string
		allowPrivate bool
		wantErr      bool
	}{
		{"metadata", "169.254.169.254:80", false, true},
		{"metadata_allow_private", "169.254.169.254:80", true, true},
		{"metadata_v4_mapped", "[::ffff:169.254.169.254]:80", false, true},
		{"metadata_v4_mapped_allow_private", "[::ffff:169.254.169.254]:80", true, true},
		{"loopback_v4_mapped", "[::ffff:127.0.0.1]:9000", false, true},
		{"private_v4_mapped", "[::ffff:10.0.0.5]:9000", false, true},
		{"link_local_v6", "[fe80::1]:9000", false, true},
		{"unique_local_v6", "[fc00::1]:9000", false, true},
		{"loopback_v6", "[::1]:9000", false, true},
		{"loopback", "127.0.0.1:9000", false, true},
		{"private_10", "10.0.0.5:9000", false, true},
		{"private_172", "172.16.3.4:9000", false, true},
		{"private_192", "192.168.1.10:9000", false, true},
		{"unspecified", "0.0.0.0:9000", false, true},
		{"shared_address_space", "100.64.0.1:9000", false, true},
		{"shared_address_space_upper", "100.127.255.254:9000", false, true},
		{"shared_address_space_allow_private", "100.64.0.1:9000", true, false},
		// 100.0.0.0/10 and 100.128.0.0/10 sit outside the shared range and
		// stay reachable.
		{"public_100_below", "100.63.255.255:9000", false, false},
		{"public_100_above", "100.128.0.1:9000", false, false},
		{"this_network", "0.1.2.3:9000", false, true},
		{"this_network_allow_private", "0.1.2.3:9000", true, true},
		// The escape hatch relaxes private space, never the metadata range.
		{"loopback_allow_private", "127.0.0.1:9000", true, false},
		{"private_allow_private", "10.0.0.5:9000", true, false},
		{"unique_local_v6_allow_private", "[fc00::1]:9000", true, false},
		// Public endpoints connect as before.
		{"public_v4", "93.184.216.34:443", false, false},
		{"public_v6", "[2606:2800:220:1:248:1893:25c8:1946]:443", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDialAddress(tc.address, tc.allowPrivate)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("checkDialAddress(%q, allowPrivate=%v): want error, got nil", tc.address, tc.allowPrivate)
				}
				if !errors.Is(err, ErrUnsafeEndpoint) {
					t.Fatalf("checkDialAddress(%q): want ErrUnsafeEndpoint, got %v", tc.address, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkDialAddress(%q, allowPrivate=%v): want nil, got %v", tc.address, tc.allowPrivate, err)
			}
		})
	}
}

// TestValidateEndpoint_SSRF verifies the endpoint guard rejects the cloud
// metadata endpoint, loopback, link-local, and private/internal hosts while
// allowing public S3 endpoints. Literal IPs are used throughout so the test
// stays hermetic (no DNS).
func TestValidateEndpoint_SSRF(t *testing.T) {
	cases := []struct {
		name         string
		endpoint     string
		allowPrivate bool
		wantErr      bool
	}{
		// The canonical SSRF pivot — must be rejected even with the
		// private opt-out, since 169.254.169.254 is link-local.
		{"metadata_ip", "http://169.254.169.254/latest/meta-data", false, true},
		{"metadata_ip_allow_private", "http://169.254.169.254/latest/meta-data", true, true},
		// The same addresses written in IPv4-mapped IPv6 form, which classify
		// as neither link-local nor loopback unless they are unmapped first.
		{"metadata_ip_v4_mapped", "http://[::ffff:169.254.169.254]", false, true},
		{"metadata_ip_v4_mapped_allow_private", "http://[::ffff:169.254.169.254]", true, true},
		{"loopback_v4_mapped", "http://[::ffff:127.0.0.1]:9000", false, true},
		{"link_local_v6", "http://[fe80::1]:9000", false, true},
		{"loopback", "http://127.0.0.1:9000", false, true},
		{"loopback_v6", "http://[::1]:9000", false, true},
		{"private_10", "http://10.0.0.5:9000", false, true},
		{"private_192", "https://192.168.1.10", false, true},
		{"private_172", "http://172.16.3.4:9000", false, true},
		{"unspecified", "http://0.0.0.0:9000", false, true},
		// Private hosts permitted only under the explicit opt-out (MinIO /
		// Localstack co-located on a private network).
		{"private_10_allow", "http://10.0.0.5:9000", true, false},
		{"loopback_allow", "http://127.0.0.1:4566", true, false},
		// Public endpoints are always fine.
		{"public_ip", "https://93.184.216.34", false, false},
		{"empty", "", false, false},
		// A bare host normalizes to https:// + public literal IP.
		{"bare_public_ip", "93.184.216.34", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateEndpoint(tc.endpoint, tc.allowPrivate)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ValidateEndpoint(%q, allowPrivate=%v): want error, got nil", tc.endpoint, tc.allowPrivate)
				}
				if !errors.Is(err, ErrUnsafeEndpoint) {
					t.Fatalf("ValidateEndpoint(%q): want ErrUnsafeEndpoint, got %v", tc.endpoint, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateEndpoint(%q, allowPrivate=%v): want nil, got %v", tc.endpoint, tc.allowPrivate, err)
			}
		})
	}
}
