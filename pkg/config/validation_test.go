package config

import (
	"strings"
	"testing"
)

func TestValidate_ValidConfig(t *testing.T) {
	cfg := GetDefaultConfig()

	err := Validate(cfg)
	if err != nil {
		t.Errorf("Expected valid config to pass validation, got error: %v", err)
	}
}

func TestValidate_InvalidLogLevel(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Logging.Level = "INVALID"

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for invalid log level")
	}
	if !strings.Contains(err.Error(), "oneof") {
		t.Errorf("Expected 'oneof' validation error, got: %v", err)
	}
}

func TestValidate_InvalidLogFormat(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Logging.Format = "xml"

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for invalid log format")
	}
}

func TestValidate_InvalidAPIPort(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.ControlPlane.Port = 70000 // Out of range

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for port out of range")
	}
	if !strings.Contains(err.Error(), "max") {
		t.Errorf("Expected 'max' validation error, got: %v", err)
	}
}

func TestValidate_NegativePort(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.ControlPlane.Port = -1

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for negative port")
	}
}

func TestValidate_MissingCachePath(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Cache.Path = ""

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for missing cache path")
	}
	// The error should mention Cache.Path or cache path in some form
	errStr := strings.ToLower(err.Error())
	if !strings.Contains(errStr, "cache") || !strings.Contains(errStr, "path") {
		t.Errorf("Expected error about cache path, got: %v", err)
	}
}

func TestValidate_TelemetryEnabledWithoutEndpoint(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Telemetry.Enabled = true
	cfg.Telemetry.Endpoint = ""

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for telemetry enabled without endpoint")
	}
	if !strings.Contains(err.Error(), "telemetry") && !strings.Contains(err.Error(), "endpoint") {
		t.Errorf("Expected error about telemetry endpoint, got: %v", err)
	}
}

func TestValidate_TelemetrySampleRate(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Telemetry.Enabled = true
	cfg.Telemetry.Endpoint = "localhost:4317"
	cfg.Telemetry.SampleRate = 1.5 // Out of range (should be 0.0-1.0)

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for sample rate out of range")
	}
}

func TestValidate_LogLevelNormalization(t *testing.T) {
	// Test that validation accepts both uppercase and lowercase log levels
	testCases := []string{"info", "INFO", "debug", "DEBUG", "warn", "WARN", "error", "ERROR"}

	for _, level := range testCases {
		cfg := GetDefaultConfig()
		cfg.Logging.Level = level

		err := Validate(cfg)
		if err != nil {
			t.Errorf("Validation failed for level %q: %v", level, err)
		}

		// Validation should NOT normalize - level should remain as-is
		if cfg.Logging.Level != level {
			t.Errorf("Expected level to remain %q after validation, got %q", level, cfg.Logging.Level)
		}
	}

	// Test that normalization happens in ApplyDefaults
	cfg := &Config{Logging: LoggingConfig{Level: "info"}}
	ApplyDefaults(cfg)
	if cfg.Logging.Level != "INFO" {
		t.Errorf("Expected ApplyDefaults to normalize 'info' to 'INFO', got %q", cfg.Logging.Level)
	}
}
