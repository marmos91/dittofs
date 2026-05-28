package config

import "testing"

// SnapshotConfig exposes a single knob: sync_gate_concurrency. Default
// is 16; Validate enforces the [1, 256] inclusive range. Operators tune
// down for slow/restrictive remotes during snapshot verification and up
// only when remote-side capacity is plentiful.

func TestSnapshotConfig_ApplyDefaults_Defaults16(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)

	if cfg.Snapshot.SyncGateConcurrency != 16 {
		t.Errorf("SyncGateConcurrency: got %d, want 16", cfg.Snapshot.SyncGateConcurrency)
	}
}

func TestSnapshotConfig_ApplyDefaults_ExplicitValuePreserved(t *testing.T) {
	cfg := &Config{
		Snapshot: SnapshotConfig{SyncGateConcurrency: 64},
	}
	ApplyDefaults(cfg)

	if cfg.Snapshot.SyncGateConcurrency != 64 {
		t.Errorf("SyncGateConcurrency was overridden to %d, want 64", cfg.Snapshot.SyncGateConcurrency)
	}
}

func TestSnapshotConfig_ApplyDefaults_NegativeIsPreservedForValidation(t *testing.T) {
	// An explicit negative is preserved through ApplyDefaults so the
	// subsequent Validate pass can reject the operator typo. Silently
	// substituting 16 here would mask invalid YAML input.
	cfg := &Config{
		Snapshot: SnapshotConfig{SyncGateConcurrency: -1},
	}
	ApplyDefaults(cfg)

	if cfg.Snapshot.SyncGateConcurrency != -1 {
		t.Errorf("negative SyncGateConcurrency: got %d, want -1 (preserved for validation)", cfg.Snapshot.SyncGateConcurrency)
	}
	if err := cfg.Snapshot.Validate(); err == nil {
		t.Fatal("Validate() on negative SyncGateConcurrency returned nil, want range error")
	}
}

func TestSnapshotConfig_Validate_RangeBounds(t *testing.T) {
	tests := []struct {
		name        string
		concurrency int
		wantErr     bool
	}{
		{"negative", -1, true},
		{"zero", 0, true},
		{"min", 1, false},
		{"default", 16, false},
		{"max", 256, false},
		{"over-max", 257, true},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cfg := SnapshotConfig{SyncGateConcurrency: tc.concurrency}
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate(concurrency=%d) = nil, want error", tc.concurrency)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate(concurrency=%d) = %v, want nil", tc.concurrency, err)
			}
		})
	}
}

func TestSnapshotConfig_ValidatePassesOnDefaults(t *testing.T) {
	cfg := SnapshotConfig{}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() on defaulted SnapshotConfig returned %v, want nil", err)
	}
}
