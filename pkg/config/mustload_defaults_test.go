package config

import (
	"strings"
	"testing"
)

// TestMustLoad_NoConfigFileFallsBackToDefaults verifies the #1245 fix: when no
// config file exists at the default location, MustLoad with an empty path must
// NOT hard-fail (which made systemd crash-loop). Instead it falls back to
// built-in defaults so the server boots. The empty XDG_CONFIG_HOME tmp dir
// guarantees DefaultConfigExists() is false.
func TestMustLoad_NoConfigFileFallsBackToDefaults(t *testing.T) {
	// Point the default config location at an empty temp dir (no config.yaml).
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if DefaultConfigExists() {
		t.Fatal("precondition failed: a config file unexpectedly exists at the default location")
	}

	cfg, err := MustLoad("")
	if err != nil {
		t.Fatalf("MustLoad(\"\") with no config file must fall back to defaults, got error: %v", err)
	}
	if cfg == nil {
		t.Fatal("MustLoad returned nil config without an error")
	}
	// Sanity: defaults were applied.
	if cfg.ControlPlane.Port == 0 {
		t.Fatalf("expected default control-plane port to be applied, got %d", cfg.ControlPlane.Port)
	}
}

// TestMustLoad_ExplicitMissingPathStillErrors verifies the fallback is scoped
// to the default-location case only: an operator who explicitly passes a
// --config path that does not exist still gets an actionable error (a typo'd
// path should not silently boot on defaults).
func TestMustLoad_ExplicitMissingPathStillErrors(t *testing.T) {
	missing := t.TempDir() + "/does-not-exist.yaml"
	_, err := MustLoad(missing)
	if err == nil {
		t.Fatal("MustLoad with an explicit missing config path must return an error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected an actionable 'not found' error, got: %v", err)
	}
}
