package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestBlockstoreJournalConfig_ApplyDefaults_SetsRemoteCacheAndBackpressure(t *testing.T) {
	c := BlockstoreJournalConfig{}
	c.ApplyDefaults()
	if c.DefaultRemoteCacheSize != 10<<30 {
		t.Errorf("DefaultRemoteCacheSize: got %d, want %d (10 GiB)", c.DefaultRemoteCacheSize, 10<<30)
	}
	if c.BackpressureMaxWait != 60*time.Second {
		t.Errorf("BackpressureMaxWait: got %s, want 60s", c.BackpressureMaxWait)
	}
}

func TestBlockstoreJournalConfig_ApplyDefaults_PreservesRemoteCacheAndBackpressure(t *testing.T) {
	c := BlockstoreJournalConfig{
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

func TestBlockstoreJournalConfig_Validate_RejectsNegativeBackpressureWait(t *testing.T) {
	c := BlockstoreJournalConfig{BackpressureMaxWait: -1}
	err := c.Validate()
	if err == nil {
		t.Fatalf("Validate() = nil for negative backpressure_max_wait, want error")
	}
	if !strings.Contains(err.Error(), "blockstore.journal.backpressure_max_wait") {
		t.Fatalf("Validate() error %q must contain dotted path 'blockstore.journal.backpressure_max_wait'", err.Error())
	}
}

func TestBlockstoreJournalConfig_Validate_AcceptsZeroNewKnobs(t *testing.T) {
	// Zero for the new knobs means "apply the built-in default" (filled by
	// ApplyDefaults), so Validate must accept a fully-zero config.
	c := BlockstoreJournalConfig{}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate(): got %v, want nil with zero new knobs", err)
	}
}

func TestConfig_UmbrellaApplyDefaults_InvokesBlockstoreLocal(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)
	if cfg.Blockstore.Journal.DefaultRemoteCacheSize != 10<<30 {
		t.Fatalf("umbrella ApplyDefaults must initialize Blockstore.Journal.DefaultRemoteCacheSize to 10 GiB; got %d", cfg.Blockstore.Journal.DefaultRemoteCacheSize)
	}
}

// max_log_bytes is the append-log pressure budget. Zero means "use the
// system-deduced default" (resolved at the runtime layer, since it depends on
// system memory), so ApplyDefaults must NOT rewrite a zero value, Validate must
// accept zero and any positive value, and the key must round-trip through YAML
// and the DITTOFS_* reflective env binding.

func TestBlockstoreJournalConfig_ApplyDefaults_LeavesMaxLogBytesZero(t *testing.T) {
	c := BlockstoreJournalConfig{}
	c.ApplyDefaults()
	if c.MaxLogBytes != 0 {
		t.Fatalf("MaxLogBytes: got %d, want 0 (zero defers to system-deduced default)", c.MaxLogBytes)
	}
}

func TestBlockstoreJournalConfig_ApplyDefaults_PreservesMaxLogBytes(t *testing.T) {
	c := BlockstoreJournalConfig{MaxLogBytes: 2 << 30}
	c.ApplyDefaults()
	if c.MaxLogBytes != 2<<30 {
		t.Fatalf("MaxLogBytes: got %d, want %d (2 GiB) preserved", c.MaxLogBytes, 2<<30)
	}
}

func TestBlockstoreJournalConfig_Validate_AcceptsMaxLogBytes(t *testing.T) {
	// Zero (defer to default) and a positive override must both validate.
	for _, v := range []uint64{0, 1 << 30, 8 << 30} {
		c := BlockstoreJournalConfig{MaxLogBytes: v}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() with max_log_bytes=%d: got %v, want nil", v, err)
		}
	}
}

func TestConfig_YAMLRoundTrip_BlockstoreJournalMaxLogBytes(t *testing.T) {
	yamlBody := []byte("blockstore:\n  journal:\n    max_log_bytes: 3221225472\n") // 3 GiB
	var cfg Config
	if err := yaml.Unmarshal(yamlBody, &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if cfg.Blockstore.Journal.MaxLogBytes != 3<<30 {
		t.Fatalf("YAML round-trip: got %d, want %d (3 GiB)", cfg.Blockstore.Journal.MaxLogBytes, 3<<30)
	}
}

func TestLoad_BlockstoreJournalMaxLogBytesFromEnv(t *testing.T) {
	content := `
controlplane:
  jwt:
    secret: "test-secret-key-for-testing-minimum-32-chars"
`
	path := writeConfigFile(t, content)

	t.Setenv("DITTOFS_BLOCKSTORE_JOURNAL_MAX_LOG_BYTES", "4294967296") // 4 GiB

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Blockstore.Journal.MaxLogBytes != 4<<30 {
		t.Fatalf("max_log_bytes from env: got %d, want %d (4 GiB)", cfg.Blockstore.Journal.MaxLogBytes, 4<<30)
	}
}

func TestBlockstoreJournalConfig_ApplyDefaults_SetsAbsolutePath(t *testing.T) {
	c := BlockstoreJournalConfig{}
	c.ApplyDefaults()
	if c.Path == "" {
		t.Fatal("Path: got empty, want a default")
	}
	if !filepath.IsAbs(c.Path) {
		t.Errorf("Path: got %q, want an absolute path", c.Path)
	}
	if want := filepath.Join(GetStateDir(), "blocks"); c.Path != want {
		t.Errorf("Path: got %q, want %q", c.Path, want)
	}
}

func TestBlockstoreJournalConfig_ApplyDefaults_PreservesPath(t *testing.T) {
	c := BlockstoreJournalConfig{Path: "/srv/dittofs/blocks"}
	c.ApplyDefaults()
	if c.Path != "/srv/dittofs/blocks" {
		t.Errorf("Path: got %q, want the configured value preserved", c.Path)
	}
}

func TestBlockstoreJournalConfig_ApplyDefaults_ExpandsHomePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	c := BlockstoreJournalConfig{Path: "~/dittofs-blocks"}
	c.ApplyDefaults()
	if want := filepath.Join(home, "dittofs-blocks"); c.Path != want {
		t.Errorf("Path: got %q, want %q", c.Path, want)
	}
}

// A relative path would resolve against whatever directory the server happens
// to be started from, putting a share's only local copy somewhere the next
// start cannot find. Validate must refuse it rather than let it through.
func TestBlockstoreJournalConfig_Validate_RejectsRelativePath(t *testing.T) {
	c := BlockstoreJournalConfig{Path: "blocks"}
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate: got nil, want an error for a relative path")
	}
	if !strings.Contains(err.Error(), "blockstore.journal.path") {
		t.Errorf("Validate: error %q does not name the offending key", err)
	}
}

// Empty means "apply the default" and is only reachable before ApplyDefaults,
// so it must not be reported as an operator error.
func TestBlockstoreJournalConfig_Validate_AcceptsEmptyPath(t *testing.T) {
	c := BlockstoreJournalConfig{}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate: got %v, want nil for an unset path", err)
	}
}
