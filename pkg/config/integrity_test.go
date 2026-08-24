package config

import (
	"testing"
	"time"
)

func TestIntegrityConfig_Defaults(t *testing.T) {
	var c IntegrityConfig
	c.ApplyDefaults()
	if !c.AutoScanEnabled() {
		t.Error("AutoScanEnabled() = false after defaults, want true (scan on by default)")
	}
	if c.AutoInterval != 24*time.Hour {
		t.Errorf("AutoInterval = %v, want 24h", c.AutoInterval)
	}
}

func TestIntegrityConfig_ExplicitOff(t *testing.T) {
	off := false
	c := IntegrityConfig{AutoEnabled: &off}
	c.ApplyDefaults()
	if c.AutoScanEnabled() {
		t.Error("AutoScanEnabled() = true, want false when explicitly disabled")
	}
}

func TestIntegrityConfig_Validate(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		wantErr  bool
	}{
		{"unset uses default", 0, false},
		{"negative rejected", -time.Second, true},
		{"below floor rejected", time.Minute, true},
		{"at floor accepted", 5 * time.Minute, false},
		{"daily accepted", 24 * time.Hour, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := IntegrityConfig{AutoInterval: tc.interval}
			if err := c.Validate(); (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
