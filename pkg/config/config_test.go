package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// yamlSafePath converts a filesystem path to a YAML-safe representation.
// On Windows, backslashes in double-quoted YAML strings are interpreted as
// escape sequences (e.g. \U -> Unicode escape), causing parse errors.
func yamlSafePath(p string) string {
	return filepath.ToSlash(p)
}

func TestLoad_DefaultConfig(t *testing.T) {
	// Create a temporary config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// Write minimal config with new structure
	configContent := `
logging:
  level: "INFO"

cache:
  path: "` + yamlSafePath(tmpDir) + `/cache"
  size: 100Mi

database:
  type: sqlite

controlplane:
  port: 8080
  jwt:
    secret: "test-secret-key-for-testing-minimum-32-chars"
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	// Load config
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Verify defaults were applied
	if cfg.Logging.Format != "text" {
		t.Errorf("Expected default format 'text', got %q", cfg.Logging.Format)
	}
	if cfg.Logging.Output != "stdout" {
		t.Errorf("Expected default output 'stdout', got %q", cfg.Logging.Output)
	}
	if cfg.ShutdownTimeout != 30*time.Second {
		t.Errorf("Expected default shutdown_timeout 30s, got %v", cfg.ShutdownTimeout)
	}
	if cfg.ControlPlane.Port != 8080 {
		t.Errorf("Expected control plane port 8080, got %d", cfg.ControlPlane.Port)
	}
}

func TestLoad_NoConfigFile(t *testing.T) {
	// Loading with no config file returns a valid default config.
	// This allows users to run the server without a config file for quick testing.
	tmpDir := t.TempDir()
	nonExistentPath := filepath.Join(tmpDir, "nonexistent.yaml")

	cfg, err := Load(nonExistentPath)
	if err != nil {
		t.Fatalf("Expected no error when loading default config, got: %v", err)
	}

	// Verify default config is returned
	if cfg == nil {
		t.Fatal("Expected default config to be returned")
	}

	// Verify default API port
	if cfg.ControlPlane.Port != 8080 {
		t.Errorf("Expected default API port 8080, got %d", cfg.ControlPlane.Port)
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "invalid.yaml")

	// Write invalid YAML
	configContent := `
logging:
  level: INFO
  invalid yaml here [[[
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	// Should return error
	_, err := Load(configPath)
	if err == nil {
		t.Fatal("Expected error with invalid YAML, got nil")
	}
}

func TestLoad_TOML(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")

	configContent := `
[logging]
level = "WARN"
format = "json"

[cache]
path = "` + yamlSafePath(tmpDir) + `/cache"
size = "100Mi"

[database]
type = "sqlite"

[api]
port = 8080

[api.jwt]
secret = "test-secret-key-for-testing-minimum-32-chars"
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Failed to load TOML config: %v", err)
	}

	if cfg.Logging.Level != "WARN" {
		t.Errorf("Expected level 'WARN', got %q", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("Expected format 'json', got %q", cfg.Logging.Format)
	}
}

func TestGetDefaultConfig(t *testing.T) {
	cfg := GetDefaultConfig()

	// Verify all defaults are set
	if cfg.Logging.Level != "INFO" {
		t.Errorf("Expected default log level 'INFO', got %q", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "text" {
		t.Errorf("Expected default log format 'text', got %q", cfg.Logging.Format)
	}
	if cfg.Logging.Output != "stdout" {
		t.Errorf("Expected default log output 'stdout', got %q", cfg.Logging.Output)
	}
	if cfg.ShutdownTimeout != 30*time.Second {
		t.Errorf("Expected default shutdown timeout 30s, got %v", cfg.ShutdownTimeout)
	}
	if cfg.ControlPlane.Port != 8080 {
		t.Errorf("Expected default API port 8080, got %d", cfg.ControlPlane.Port)
	}
	if cfg.Admin.Username != "admin" {
		t.Errorf("Expected default admin username 'admin', got %q", cfg.Admin.Username)
	}
}

func TestConfigExists(t *testing.T) {
	// Should return false for non-existent config
	// Note: This test assumes there's no config in the default location
	// or we're in a test environment where XDG_CONFIG_HOME is not set

	// We can't easily test this without mocking the environment
	// So we'll skip for now or make it a table test with temp dirs
}

func TestGetDefaultConfigPath(t *testing.T) {
	path := GetDefaultConfigPath()

	// Should contain dittofs and config.yaml
	if !filepath.IsAbs(path) {
		t.Errorf("Expected absolute path, got %q", path)
	}
	if filepath.Base(path) != "config.yaml" {
		t.Errorf("Expected filename 'config.yaml', got %q", filepath.Base(path))
	}
}

func TestGetConfigDir(t *testing.T) {
	dir := GetConfigDir()

	// Should contain dittofs
	if filepath.Base(dir) != "dittofs" {
		t.Errorf("Expected directory name 'dittofs', got %q", filepath.Base(dir))
	}
}

func TestLoad_EnvironmentVariables(t *testing.T) {
	// Set environment variables
	_ = os.Setenv("DITTOFS_LOGGING_LEVEL", "ERROR")
	_ = os.Setenv("DITTOFS_CONTROLPLANE_PORT", "9090")
	defer func() {
		_ = os.Unsetenv("DITTOFS_LOGGING_LEVEL")
		_ = os.Unsetenv("DITTOFS_CONTROLPLANE_PORT")
	}()

	// Create minimal config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `
logging:
  level: "INFO"

cache:
  path: "` + yamlSafePath(tmpDir) + `/cache"
  size: 100Mi

database:
  type: sqlite

controlplane:
  port: 8080
  jwt:
    secret: "test-secret-key-for-testing-minimum-32-chars"
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Verify environment variables override config file
	if cfg.Logging.Level != "ERROR" {
		t.Errorf("Expected level 'ERROR' from env var, got %q", cfg.Logging.Level)
	}
	if cfg.ControlPlane.Port != 9090 {
		t.Errorf("Expected port 9090 from env var, got %d", cfg.ControlPlane.Port)
	}
}
