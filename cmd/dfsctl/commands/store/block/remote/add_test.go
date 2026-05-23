package remote

import (
	"strings"
	"testing"
)

func TestBuildCompressionBlock(t *testing.T) {
	cases := []struct {
		algo     string
		wantAlgo string // "" → expect nil block
		wantErr  string // substring; "" → no error
	}{
		{"", "", ""},
		{"zstd", "zstd", ""},
		{"lz4", "lz4", ""},
		{"snappy", "", "invalid --compression"},
		{"none", "", "invalid --compression"},
		{"ZSTD", "", "invalid --compression"},
	}
	for _, tc := range cases {
		t.Run(tc.algo, func(t *testing.T) {
			block, err := buildCompressionBlock(tc.algo)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantAlgo == "" {
				if block != nil {
					t.Fatalf("expected nil block for empty flag, got %#v", block)
				}
				return
			}
			if got, _ := block["algo"].(string); got != tc.wantAlgo {
				t.Fatalf("algo=%v, want %s", block["algo"], tc.wantAlgo)
			}
		})
	}
}

func TestBuildRemoteConfig_S3_CompressionMergesIn(t *testing.T) {
	cfg, err := buildRemoteConfig("s3", "", "bucket", "us-east-1", "", "", "AK", "SK", "zstd", encryptionFlags{})
	if err != nil {
		t.Fatalf("buildRemoteConfig: %v", err)
	}
	m, ok := cfg.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", cfg)
	}
	comp, ok := m["compression"].(map[string]any)
	if !ok {
		t.Fatalf("missing compression block in config: %#v", m)
	}
	if comp["algo"] != "zstd" {
		t.Fatalf("algo=%v, want zstd", comp["algo"])
	}
}

func TestBuildRemoteConfig_S3_NoCompressionByDefault(t *testing.T) {
	cfg, err := buildRemoteConfig("s3", "", "bucket", "us-east-1", "", "", "AK", "SK", "", encryptionFlags{})
	if err != nil {
		t.Fatalf("buildRemoteConfig: %v", err)
	}
	m, _ := cfg.(map[string]any)
	if _, present := m["compression"]; present {
		t.Fatalf("compression key should be absent when flag empty: %#v", m)
	}
}

func TestBuildRemoteConfig_S3_RejectsInvalidAlgo(t *testing.T) {
	_, err := buildRemoteConfig("s3", "", "bucket", "us-east-1", "", "", "AK", "SK", "gzip", encryptionFlags{})
	if err == nil || !strings.Contains(err.Error(), "invalid --compression") {
		t.Fatalf("err=%v, want invalid --compression error", err)
	}
}

func TestBuildEncryptionBlock_Disabled(t *testing.T) {
	block, err := buildEncryptionBlock(encryptionFlags{})
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if block != nil {
		t.Fatalf("expected nil block, got %#v", block)
	}
}

func TestBuildEncryptionBlock_Local(t *testing.T) {
	block, err := buildEncryptionBlock(encryptionFlags{
		AEAD:    "aes-256-gcm",
		KeyKind: "local",
		KeyFile: "/etc/dittofs/keys/share.key",
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if block["aead"] != "aes-256-gcm" {
		t.Errorf("aead=%v", block["aead"])
	}
	key, _ := block["key"].(map[string]any)
	if key["kind"] != "local" || key["file"] != "/etc/dittofs/keys/share.key" {
		t.Errorf("key block: %#v", key)
	}
}

func TestBuildEncryptionBlock_KMIP(t *testing.T) {
	block, err := buildEncryptionBlock(encryptionFlags{
		AEAD:       "chacha20-poly1305",
		KeyKind:    "kmip",
		KMIPHost:   "kms.example.com:5696",
		KMIPCA:     "/etc/dittofs/kmip/ca.pem",
		KMIPCert:   "/etc/dittofs/kmip/client.pem",
		KMIPKey:    "/etc/dittofs/kmip/client.key",
		KMIPKeyUID: "abcd-1234",
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if block["aead"] != "chacha20-poly1305" {
		t.Errorf("aead=%v", block["aead"])
	}
	key, _ := block["key"].(map[string]any)
	if key["kind"] != "kmip" || key["endpoint"] != "kms.example.com:5696" || key["key_uid"] != "abcd-1234" || key["server_ca"] != "/etc/dittofs/kmip/ca.pem" {
		t.Errorf("key block: %#v", key)
	}
}

func TestBuildEncryptionBlock_Rejects(t *testing.T) {
	cases := []struct {
		name    string
		flags   encryptionFlags
		wantSub string
	}{
		{"missing-aead-with-key", encryptionFlags{KeyKind: "local", KeyFile: "/x"}, "--encryption-aead is required"},
		{"unknown-aead", encryptionFlags{AEAD: "rc4"}, "invalid --encryption-aead"},
		{"local-missing-file", encryptionFlags{AEAD: "aes-256-gcm", KeyKind: "local"}, "--encryption-key-file is required"},
		{"kmip-missing-host", encryptionFlags{AEAD: "aes-256-gcm", KeyKind: "kmip", KMIPCert: "/c", KMIPKey: "/k", KMIPKeyUID: "u"}, "kmip-endpoint"},
		{"unknown-kind", encryptionFlags{AEAD: "aes-256-gcm", KeyKind: "vault"}, "invalid --encryption-key-kind"},
		{"missing-kind", encryptionFlags{AEAD: "aes-256-gcm"}, "--encryption-key-kind is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildEncryptionBlock(tc.flags)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err=%v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

func TestBuildRemoteConfig_S3_EncryptionMergesIn(t *testing.T) {
	cfg, err := buildRemoteConfig("s3", "", "bucket", "us-east-1", "", "", "AK", "SK", "", encryptionFlags{
		AEAD:    "aes-256-gcm",
		KeyKind: "local",
		KeyFile: "/etc/dittofs/share.key",
	})
	if err != nil {
		t.Fatalf("buildRemoteConfig: %v", err)
	}
	m, _ := cfg.(map[string]any)
	enc, ok := m["encryption"].(map[string]any)
	if !ok {
		t.Fatalf("missing encryption block: %#v", m)
	}
	if enc["aead"] != "aes-256-gcm" {
		t.Errorf("aead=%v", enc["aead"])
	}
}

func TestBuildRemoteConfig_JSONConfigShortCircuitsFlag(t *testing.T) {
	// --config takes the parsed JSON verbatim; --compression flag is
	// ignored when --config is set (matches existing flag interaction).
	cfg, err := buildRemoteConfig("s3", `{"bucket":"x"}`, "", "", "", "", "", "", "lz4", encryptionFlags{})
	if err != nil {
		t.Fatalf("buildRemoteConfig: %v", err)
	}
	m, _ := cfg.(map[string]any)
	if _, present := m["compression"]; present {
		t.Fatalf("compression must not be injected when --config provided: %#v", m)
	}
}
