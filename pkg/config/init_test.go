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

	// Override the config directory for this test
	// We'll use environment variable to control this
	oldHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", oldHome) }()

	// Clear XDG_CONFIG_HOME
	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	_ = os.Unsetenv("XDG_CONFIG_HOME")
	defer func() {
		if oldXDG != "" {
			_ = os.Setenv("XDG_CONFIG_HOME", oldXDG)
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
		"content:",
		"metadata:",
		"shares:",
		"adapters:",
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
	oldHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", oldHome) }()

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	_ = os.Unsetenv("XDG_CONFIG_HOME")
	defer func() {
		if oldXDG != "" {
			_ = os.Setenv("XDG_CONFIG_HOME", oldXDG)
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

func TestInitConfig_ForceOverwrite(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", oldHome) }()

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	_ = os.Unsetenv("XDG_CONFIG_HOME")
	defer func() {
		if oldXDG != "" {
			_ = os.Setenv("XDG_CONFIG_HOME", oldXDG)
		}
	}()

	// Create config first time
	configPath, err := InitConfig(false)
	if err != nil {
		t.Fatalf("First InitConfig failed: %v", err)
	}

	// Modify the file
	if err := os.WriteFile(configPath, []byte("# Modified"), 0644); err != nil {
		t.Fatalf("Failed to modify config: %v", err)
	}

	// Force overwrite
	newPath, err := InitConfig(true)
	if err != nil {
		t.Fatalf("Force InitConfig failed: %v", err)
	}

	if newPath != configPath {
		t.Errorf("Expected same path, got different: %s vs %s", configPath, newPath)
	}

	// Verify file was overwritten (contains DittoFS header)
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("Failed to read config: %v", err)
	}

	if !strings.Contains(string(content), "# DittoFS Configuration File") {
		t.Error("Config file was not properly overwritten")
	}
}

func TestInitConfigToPath_Success(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "custom", "dittofs.yaml")

	err := InitConfigToPath(configPath, false)
	if err != nil {
		t.Fatalf("InitConfigToPath failed: %v", err)
	}

	// Verify file was created
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Fatalf("Config file was not created at %s", configPath)
	}

	// Verify it's valid YAML
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("Failed to read config: %v", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(content, &cfg); err != nil {
		t.Fatalf("Generated config is not valid YAML: %v", err)
	}
}

func TestInitConfigToPath_AlreadyExists(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// Create file first
	if err := os.WriteFile(configPath, []byte("existing"), 0644); err != nil {
		t.Fatalf("Failed to create existing file: %v", err)
	}

	// Try to init without force
	err := InitConfigToPath(configPath, false)
	if err == nil {
		t.Fatal("Expected error when file already exists")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("Expected 'already exists' error, got: %v", err)
	}
}

func TestInitConfigToPath_ForceOverwrite(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// Create file first
	if err := os.WriteFile(configPath, []byte("existing"), 0644); err != nil {
		t.Fatalf("Failed to create existing file: %v", err)
	}

	// Force overwrite
	err := InitConfigToPath(configPath, true)
	if err != nil {
		t.Fatalf("Force InitConfigToPath failed: %v", err)
	}

	// Verify file was overwritten
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("Failed to read config: %v", err)
	}

	if string(content) == "existing" {
		t.Error("File was not overwritten")
	}
}

func TestGenerateYAMLWithComments_ValidConfig(t *testing.T) {
	cfg := GetDefaultConfig()

	yaml, err := generateYAMLWithComments(cfg)
	if err != nil {
		t.Fatalf("generateYAMLWithComments failed: %v", err)
	}

	// Verify it contains comments
	if !strings.Contains(yaml, "#") {
		t.Error("Generated YAML should contain comments")
	}

	// Verify it contains major sections
	sections := []string{
		"logging:",
		"server:",
		"content:",
		"metadata:",
		"shares:",
		"adapters:",
	}

	for _, section := range sections {
		if !strings.Contains(yaml, section) {
			t.Errorf("Generated YAML missing section: %s", section)
		}
	}

	// Verify actual values are present
	if !strings.Contains(yaml, "INFO") {
		t.Error("Generated YAML should contain default INFO log level")
	}
	if !strings.Contains(yaml, "/export") {
		t.Error("Generated YAML should contain default /export share")
	}
	if !strings.Contains(yaml, "2049") {
		t.Error("Generated YAML should contain default NFS port 2049")
	}
}

func TestGeneratedConfigIsLoadable(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", oldHome) }()

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	_ = os.Unsetenv("XDG_CONFIG_HOME")
	defer func() {
		if oldXDG != "" {
			_ = os.Setenv("XDG_CONFIG_HOME", oldXDG)
		}
	}()

	// Generate config
	configPath, err := InitConfig(false)
	if err != nil {
		t.Fatalf("InitConfig failed: %v", err)
	}

	// Try to load it
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Failed to load generated config: %v", err)
	}

	// Validate it
	if err := Validate(cfg); err != nil {
		t.Fatalf("Generated config failed validation: %v", err)
	}
}

func TestGeneratedConfigValuesAreCorrect(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// Generate config
	err := InitConfigToPath(configPath, false)
	if err != nil {
		t.Fatalf("Failed to generate config: %v", err)
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
	if cfg.Adapters.NFS.Port != 2049 {
		t.Errorf("Expected port 2049 in generated config, got %d", cfg.Adapters.NFS.Port)
	}
	if len(cfg.Shares) != 1 {
		t.Errorf("Expected 1 share in generated config, got %d", len(cfg.Shares))
	}
	if cfg.Shares[0].Name != "/export" {
		t.Errorf("Expected share name '/export', got %q", cfg.Shares[0].Name)
	}
}
