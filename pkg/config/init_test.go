package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestInitConfig_Success(t *testing.T) {
	// Create a temporary directory to act as config dir
	tmpDir := t.TempDir()

	// Override XDG_CONFIG_HOME so getConfigDir() resolves to our temp directory.
	// Using HOME doesn't work on Windows where os.UserHomeDir() reads USERPROFILE.
	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)
	defer func() {
		if oldXDG != "" {
			_ = os.Setenv("XDG_CONFIG_HOME", oldXDG)
		} else {
			_ = os.Unsetenv("XDG_CONFIG_HOME")
		}
	}()

	configPath, err := InitConfig(false)
	if err != nil {
		t.Fatalf("InitConfig failed: %v", err)
	}

	// Verify config file was created
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Fatalf("Config file was not created at %s", configPath)
	}

	// Verify config file contains expected content
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("Failed to read config file: %v", err)
	}

	contentStr := string(content)
	expectedSections := []string{
		"# DittoFS Configuration File",
		"logging:",
		"cache:",
		"database:",
		"controlplane:",
		"admin:",
	}

	for _, section := range expectedSections {
		if !strings.Contains(contentStr, section) {
			t.Errorf("Config file missing section: %s", section)
		}
	}

	// Verify the generated file is valid YAML
	var cfg Config
	if err := yaml.Unmarshal(content, &cfg); err != nil {
		t.Fatalf("Generated config is not valid YAML: %v", err)
	}
}

func TestInitConfig_AlreadyExists(t *testing.T) {
	tmpDir := t.TempDir()

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)
	defer func() {
		if oldXDG != "" {
			_ = os.Setenv("XDG_CONFIG_HOME", oldXDG)
		} else {
			_ = os.Unsetenv("XDG_CONFIG_HOME")
		}
	}()

	// Create config first time
	_, err := InitConfig(false)
	if err != nil {
		t.Fatalf("First InitConfig failed: %v", err)
	}

	// Try to create again without force
	_, err = InitConfig(false)
	if err == nil {
		t.Fatal("Expected error when config already exists")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("Expected 'already exists' error, got: %v", err)
	}
}

func TestInitConfig_Force(t *testing.T) {
	tmpDir := t.TempDir()

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)
	defer func() {
		if oldXDG != "" {
			_ = os.Setenv("XDG_CONFIG_HOME", oldXDG)
		} else {
			_ = os.Unsetenv("XDG_CONFIG_HOME")
		}
	}()

	// Create config first time
	configPath, err := InitConfig(false)
	if err != nil {
		t.Fatalf("First InitConfig failed: %v", err)
	}

	// Get original file info
	origInfo, _ := os.Stat(configPath)

	// Create again with force
	_, err = InitConfig(true)
	if err != nil {
		t.Fatalf("InitConfig with force failed: %v", err)
	}

	// File should have been recreated (different mtime or we trust the operation)
	newInfo, _ := os.Stat(configPath)
	if newInfo.Size() == 0 {
		t.Fatal("Recreated config file is empty")
	}
	_ = origInfo // Silences unused warning
}

func TestInitConfigToPath_Success(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "custom", "config.yaml")

	err := InitConfigToPath(configPath, false)
	if err != nil {
		t.Fatalf("InitConfigToPath failed: %v", err)
	}

	// Verify config file was created
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Fatalf("Config file was not created at %s", configPath)
	}

	// Verify it's valid YAML
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("Failed to read config file: %v", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(content, &cfg); err != nil {
		t.Fatalf("Generated config is not valid YAML: %v", err)
	}
}

func TestInitConfigToPath_AlreadyExists(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// Create first time
	err := InitConfigToPath(configPath, false)
	if err != nil {
		t.Fatalf("First InitConfigToPath failed: %v", err)
	}

	// Try again without force
	err = InitConfigToPath(configPath, false)
	if err == nil {
		t.Fatal("Expected error when config already exists")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("Expected 'already exists' error, got: %v", err)
	}
}

func TestInitConfigToPath_Force(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// Create first time
	err := InitConfigToPath(configPath, false)
	if err != nil {
		t.Fatalf("First InitConfigToPath failed: %v", err)
	}

	// Create again with force
	err = InitConfigToPath(configPath, true)
	if err != nil {
		t.Fatalf("InitConfigToPath with force failed: %v", err)
	}

	// Verify file exists and has content
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("Failed to stat recreated config: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("Recreated config file is empty")
	}
}

func TestGeneratedConfigIsLoadable(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	err := InitConfigToPath(configPath, false)
	if err != nil {
		t.Fatalf("InitConfigToPath failed: %v", err)
	}

	// Load and verify
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Check key values
	if cfg.Logging.Level != "INFO" {
		t.Errorf("Expected INFO log level in generated config, got %q", cfg.Logging.Level)
	}
	if cfg.ControlPlane.Port != 8080 {
		t.Errorf("Expected port 8080 in generated config, got %d", cfg.ControlPlane.Port)
	}
	if cfg.Admin.Username != "admin" {
		t.Errorf("Expected admin username 'admin', got %q", cfg.Admin.Username)
	}
}

func TestGeneratedConfigHasJWTSecret(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	err := InitConfigToPath(configPath, false)
	if err != nil {
		t.Fatalf("InitConfigToPath failed: %v", err)
	}

	// Load and verify JWT secret is present and long enough
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if cfg.ControlPlane.JWT.Secret == "" {
		t.Error("Expected JWT secret to be generated")
	}
	if len(cfg.ControlPlane.JWT.Secret) < 32 {
		t.Errorf("Expected JWT secret to be at least 32 chars, got %d", len(cfg.ControlPlane.JWT.Secret))
	}
}
