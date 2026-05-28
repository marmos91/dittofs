package config

import (
	"testing"
	"time"
)

// SyncerConfig exposes upload-pool / janitor knobs at the top-level
// Config so operators can tune the syncer without recompiling. The
// defaults are 8 parallel uploads per share, 10-minute claim timeout,
// and 30-second tick.
//
// The `claim_batch_size` field was set/defaulted historically but
// never read by the syncer claim path. The defaults/validate/test
// assertions for it have been removed.

func TestSyncerConfig_DefaultsApplied(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)

	if cfg.Syncer.UploadConcurrency != 8 {
		t.Errorf("UploadConcurrency: got %d, want 8", cfg.Syncer.UploadConcurrency)
	}
	if cfg.Syncer.ClaimTimeout != 10*time.Minute {
		t.Errorf("ClaimTimeout: got %v, want 10m", cfg.Syncer.ClaimTimeout)
	}
	if cfg.Syncer.Tick != 30*time.Second {
		t.Errorf("Tick: got %v, want 30s", cfg.Syncer.Tick)
	}
}

func TestSyncerConfig_ExplicitValuesPreserved(t *testing.T) {
	cfg := &Config{
		Syncer: SyncerConfig{
			UploadConcurrency: 16,
			ClaimTimeout:      5 * time.Minute,
			Tick:              15 * time.Second,
		},
	}
	ApplyDefaults(cfg)

	if cfg.Syncer.UploadConcurrency != 16 {
		t.Errorf("UploadConcurrency was overridden to %d, want 16", cfg.Syncer.UploadConcurrency)
	}
	if cfg.Syncer.ClaimTimeout != 5*time.Minute {
		t.Errorf("ClaimTimeout was overridden to %v, want 5m", cfg.Syncer.ClaimTimeout)
	}
	if cfg.Syncer.Tick != 15*time.Second {
		t.Errorf("Tick was overridden to %v, want 15s", cfg.Syncer.Tick)
	}
}

func TestSyncerConfig_ValidateRejectsUploadConcurrency(t *testing.T) {
	cfg := SyncerConfig{
		UploadConcurrency: 0,
		ClaimTimeout:      10 * time.Minute,
		Tick:              30 * time.Second,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() returned nil for UploadConcurrency=0, want error")
	}
}

func TestSyncerConfig_ValidatePassesOnDefaults(t *testing.T) {
	cfg := SyncerConfig{}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() on defaulted SyncerConfig returned %v, want nil", err)
	}
}
