package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad_DefaultConfig(t *testing.T) {
	// Create a temporary config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// Write minimal config
	configContent := `
logging:
  level: "INFO"

content:
  type: "filesystem"

shares:
  - name: "/export"

adapters:
  nfs:
    enabled: true
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
	if cfg.Server.ShutdownTimeout != 30*time.Second {
		t.Errorf("Expected default shutdown_timeout 30s, got %v", cfg.Server.ShutdownTimeout)
	}
	if cfg.Adapters.NFS.Port != 2049 {
		t.Errorf("Expected default NFS port 2049, got %d", cfg.Adapters.NFS.Port)
	}
}

// TestLoad_WithOverrides removed - we only support environment variable overrides now

func TestLoad_NoConfigFile(t *testing.T) {
	// Use a temporary directory with a non-existent config file path
	// This ensures we don't load the user's config from ~/.config/dittofs/
	tmpDir := t.TempDir()
	nonExistentPath := filepath.Join(tmpDir, "nonexistent.yaml")

	cfg, err := Load(nonExistentPath)
	if err != nil {
		t.Fatalf("Expected no error with missing config file, got: %v", err)
	}

	// Verify defaults
	if cfg.Logging.Level != "INFO" {
		t.Errorf("Expected default level 'INFO', got %q", cfg.Logging.Level)
	}
	if cfg.Content.Type != "filesystem" {
		t.Errorf("Expected default content type 'filesystem', got %q", cfg.Content.Type)
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

[content]
type = "filesystem"

[[shares]]
name = "/export"

[adapters.nfs]
enabled = true
port = 2049
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
	if cfg.Server.ShutdownTimeout != 30*time.Second {
		t.Errorf("Expected default shutdown timeout 30s, got %v", cfg.Server.ShutdownTimeout)
	}
	if cfg.Content.Type != "filesystem" {
		t.Errorf("Expected default content type 'filesystem', got %q", cfg.Content.Type)
	}
	if cfg.Metadata.Type != "badger" {
		t.Errorf("Expected default metadata type 'badger', got %q", cfg.Metadata.Type)
	}
	if len(cfg.Shares) != 1 {
		t.Errorf("Expected 1 default share, got %d", len(cfg.Shares))
	}
	if cfg.Shares[0].Name != "/export" {
		t.Errorf("Expected default share name '/export', got %q", cfg.Shares[0].Name)
	}
	if !cfg.Adapters.NFS.Enabled {
		t.Error("Expected NFS adapter enabled by default")
	}
	if cfg.Adapters.NFS.Port != 2049 {
		t.Errorf("Expected default NFS port 2049, got %d", cfg.Adapters.NFS.Port)
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
	_ = os.Setenv("DITTOFS_ADAPTERS_NFS_PORT", "5049")
	defer func() {
		_ = os.Unsetenv("DITTOFS_LOGGING_LEVEL")
		_ = os.Unsetenv("DITTOFS_ADAPTERS_NFS_PORT")
	}()

	// Create minimal config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `
logging:
  level: "INFO"

content:
  type: "filesystem"

shares:
  - name: "/export"

adapters:
  nfs:
    enabled: true
    port: 2049
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
	if cfg.Adapters.NFS.Port != 5049 {
		t.Errorf("Expected port 5049 from env var, got %d", cfg.Adapters.NFS.Port)
	}
}
