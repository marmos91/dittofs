package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestBadgerMetadataConfig_DefaultsAreZero pins the design contract that an
// unset cache size stays 0 — the signal that defers to RAM-relative auto-sizing
// inside the badger engine (#1245 Bug D). ApplyDefaults must NOT substitute a
// fixed number.
func TestBadgerMetadataConfig_DefaultsAreZero(t *testing.T) {
	c := BadgerMetadataConfig{}
	c.ApplyDefaults()
	if c.BlockCacheSizeMB != 0 {
		t.Errorf("BlockCacheSizeMB default: got %d, want 0 (auto-size)", c.BlockCacheSizeMB)
	}
	if c.IndexCacheSizeMB != 0 {
		t.Errorf("IndexCacheSizeMB default: got %d, want 0 (auto-size)", c.IndexCacheSizeMB)
	}
}

func TestBadgerMetadataConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     BadgerMetadataConfig
		wantErr bool
	}{
		{"zero ok (auto-size)", BadgerMetadataConfig{}, false},
		{"positive ok", BadgerMetadataConfig{BlockCacheSizeMB: 2048, IndexCacheSizeMB: 1024}, false},
		{"negative block rejected", BadgerMetadataConfig{BlockCacheSizeMB: -1}, true},
		{"negative index rejected", BadgerMetadataConfig{IndexCacheSizeMB: -1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

// TestBadgerMetadataConfig_Validate_ErrorMentionsDottedPath asserts the error
// carries the canonical dotted config key so operators can pinpoint it.
func TestBadgerMetadataConfig_Validate_ErrorMentionsDottedPath(t *testing.T) {
	c := BadgerMetadataConfig{BlockCacheSizeMB: -5}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "metadata.badger.block_cache_mb") {
		t.Fatalf("error must mention metadata.badger.block_cache_mb, got %v", err)
	}
}

// TestConfig_UmbrellaApplyDefaults_InvokesMetadata guards the ApplyDefaults
// fan-out wires the metadata sub-section (no panic on a zero Config).
func TestConfig_UmbrellaApplyDefaults_InvokesMetadata(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)
	if cfg.Metadata.Badger.BlockCacheSizeMB != 0 {
		t.Fatalf("metadata.badger.block_cache_mb default: got %d, want 0", cfg.Metadata.Badger.BlockCacheSizeMB)
	}
}

// TestConfig_YAMLRoundTrip_MetadataBadger asserts the new keys parse from a YAML
// body via the canonical loader pattern.
func TestConfig_YAMLRoundTrip_MetadataBadger(t *testing.T) {
	yamlBody := []byte("metadata:\n  badger:\n    block_cache_mb: 2048\n    index_cache_mb: 1024\n")
	var cfg Config
	if err := yaml.Unmarshal(yamlBody, &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if cfg.Metadata.Badger.BlockCacheSizeMB != 2048 {
		t.Errorf("block_cache_mb: got %d, want 2048", cfg.Metadata.Badger.BlockCacheSizeMB)
	}
	if cfg.Metadata.Badger.IndexCacheSizeMB != 1024 {
		t.Errorf("index_cache_mb: got %d, want 1024", cfg.Metadata.Badger.IndexCacheSizeMB)
	}
}

// TestLoad_MetadataBadgerFromFile verifies the keys load through the full
// viper-backed Load() path from a config file.
func TestLoad_MetadataBadgerFromFile(t *testing.T) {
	content := `
database:
  type: sqlite
controlplane:
  jwt:
    secret: "test-secret-key-for-testing-minimum-32-chars"
metadata:
  badger:
    block_cache_mb: 3072
    index_cache_mb: 1536
`
	cfg, err := Load(writeConfigFile(t, content))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Metadata.Badger.BlockCacheSizeMB != 3072 {
		t.Errorf("block_cache_mb from file: got %d, want 3072", cfg.Metadata.Badger.BlockCacheSizeMB)
	}
	if cfg.Metadata.Badger.IndexCacheSizeMB != 1536 {
		t.Errorf("index_cache_mb from file: got %d, want 1536", cfg.Metadata.Badger.IndexCacheSizeMB)
	}
}

// TestLoad_MetadataBadgerFromEnv verifies env overrides resolve through the
// reflective env binding (DITTOFS_METADATA_BADGER_*), even when the key is
// absent from the file.
func TestLoad_MetadataBadgerFromEnv(t *testing.T) {
	content := `
database:
  type: sqlite
controlplane:
  jwt:
    secret: "test-secret-key-for-testing-minimum-32-chars"
`
	path := writeConfigFile(t, content)

	t.Setenv("DITTOFS_METADATA_BADGER_BLOCK_CACHE_MB", "4096")
	t.Setenv("DITTOFS_METADATA_BADGER_INDEX_CACHE_MB", "2048")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Metadata.Badger.BlockCacheSizeMB != 4096 {
		t.Errorf("block_cache_mb from env: got %d, want 4096", cfg.Metadata.Badger.BlockCacheSizeMB)
	}
	if cfg.Metadata.Badger.IndexCacheSizeMB != 2048 {
		t.Errorf("index_cache_mb from env: got %d, want 2048", cfg.Metadata.Badger.IndexCacheSizeMB)
	}
}

// TestLoad_MetadataBadgerNegativeRejected verifies a negative size fails Load
// validation.
func TestLoad_MetadataBadgerNegativeRejected(t *testing.T) {
	content := `
database:
  type: sqlite
controlplane:
  jwt:
    secret: "test-secret-key-for-testing-minimum-32-chars"
metadata:
  badger:
    block_cache_mb: -1
`
	if _, err := Load(writeConfigFile(t, content)); err == nil {
		t.Fatal("expected Load to reject negative metadata.badger.block_cache_mb")
	}
}

// TestConfigEnvKeys_CoversMetadataBadger guards that the reflective key walk
// includes the new nested keys (so BindEnv binds them — otherwise the env
// override would silently drop).
func TestConfigEnvKeys_CoversMetadataBadger(t *testing.T) {
	keys := configEnvKeys()
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[k] = true
	}
	for _, want := range []string{"metadata.badger.block_cache_mb", "metadata.badger.index_cache_mb"} {
		if !set[want] {
			t.Errorf("configEnvKeys() missing %q", want)
		}
	}
}
