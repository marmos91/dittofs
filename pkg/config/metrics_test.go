package config

import (
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/api"
)

func TestMetricsConfig_ApplyDefaults(t *testing.T) {
	var c MetricsConfig
	c.ApplyDefaults()
	if c.Host != "127.0.0.1" {
		t.Errorf("host = %q, want 127.0.0.1", c.Host)
	}
	if c.Port != 9090 {
		t.Errorf("port = %d, want 9090", c.Port)
	}
	if c.Path != "/metrics" {
		t.Errorf("path = %q, want /metrics", c.Path)
	}
	if c.Auth != "none" {
		t.Errorf("auth = %q, want none", c.Auth)
	}
	if got := c.Addr(); got != "127.0.0.1:9090" {
		t.Errorf("Addr() = %q, want 127.0.0.1:9090", got)
	}
}

func TestMetricsConfig_ApplyDefaults_PreservesExplicit(t *testing.T) {
	c := MetricsConfig{Host: "0.0.0.0", Port: 1234, Path: "/m", Auth: "token"}
	c.ApplyDefaults()
	if c.Host != "0.0.0.0" || c.Port != 1234 || c.Path != "/m" || c.Auth != "token" {
		t.Errorf("ApplyDefaults overwrote explicit values: %+v", c)
	}
}

func TestMetricsConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     MetricsConfig
		wantErr bool
	}{
		{name: "disabled skips all checks", cfg: MetricsConfig{Enabled: false, Auth: "token", Path: "bad"}, wantErr: false},
		{name: "enabled none ok", cfg: MetricsConfig{Enabled: true, Auth: "none", Path: "/metrics"}, wantErr: false},
		{name: "token without token_file", cfg: MetricsConfig{Enabled: true, Auth: "token", Path: "/metrics"}, wantErr: true},
		{name: "token with token_file ok", cfg: MetricsConfig{Enabled: true, Auth: "token", TokenFile: "/x", Path: "/metrics"}, wantErr: false},
		{name: "path without leading slash", cfg: MetricsConfig{Enabled: true, Auth: "none", Path: "metrics"}, wantErr: true},
		{name: "tls cert without key", cfg: MetricsConfig{Enabled: true, Auth: "none", Path: "/metrics", TLS: api.TLSConfig{CertFile: "/c"}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestMetricsConfig_ValidateTokenMessage(t *testing.T) {
	c := MetricsConfig{Enabled: true, Auth: "token", Path: "/metrics"}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "token_file") {
		t.Fatalf("want token_file error, got %v", err)
	}
}
