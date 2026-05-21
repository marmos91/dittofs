package config

import (
	"testing"
	"time"
)

// Phase 11 Plan 02 (D-14/D-25):
// SyncerConfig exposes upload-pool / janitor knobs at the top-level Config
// so operators can tune the v0.15.0 syncer without recompiling. The defaults
// match D-25 (8 parallel uploads per share), D-14 (10-minute claim timeout),
// and D-24 (30-second tick).
//
// Phase 19 D-23 closed the Phase 18 D-16 `claim_batch_size` deprecation cycle:
// the field was set/defaulted in Phase 11 but never read by the syncer claim
// path. The defaults/validate/test assertions for it are gone.

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
