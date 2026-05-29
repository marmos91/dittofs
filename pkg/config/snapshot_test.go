package config

import "testing"

// TestSnapshotConfigEmpty asserts SnapshotConfig has no required knobs.
// The struct is kept as a placeholder for future operator-tunable fields.
func TestSnapshotConfigEmpty(t *testing.T) {
	cfg := SnapshotConfig{}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() on empty SnapshotConfig returned %v, want nil", err)
	}
}
