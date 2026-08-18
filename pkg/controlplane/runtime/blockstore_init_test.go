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

func TestValidateParallelUploads(t *testing.T) {
	cases := []struct {
		name    string
		cfg     map[string]any
		wantErr string // substring; "" means accept
	}{
		{"absent", map[string]any{}, ""},
		{"zero_means_auto", map[string]any{"parallel_uploads": float64(0)}, ""},
		{"valid", map[string]any{"parallel_uploads": float64(16)}, ""},
		{"max_boundary", map[string]any{"parallel_uploads": float64(256)}, ""},
		{"negative", map[string]any{"parallel_uploads": float64(-1)}, "between 0 and 256"},
		{"over_max", map[string]any{"parallel_uploads": float64(257)}, "between 0 and 256"},
		{"non_integer", map[string]any{"parallel_uploads": float64(2.5)}, "expected integer"},
		{"wrong_type", map[string]any{"parallel_uploads": "8"}, "expected number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateParallelUploads(tc.cfg)
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

func TestValidateEncryptionSubconfig(t *testing.T) {
	cases := []struct {
		name    string
		cfg     map[string]any
		wantErr string // substring; "" means accept
	}{
		{"absent", map[string]any{}, ""},
		{"local_ok", map[string]any{"encryption": map[string]any{
			"key": map[string]any{"kind": "local", "file": "/etc/dittofs/share.key"},
		}}, ""},
		{"local_with_aead", map[string]any{"encryption": map[string]any{
			"aead": "xchacha20-poly1305",
			"key":  map[string]any{"kind": "local", "file": "/k"},
		}}, ""},
		{"kmip_ok", map[string]any{"encryption": map[string]any{
			"key": map[string]any{
				"kind": "kmip", "endpoint": "kmip.example:5696", "key_uid": "uid-1",
				"client_cert": "/c.pem", "client_key": "/c.key",
			},
		}}, ""},
		{"wrong_type_block", map[string]any{"encryption": "on"}, "expected JSON object"},
		{"unknown_aead", map[string]any{"encryption": map[string]any{
			"aead": "rot13",
			"key":  map[string]any{"kind": "local", "file": "/k"},
		}}, "unsupported aead"},
		{"missing_kind", map[string]any{"encryption": map[string]any{}}, "kind is required"},
		{"unknown_kind", map[string]any{"encryption": map[string]any{
			"key": map[string]any{"kind": "vault"},
		}}, "unsupported value"},
		{"local_missing_file", map[string]any{"encryption": map[string]any{
			"key": map[string]any{"kind": "local"},
		}}, "file is required"},
		{"kmip_missing_endpoint", map[string]any{"encryption": map[string]any{
			"key": map[string]any{"kind": "kmip", "key_uid": "uid-1"},
		}}, "endpoint is required"},
		{"kmip_missing_key_uid", map[string]any{"encryption": map[string]any{
			"key": map[string]any{"kind": "kmip", "endpoint": "kmip.example:5696"},
		}}, "key_uid is required"},
		{"kmip_missing_client_cert", map[string]any{"encryption": map[string]any{
			"key": map[string]any{
				"kind": "kmip", "endpoint": "kmip.example:5696", "key_uid": "uid-1",
				"client_key": "/c.key",
			},
		}}, "client_cert"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateEncryptionSubconfig(tc.cfg)
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

// TestValidateBlockStoreConfig_S3_RejectsBadEncryption pins that the s3 case
// actually calls the encryption validator — a validator nobody wires up is
// dead code, and the bad config would only surface at share-attach time.
func TestValidateBlockStoreConfig_S3_RejectsBadEncryption(t *testing.T) {
	cfg := configMap{
		"bucket":            "b",
		"access_key_id":     "ak",
		"secret_access_key": "sk",
		"encryption":        map[string]any{"key": map[string]any{"kind": "vault"}},
	}
	err := ValidateBlockStoreConfig(models.BlockStoreKindRemote, "s3", cfg)
	if err == nil {
		t.Fatal("want error for an unsupported key provider kind, got nil")
	}
	if !strings.Contains(err.Error(), "encryption.key.kind") {
		t.Fatalf("error %q does not mention encryption.key.kind", err)
	}
}
