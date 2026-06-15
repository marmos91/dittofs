package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// BlockstoreLocalConfig exposes the dedup_lru_size knob at the
// top-level Config so operators can tune the dedup LRU without
// recompiling. Default 4096 slots; Validate rejects non-positive
// values; YAML round-trip works under the canonical loader pattern.

func TestBlockstoreLocalConfig_ApplyDefaults_SetsDedupLRUSizeTo4096(t *testing.T) {
	c := BlockstoreLocalConfig{}
	c.ApplyDefaults()
	if c.DedupLRUSize != 4096 {
		t.Fatalf("DedupLRUSize: got %d, want 4096", c.DedupLRUSize)
	}
}

func TestBlockstoreLocalConfig_ApplyDefaults_PreservesNonZero(t *testing.T) {
	c := BlockstoreLocalConfig{DedupLRUSize: 8192}
	c.ApplyDefaults()
	if c.DedupLRUSize != 8192 {
		t.Fatalf("DedupLRUSize: got %d, want 8192 preserved", c.DedupLRUSize)
	}
}

func TestBlockstoreLocalConfig_Validate_RejectsZero(t *testing.T) {
	c := BlockstoreLocalConfig{DedupLRUSize: 0}
	err := c.Validate()
	if err == nil {
		t.Fatalf("Validate() = nil for zero size, want error")
	}
	if !strings.Contains(err.Error(), "blockstore.local.dedup_lru_size") {
		t.Fatalf("Validate() error %q must contain dotted path 'blockstore.local.dedup_lru_size'", err.Error())
	}
}

func TestBlockstoreLocalConfig_Validate_RejectsNegative(t *testing.T) {
	c := BlockstoreLocalConfig{DedupLRUSize: -1}
	if err := c.Validate(); err == nil {
		t.Fatalf("Validate() = nil for -1, want error")
	}
}

func TestBlockstoreLocalConfig_Validate_AcceptsPositive(t *testing.T) {
	c := BlockstoreLocalConfig{DedupLRUSize: 1024}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate(): got %v, want nil for 1024", err)
	}
}

func TestBlockstoreLocalConfig_ApplyDefaults_SetsRemoteCacheAndBackpressure(t *testing.T) {
	c := BlockstoreLocalConfig{}
	c.ApplyDefaults()
	if c.DefaultRemoteCacheSize != 10<<30 {
		t.Errorf("DefaultRemoteCacheSize: got %d, want %d (10 GiB)", c.DefaultRemoteCacheSize, 10<<30)
	}
	if c.BackpressureMaxWait != 60*time.Second {
		t.Errorf("BackpressureMaxWait: got %s, want 60s", c.BackpressureMaxWait)
	}
}

func TestBlockstoreLocalConfig_ApplyDefaults_PreservesRemoteCacheAndBackpressure(t *testing.T) {
	c := BlockstoreLocalConfig{
		DefaultRemoteCacheSize: 1 << 30,
		BackpressureMaxWait:    30 * time.Second,
	}
	c.ApplyDefaults()
	if c.DefaultRemoteCacheSize != 1<<30 {
		t.Errorf("DefaultRemoteCacheSize: got %d, want 1 GiB preserved", c.DefaultRemoteCacheSize)
	}
	if c.BackpressureMaxWait != 30*time.Second {
		t.Errorf("BackpressureMaxWait: got %s, want 30s preserved", c.BackpressureMaxWait)
	}
}

func TestBlockstoreLocalConfig_Validate_RejectsNegativeBackpressureWait(t *testing.T) {
	c := BlockstoreLocalConfig{DedupLRUSize: 1024, BackpressureMaxWait: -1}
	err := c.Validate()
	if err == nil {
		t.Fatalf("Validate() = nil for negative backpressure_max_wait, want error")
	}
	if !strings.Contains(err.Error(), "blockstore.local.backpressure_max_wait") {
		t.Fatalf("Validate() error %q must contain dotted path 'blockstore.local.backpressure_max_wait'", err.Error())
	}
}

func TestBlockstoreLocalConfig_Validate_AcceptsZeroNewKnobs(t *testing.T) {
	// Zero for the new knobs means "apply the built-in default" (filled by
	// ApplyDefaults), so Validate must accept a config that only sets the
	// required DedupLRUSize.
	c := BlockstoreLocalConfig{DedupLRUSize: 1024}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate(): got %v, want nil with zero new knobs", err)
	}
}

func TestConfig_UmbrellaApplyDefaults_InvokesBlockstoreLocal(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)
	if cfg.Blockstore.Local.DedupLRUSize != 4096 {
		t.Fatalf("umbrella ApplyDefaults must initialize Blockstore.Local.DedupLRUSize to 4096; got %d", cfg.Blockstore.Local.DedupLRUSize)
	}
}

func TestConfig_YAMLRoundTrip_BlockstoreLocalDedupLRUSize(t *testing.T) {
	// Mirrors init_test.go's yaml.Unmarshal loader pattern. Uses gopkg.in/yaml.v3.
	yamlBody := []byte("blockstore:\n  local:\n    dedup_lru_size: 2048\n")
	var cfg Config
	if err := yaml.Unmarshal(yamlBody, &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if cfg.Blockstore.Local.DedupLRUSize != 2048 {
		t.Fatalf("YAML round-trip: got %d, want 2048", cfg.Blockstore.Local.DedupLRUSize)
	}
}
