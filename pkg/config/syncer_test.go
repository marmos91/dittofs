package config

import (
	"testing"
	"time"
)

// Phase 11 Plan 02 (D-13/D-14/D-25):
// SyncerConfig exposes claim-batch / upload-pool / janitor knobs at the
// top-level Config so operators can tune the v0.15.0 syncer without
// recompiling. The defaults match D-13 (32-block batches), D-25 (8 parallel
// uploads per share), D-14 (10-minute claim timeout), and D-24 (30-second
// tick).

func TestSyncerConfig_DefaultsApplied(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)

	if cfg.Syncer.ClaimBatchSize != 32 {
		t.Errorf("ClaimBatchSize: got %d, want 32", cfg.Syncer.ClaimBatchSize)
	}
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
			ClaimBatchSize:    64,
			UploadConcurrency: 16,
			ClaimTimeout:      5 * time.Minute,
			Tick:              15 * time.Second,
		},
	}
	ApplyDefaults(cfg)

	if cfg.Syncer.ClaimBatchSize != 64 {
		t.Errorf("ClaimBatchSize was overridden to %d, want 64", cfg.Syncer.ClaimBatchSize)
	}
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

func TestSyncerConfig_ValidateRejectsClaimBatchSize(t *testing.T) {
	cfg := SyncerConfig{
		ClaimBatchSize:    0,
		UploadConcurrency: 8,
		ClaimTimeout:      10 * time.Minute,
		Tick:              30 * time.Second,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() returned nil for ClaimBatchSize=0, want error")
	}
}

func TestSyncerConfig_ValidateRejectsUploadConcurrency(t *testing.T) {
	cfg := SyncerConfig{
		ClaimBatchSize:    32,
		UploadConcurrency: 0,
		ClaimTimeout:      10 * time.Minute,
		Tick:              30 * time.Second,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() returned nil for UploadConcurrency=0, want error")
	}
}

func TestSyncerConfig_ValidateRejectsConcurrencyAboveBatch(t *testing.T) {
	cfg := SyncerConfig{
		ClaimBatchSize:    8,
		UploadConcurrency: 32,
		ClaimTimeout:      10 * time.Minute,
		Tick:              30 * time.Second,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() returned nil for UploadConcurrency > ClaimBatchSize, want error")
	}
}

func TestSyncerConfig_ValidatePassesOnDefaults(t *testing.T) {
	cfg := SyncerConfig{}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() on defaulted SyncerConfig returned %v, want nil", err)
	}
}
