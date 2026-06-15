package s3

import (
	"errors"
	"testing"
)

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
