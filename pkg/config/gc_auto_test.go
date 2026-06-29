package config

import (
	"testing"
	"time"
)

func TestGCConfig_AutoDefaults(t *testing.T) {
	var c GCConfig
	c.ApplyDefaults()

	if !c.AutoGCEnabled() {
		t.Error("AutoGCEnabled() = false after defaults, want true (auto-GC on by default)")
	}
	if c.AutoEnabled == nil || !*c.AutoEnabled {
		t.Errorf("AutoEnabled = %v, want non-nil true", c.AutoEnabled)
	}
	if c.AutoInterval != 15*time.Minute {
		t.Errorf("AutoInterval = %v, want 15m default", c.AutoInterval)
	}
}

func TestGCConfig_AutoDisabledRespected(t *testing.T) {
	off := false
	c := GCConfig{AutoEnabled: &off}
	c.ApplyDefaults()

	if c.AutoGCEnabled() {
		t.Error("AutoGCEnabled() = true, want false when explicitly disabled")
	}
	// ApplyDefaults must not flip an explicit false back to true.
	if c.AutoEnabled == nil || *c.AutoEnabled {
		t.Errorf("ApplyDefaults overwrote explicit AutoEnabled=false: %v", c.AutoEnabled)
	}
}

func TestGCConfig_Validate_AutoInterval(t *testing.T) {
	cases := []struct {
		name     string
		interval time.Duration
		wantErr  bool
	}{
		{"zero ok (uses default)", 0, false},
		{"15m ok", 15 * time.Minute, false},
		{"1m ok", time.Minute, false},
		{"30s rejected", 30 * time.Second, true},
		{"negative rejected", -time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := GCConfig{GracePeriod: time.Hour, DryRunSampleSize: 1000, AutoInterval: tc.interval}
			err := c.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate() = nil, want error for interval %v", tc.interval)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() = %v, want nil for interval %v", err, tc.interval)
			}
		})
	}
}
