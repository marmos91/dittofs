package runtime

import (
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// configMap adapts a plain map to the GetConfig interface ValidateBlockStoreConfig expects.
type configMap map[string]any

func (c configMap) GetConfig() (map[string]any, error) { return c, nil }

// TestValidateBlockStoreConfig_S3_SSRF verifies the s3 endpoint SSRF guard
// fires at config-validation time — before the create-time HealthCheck can
// dial a metadata/loopback/private host.
func TestValidateBlockStoreConfig_S3_SSRF(t *testing.T) {
	base := func(extra map[string]any) configMap {
		cfg := configMap{
			"bucket":            "b",
			"access_key_id":     "ak",
			"secret_access_key": "sk",
		}
		for k, v := range extra {
			cfg[k] = v
		}
		return cfg
	}
	cases := []struct {
		name    string
		cfg     configMap
		wantErr bool
	}{
		{"metadata_endpoint", base(map[string]any{"endpoint": "http://169.254.169.254/latest/meta-data"}), true},
		{"private_endpoint", base(map[string]any{"endpoint": "http://10.0.0.5:9000"}), true},
		{"private_endpoint_allowed", base(map[string]any{"endpoint": "http://10.0.0.5:9000", "allow_private_endpoint": true}), false},
		{"no_endpoint", base(nil), false},
		{"public_endpoint", base(map[string]any{"endpoint": "https://93.184.216.34"}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBlockStoreConfig(models.BlockStoreKindRemote, "s3", tc.cfg)
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
		})
	}
}

func TestValidateCompressionSubconfig(t *testing.T) {
	cases := []struct {
		name    string
		cfg     map[string]any
		wantErr string // substring; "" means accept
	}{
		{"absent", map[string]any{}, ""},
		{"empty_object_defaults_zstd", map[string]any{"compression": map[string]any{}}, ""},
		{"explicit_zstd", map[string]any{"compression": map[string]any{"algo": "zstd"}}, ""},
		{"explicit_lz4", map[string]any{"compression": map[string]any{"algo": "lz4"}}, ""},
		{"unknown_algo", map[string]any{"compression": map[string]any{"algo": "snappy"}}, "unsupported value"},
		{"wrong_type_block", map[string]any{"compression": "zstd"}, "expected object"},
		{"wrong_type_algo", map[string]any{"compression": map[string]any{"algo": 7}}, "expected string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCompressionSubconfig(tc.cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}
