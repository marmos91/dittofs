package s3

import (
	"context"
	"strings"
	"testing"
)

func TestValidateEndpoint(t *testing.T) {
	// Private/loopback are gated by the opt-in; ensure it is OFF for the
	// default-reject cases.
	t.Setenv(allowPrivateEndpointsEnv, "")

	rejected := []struct {
		name     string
		endpoint string
	}{
		{"metadata endpoint with scheme", "http://169.254.169.254/latest/meta-data/"},
		{"metadata endpoint bare", "169.254.169.254"},
		{"metadata endpoint host:port", "169.254.169.254:80"},
		{"link-local v6", "http://[fe80::1]:80"},
		{"loopback v4", "http://127.0.0.1:9000"},
		{"loopback bare", "127.0.0.1:9000"},
		{"loopback v6", "http://[::1]:9000"},
		{"private 10.x", "http://10.0.0.5:9000"},
		{"private 192.168.x", "192.168.1.10:9000"},
		{"unspecified", "http://0.0.0.0:9000"},
		{"non-http scheme", "file:///etc/passwd"},
	}
	for _, tc := range rejected {
		if err := ValidateEndpoint(tc.endpoint); err == nil {
			t.Errorf("ValidateEndpoint(%q) [%s] = nil, want rejection", tc.endpoint, tc.name)
		}
	}

	accepted := []struct {
		name     string
		endpoint string
	}{
		{"empty (default AWS)", ""},
		{"public hostname", "s3.amazonaws.com"},
		{"public hostname with scheme", "https://s3.eu-west-1.amazonaws.com"},
		{"internal hostname (left to network policy)", "minio.internal:9000"},
		{"public IP", "https://93.184.216.34:443"},
	}
	for _, tc := range accepted {
		if err := ValidateEndpoint(tc.endpoint); err != nil {
			t.Errorf("ValidateEndpoint(%q) [%s] = %v, want nil", tc.endpoint, tc.name, err)
		}
	}
}

func TestValidateEndpoint_PrivateOptIn(t *testing.T) {
	t.Setenv(allowPrivateEndpointsEnv, "1")

	// Loopback / private now allowed.
	for _, ep := range []string{"http://127.0.0.1:9000", "http://10.0.0.5:9000", "192.168.1.10:9000"} {
		if err := ValidateEndpoint(ep); err != nil {
			t.Errorf("with opt-in, ValidateEndpoint(%q) = %v, want nil", ep, err)
		}
	}

	// Link-local / metadata endpoint stays rejected even with the opt-in.
	if err := ValidateEndpoint("http://169.254.169.254/"); err == nil {
		t.Errorf("opt-in must NOT relax the link-local/metadata reject")
	}
}

// TestNewFromConfig_RejectsSSRFEndpoint verifies the SSRF guard fires at the
// construction chokepoint, so a DB-persisted malicious endpoint never reaches
// the dial regardless of whether write-time validation ran.
func TestNewFromConfig_RejectsSSRFEndpoint(t *testing.T) {
	t.Setenv(allowPrivateEndpointsEnv, "")
	_, err := NewFromConfig(context.Background(), Config{
		Bucket:    "b",
		AccessKey: "ak",
		SecretKey: "sk",
		Endpoint:  "http://169.254.169.254/latest/meta-data/",
	})
	if err == nil {
		t.Fatal("NewFromConfig with metadata-endpoint = nil error, want SSRF rejection")
	}
	if !strings.Contains(err.Error(), "link-local") {
		t.Errorf("expected link-local rejection, got %v", err)
	}
}
