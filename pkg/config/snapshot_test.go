package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestSnapshotConfigEmpty asserts SnapshotConfig validates cleanly with
// no operator-supplied knobs.
func TestSnapshotConfigEmpty(t *testing.T) {
	cfg := SnapshotConfig{}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() on empty SnapshotConfig returned %v, want nil", err)
	}
}

// TestSnapshotConfigRestoreHTTPTimeoutDefault asserts the zero value is
// rewritten to 30m by ApplyDefaults.
func TestSnapshotConfigRestoreHTTPTimeoutDefault(t *testing.T) {
	cfg := SnapshotConfig{}
	cfg.ApplyDefaults()
	if cfg.RestoreHTTPTimeout != 30*time.Minute {
		t.Fatalf("RestoreHTTPTimeout = %s, want 30m", cfg.RestoreHTTPTimeout)
	}
}

// TestSnapshotConfigRestoreHTTPTimeoutRoundTrip asserts a YAML-loaded
// value survives ApplyDefaults + Validate and is not overwritten.
func TestSnapshotConfigRestoreHTTPTimeoutRoundTrip(t *testing.T) {
	in := []byte("restore_http_timeout: 5m\n")
	var cfg SnapshotConfig
	if err := yaml.Unmarshal(in, &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if cfg.RestoreHTTPTimeout != 5*time.Minute {
		t.Fatalf("RestoreHTTPTimeout = %s, want 5m", cfg.RestoreHTTPTimeout)
	}
	cfg.ApplyDefaults()
	if cfg.RestoreHTTPTimeout != 5*time.Minute {
		t.Fatalf("after ApplyDefaults RestoreHTTPTimeout = %s, want 5m (not overwritten)", cfg.RestoreHTTPTimeout)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestSnapshotConfigRestoreHTTPTimeoutRejectsNegative asserts a negative
// value is surfaced by Validate.
func TestSnapshotConfigRestoreHTTPTimeoutRejectsNegative(t *testing.T) {
	cfg := SnapshotConfig{RestoreHTTPTimeout: -1 * time.Second}
	if err := cfg.Validate(); err == nil {
		t.Fatalf("Validate() = nil, want non-nil error for negative timeout")
	}
}
