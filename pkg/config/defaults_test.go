package config

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/bytesize"
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
	if cfg.Logging.Output != GetDefaultLogPath() {
		t.Errorf("Expected default log output %q, got %q", GetDefaultLogPath(), cfg.Logging.Output)
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

func TestApplyDefaults_RotationDefaults(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)

	if cfg.Logging.Rotation.MaxSize != 100 {
		t.Errorf("Expected default MaxSize 100, got %d", cfg.Logging.Rotation.MaxSize)
	}
	// MaxBackups and MaxAge should remain 0 (unlimited) when not set,
	// because 0 has meaning in lumberjack (keep all / no age limit).
	if cfg.Logging.Rotation.MaxBackups != 0 {
		t.Errorf("Expected MaxBackups 0 (keep all) when not set, got %d", cfg.Logging.Rotation.MaxBackups)
	}
	if cfg.Logging.Rotation.MaxAge != 0 {
		t.Errorf("Expected MaxAge 0 (no limit) when not set, got %d", cfg.Logging.Rotation.MaxAge)
	}
}

func TestApplyDefaults_PreservesZeroRotationValues(t *testing.T) {
	cfg := &Config{
		Logging: LoggingConfig{
			Rotation: LogRotationConfig{
				MaxSize:    50,
				MaxBackups: 0, // explicitly: keep all
				MaxAge:     0, // explicitly: no age limit
			},
		},
	}
	ApplyDefaults(cfg)

	if cfg.Logging.Rotation.MaxSize != 50 {
		t.Errorf("Expected explicit MaxSize 50 to be preserved, got %d", cfg.Logging.Rotation.MaxSize)
	}
	if cfg.Logging.Rotation.MaxBackups != 0 {
		t.Errorf("Expected explicit MaxBackups 0 to be preserved, got %d", cfg.Logging.Rotation.MaxBackups)
	}
	if cfg.Logging.Rotation.MaxAge != 0 {
		t.Errorf("Expected explicit MaxAge 0 to be preserved, got %d", cfg.Logging.Rotation.MaxAge)
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

func TestApplyDefaults_OffloaderDefaults(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)

	// Offloader values should be set to explicit defaults
	if cfg.Offloader.ParallelUploads != 16 {
		t.Errorf("Expected ParallelUploads 16, got %d", cfg.Offloader.ParallelUploads)
	}
	if cfg.Offloader.ParallelDownloads != 32 {
		t.Errorf("Expected ParallelDownloads 32, got %d", cfg.Offloader.ParallelDownloads)
	}
	if cfg.Offloader.PrefetchBlocks != 64 {
		t.Errorf("Expected PrefetchBlocks 64, got %d", cfg.Offloader.PrefetchBlocks)
	}
}

func TestApplyDefaults_OffloaderPreservesExplicit(t *testing.T) {
	cfg := &Config{
		Offloader: OffloaderConfig{
			ParallelUploads:   32,
			ParallelDownloads: 16,
			PrefetchBlocks:    8,
		},
	}
	ApplyDefaults(cfg)

	if cfg.Offloader.ParallelUploads != 32 {
		t.Errorf("Expected explicit ParallelUploads 32 preserved, got %d", cfg.Offloader.ParallelUploads)
	}
	if cfg.Offloader.ParallelDownloads != 16 {
		t.Errorf("Expected explicit ParallelDownloads 16 preserved, got %d", cfg.Offloader.ParallelDownloads)
	}
	if cfg.Offloader.PrefetchBlocks != 8 {
		t.Errorf("Expected explicit PrefetchBlocks 8 preserved, got %d", cfg.Offloader.PrefetchBlocks)
	}
}

func TestApplyDefaults_ReadCacheSizeNilGetsDefault(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)

	if cfg.Cache.ReadCacheSize == nil {
		t.Fatal("ReadCacheSize should be set to default when nil")
	}
	if *cfg.Cache.ReadCacheSize != bytesize.ByteSize(128*bytesize.MiB) {
		t.Errorf("Expected ReadCacheSize 128MiB, got %v", *cfg.Cache.ReadCacheSize)
	}
}

func TestApplyDefaults_ReadCacheSizeZeroPreserved(t *testing.T) {
	zero := bytesize.ByteSize(0)
	cfg := &Config{
		Cache: CacheConfig{ReadCacheSize: &zero},
	}
	ApplyDefaults(cfg)

	if cfg.Cache.ReadCacheSize == nil {
		t.Fatal("ReadCacheSize should not be nil when explicitly set to 0")
	}
	if *cfg.Cache.ReadCacheSize != 0 {
		t.Errorf("Expected ReadCacheSize 0 (disabled), got %v", *cfg.Cache.ReadCacheSize)
	}
}

func TestApplyDefaults_PrefetchWorkersNilGetsDefault(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)

	if cfg.Offloader.PrefetchWorkers == nil {
		t.Fatal("PrefetchWorkers should be set to default when nil")
	}
	if *cfg.Offloader.PrefetchWorkers != 4 {
		t.Errorf("Expected PrefetchWorkers 4, got %d", *cfg.Offloader.PrefetchWorkers)
	}
}

func TestApplyDefaults_PrefetchWorkersZeroPreserved(t *testing.T) {
	zero := 0
	cfg := &Config{
		Offloader: OffloaderConfig{PrefetchWorkers: &zero},
	}
	ApplyDefaults(cfg)

	if cfg.Offloader.PrefetchWorkers == nil {
		t.Fatal("PrefetchWorkers should not be nil when explicitly set to 0")
	}
	if *cfg.Offloader.PrefetchWorkers != 0 {
		t.Errorf("Expected PrefetchWorkers 0 (disabled), got %d", *cfg.Offloader.PrefetchWorkers)
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
