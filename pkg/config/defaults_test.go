package config

import (
	"testing"
	"time"
)

func TestApplyDefaults_Logging(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)

	if cfg.Logging.Level != "INFO" {
		t.Errorf("Expected default log level 'INFO', got %q", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "text" {
		t.Errorf("Expected default log format 'text', got %q", cfg.Logging.Format)
	}
	if cfg.Logging.Output != "stdout" {
		t.Errorf("Expected default log output 'stdout', got %q", cfg.Logging.Output)
	}
}

func TestApplyDefaults_ShutdownTimeout(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)

	if cfg.ShutdownTimeout != 30*time.Second {
		t.Errorf("Expected default shutdown timeout 30s, got %v", cfg.ShutdownTimeout)
	}
}

func TestApplyDefaults_ControlPlane(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)

	if cfg.ControlPlane.Port != 8080 {
		t.Errorf("Expected default API port 8080, got %d", cfg.ControlPlane.Port)
	}
	if cfg.ControlPlane.ReadTimeout != 10*time.Second {
		t.Errorf("Expected default read timeout 10s, got %v", cfg.ControlPlane.ReadTimeout)
	}
	if cfg.ControlPlane.WriteTimeout != 10*time.Second {
		t.Errorf("Expected default write timeout 10s, got %v", cfg.ControlPlane.WriteTimeout)
	}
	if cfg.ControlPlane.IdleTimeout != 60*time.Second {
		t.Errorf("Expected default idle timeout 60s, got %v", cfg.ControlPlane.IdleTimeout)
	}
}

func TestApplyDefaults_Admin(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)

	if cfg.Admin.Username != "admin" {
		t.Errorf("Expected default admin username 'admin', got %q", cfg.Admin.Username)
	}
}

func TestApplyDefaults_PreservesExplicitValues(t *testing.T) {
	cfg := &Config{
		Logging: LoggingConfig{
			Level:  "DEBUG",
			Format: "json",
			Output: "/var/log/dittofs.log",
		},
		ShutdownTimeout: 60 * time.Second,
		Admin: AdminConfig{
			Username: "customadmin",
			Email:    "admin@example.com",
		},
	}

	ApplyDefaults(cfg)

	// Verify explicit values were preserved
	if cfg.Logging.Level != "DEBUG" {
		t.Errorf("Expected explicit level 'DEBUG' to be preserved, got %q", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("Expected explicit format 'json' to be preserved, got %q", cfg.Logging.Format)
	}
	if cfg.Logging.Output != "/var/log/dittofs.log" {
		t.Errorf("Expected explicit output to be preserved, got %q", cfg.Logging.Output)
	}
	if cfg.ShutdownTimeout != 60*time.Second {
		t.Errorf("Expected explicit timeout 60s to be preserved, got %v", cfg.ShutdownTimeout)
	}
	if cfg.Admin.Username != "customadmin" {
		t.Errorf("Expected explicit admin username to be preserved, got %q", cfg.Admin.Username)
	}
}

func TestGetDefaultConfig_IsValid(t *testing.T) {
	cfg := GetDefaultConfig()

	// The default config should pass validation
	err := Validate(cfg)
	if err != nil {
		t.Errorf("Default config should be valid, got error: %v", err)
	}
}

func TestGetDefaultConfig_HasRequiredFields(t *testing.T) {
	cfg := GetDefaultConfig()

	// Check all required sections are present
	if cfg.Logging.Level == "" {
		t.Error("Default config missing logging level")
	}
	if cfg.ControlPlane.Port == 0 {
		t.Error("Default config missing API port")
	}
	if cfg.Admin.Username == "" {
		t.Error("Default config missing admin username")
	}
	if cfg.Cache.Path == "" {
		t.Error("Default config missing cache path")
	}
}
