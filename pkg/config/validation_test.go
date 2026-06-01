package config

import (
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/store"
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

func TestValidate_DatabaseInFanOut(t *testing.T) {
	t.Run("UnsupportedType", func(t *testing.T) {
		cfg := GetDefaultConfig()
		cfg.Database.Type = "mysql"
		if err := Validate(cfg); err == nil {
			t.Fatal("expected Validate to reject unsupported database.type via Database.Validate fan-out")
		}
	})

	t.Run("PostgresMissingHost", func(t *testing.T) {
		cfg := GetDefaultConfig()
		cfg.Database = store.Config{
			Type: store.DatabaseTypePostgres,
			Postgres: store.PostgresConfig{
				// Host omitted on purpose.
				Database: "dittofs",
				User:     "dittofs",
			},
		}
		err := Validate(cfg)
		if err == nil {
			t.Fatal("expected Validate to reject postgres config missing host")
		}
		if !strings.Contains(err.Error(), "host") {
			t.Errorf("expected host error, got %v", err)
		}
	})
}

func TestValidate_KerberosInFanOut(t *testing.T) {
	t.Run("NegativeContextTTL", func(t *testing.T) {
		cfg := GetDefaultConfig()
		cfg.Kerberos.ContextTTL = -1 * time.Second
		if err := Validate(cfg); err == nil {
			t.Fatal("expected Validate to reject negative kerberos.context_ttl")
		}
	})

	t.Run("NegativeMaxClockSkew", func(t *testing.T) {
		cfg := GetDefaultConfig()
		cfg.Kerberos.MaxClockSkew = -1 * time.Second
		if err := Validate(cfg); err == nil {
			t.Fatal("expected Validate to reject negative kerberos.max_clock_skew")
		}
	})

	t.Run("NegativeMaxContexts", func(t *testing.T) {
		cfg := GetDefaultConfig()
		cfg.Kerberos.MaxContexts = -1
		if err := Validate(cfg); err == nil {
			t.Fatal("expected Validate to reject negative kerberos.max_contexts")
		}
	})

	t.Run("UnsupportedStrategy", func(t *testing.T) {
		cfg := GetDefaultConfig()
		cfg.Kerberos.IdentityMapping.Strategy = "ldap"
		err := Validate(cfg)
		if err == nil {
			t.Fatal("expected Validate to reject unsupported identity-mapping strategy")
		}
		if !strings.Contains(err.Error(), "strategy") {
			t.Errorf("expected strategy error, got %v", err)
		}
	})

	t.Run("DefaultStrategyPasses", func(t *testing.T) {
		cfg := GetDefaultConfig() // strategy defaulted to "static"
		if err := Validate(cfg); err != nil {
			t.Fatalf("expected default (static) strategy to pass, got %v", err)
		}
	})
}
