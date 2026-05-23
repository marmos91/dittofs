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
	cfg, err := buildRemoteConfig("s3", "", "bucket", "us-east-1", "", "", "AK", "SK", "zstd")
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
	cfg, err := buildRemoteConfig("s3", "", "bucket", "us-east-1", "", "", "AK", "SK", "")
	if err != nil {
		t.Fatalf("buildRemoteConfig: %v", err)
	}
	m, _ := cfg.(map[string]any)
	if _, present := m["compression"]; present {
		t.Fatalf("compression key should be absent when flag empty: %#v", m)
	}
}

func TestBuildRemoteConfig_S3_RejectsInvalidAlgo(t *testing.T) {
	_, err := buildRemoteConfig("s3", "", "bucket", "us-east-1", "", "", "AK", "SK", "gzip")
	if err == nil || !strings.Contains(err.Error(), "invalid --compression") {
		t.Fatalf("err=%v, want invalid --compression error", err)
	}
}

func TestBuildRemoteConfig_JSONConfigShortCircuitsFlag(t *testing.T) {
	// --config takes the parsed JSON verbatim; --compression flag is
	// ignored when --config is set (matches existing flag interaction).
	cfg, err := buildRemoteConfig("s3", `{"bucket":"x"}`, "", "", "", "", "", "", "lz4")
	if err != nil {
		t.Fatalf("buildRemoteConfig: %v", err)
	}
	m, _ := cfg.(map[string]any)
	if _, present := m["compression"]; present {
		t.Fatalf("compression must not be injected when --config provided: %#v", m)
	}
}
