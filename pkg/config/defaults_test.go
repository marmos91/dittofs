package config

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/adapter/nfs"
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

func TestApplyDefaults_Server(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)

	if cfg.Server.ShutdownTimeout != 30*time.Second {
		t.Errorf("Expected default shutdown timeout 30s, got %v", cfg.Server.ShutdownTimeout)
	}
}

func TestApplyDefaults_Content(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)

	if cfg.Content.Type != "filesystem" {
		t.Errorf("Expected default content type 'filesystem', got %q", cfg.Content.Type)
	}

	// Check filesystem defaults
	if cfg.Content.Filesystem == nil {
		t.Fatal("Expected Filesystem map to be initialized")
	}
	if path, ok := cfg.Content.Filesystem["path"]; !ok || path != "/tmp/dittofs-content" {
		t.Errorf("Expected default filesystem path '/tmp/dittofs-content', got %v", path)
	}

	// Check memory defaults
	if cfg.Content.Memory == nil {
		t.Fatal("Expected Memory map to be initialized")
	}
	if maxSize, ok := cfg.Content.Memory["max_size_bytes"]; !ok || maxSize != uint64(1073741824) {
		t.Errorf("Expected default memory max_size_bytes 1073741824, got %v", maxSize)
	}
}

func TestApplyDefaults_Metadata(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)

	if cfg.Metadata.Type != "memory" {
		t.Errorf("Expected default metadata type 'memory', got %q", cfg.Metadata.Type)
	}

	if cfg.Metadata.Memory == nil {
		t.Fatal("Expected Memory map to be initialized")
	}

	// DumpRestricted should default to false
	if cfg.Metadata.DumpRestricted {
		t.Error("Expected dump_restricted to default to false")
	}
}

func TestApplyDefaults_Shares(t *testing.T) {
	cfg := &Config{
		Shares: []ShareConfig{
			{
				Name: "/export",
			},
		},
	}
	ApplyDefaults(cfg)

	share := cfg.Shares[0]

	// Check allowed clients initialized
	if share.AllowedClients == nil {
		t.Error("Expected AllowedClients to be initialized")
	}

	// Check denied clients initialized
	if share.DeniedClients == nil {
		t.Error("Expected DeniedClients to be initialized")
	}

	// Check auth methods defaulted
	if len(share.AllowedAuthMethods) != 2 {
		t.Errorf("Expected 2 default auth methods, got %d", len(share.AllowedAuthMethods))
	}

	// Check anonymous UID/GID
	if share.IdentityMapping.AnonymousUID != 65534 {
		t.Errorf("Expected default anonymous UID 65534, got %d", share.IdentityMapping.AnonymousUID)
	}
	if share.IdentityMapping.AnonymousGID != 65534 {
		t.Errorf("Expected default anonymous GID 65534, got %d", share.IdentityMapping.AnonymousGID)
	}

	// Check root attributes
	if share.RootAttr.Mode != 0755 {
		t.Errorf("Expected default root mode 0755, got 0%o", share.RootAttr.Mode)
	}
}

func TestApplyDefaults_NFS(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)

	nfs := cfg.Adapters.NFS

	// Note: ApplyDefaults now enables NFS by default when in unconfigured state.
	// This ensures configs loaded without a config file pass validation.
	// Users can explicitly disable by setting enabled: false and port: 2049 in their config.
	if !nfs.Enabled {
		t.Error("Expected NFS Enabled to be true after ApplyDefaults on unconfigured state")
	}
	if nfs.Port != 2049 {
		t.Errorf("Expected default NFS port 2049, got %d", nfs.Port)
	}
	if nfs.MaxConnections != 0 {
		t.Errorf("Expected default max_connections 0, got %d", nfs.MaxConnections)
	}
	if nfs.ReadTimeout != 5*time.Minute {
		t.Errorf("Expected default read_timeout 5m, got %v", nfs.ReadTimeout)
	}
	if nfs.WriteTimeout != 30*time.Second {
		t.Errorf("Expected default write_timeout 30s, got %v", nfs.WriteTimeout)
	}
	if nfs.IdleTimeout != 5*time.Minute {
		t.Errorf("Expected default idle_timeout 5m, got %v", nfs.IdleTimeout)
	}
	if nfs.ShutdownTimeout != 30*time.Second {
		t.Errorf("Expected default shutdown_timeout 30s, got %v", nfs.ShutdownTimeout)
	}
	if nfs.MetricsLogInterval != 5*time.Minute {
		t.Errorf("Expected default metrics_log_interval 5m, got %v", nfs.MetricsLogInterval)
	}
}

func TestApplyDefaults_PreservesExplicitValues(t *testing.T) {
	cfg := &Config{
		Logging: LoggingConfig{
			Level:  "DEBUG",
			Format: "json",
			Output: "/var/log/dittofs.log",
		},
		Server: ServerConfig{
			ShutdownTimeout: 60 * time.Second,
		},
		Content: ContentConfig{
			Type: "memory",
			Filesystem: map[string]any{
				"path": "/custom/path",
			},
		},
		Metadata: MetadataConfig{
			Type:           "memory",
			DumpRestricted: true,
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
	if cfg.Server.ShutdownTimeout != 60*time.Second {
		t.Errorf("Expected explicit timeout 60s to be preserved, got %v", cfg.Server.ShutdownTimeout)
	}
	if cfg.Content.Type != "memory" {
		t.Errorf("Expected explicit content type 'memory' to be preserved, got %q", cfg.Content.Type)
	}
	if !cfg.Metadata.DumpRestricted {
		t.Error("Expected explicit dump_restricted true to be preserved")
	}
}

func TestApplyDefaults_MultipleSharesWithMixedDefaults(t *testing.T) {
	cfg := &Config{
		Shares: []ShareConfig{
			{
				Name:     "/export1",
				ReadOnly: true,
			},
			{
				Name: "/export2",
				IdentityMapping: IdentityMappingConfig{
					MapAllToAnonymous: true,
					AnonymousUID:      1000,
					AnonymousGID:      1000,
				},
			},
		},
	}

	ApplyDefaults(cfg)

	// First share
	if cfg.Shares[0].IdentityMapping.AnonymousUID != 65534 {
		t.Errorf("Expected default anonymous UID 65534 for first share, got %d",
			cfg.Shares[0].IdentityMapping.AnonymousUID)
	}

	// Second share should preserve explicit values
	if cfg.Shares[1].IdentityMapping.AnonymousUID != 1000 {
		t.Errorf("Expected explicit anonymous UID 1000 for second share, got %d",
			cfg.Shares[1].IdentityMapping.AnonymousUID)
	}
	if cfg.Shares[1].IdentityMapping.AnonymousGID != 1000 {
		t.Errorf("Expected explicit anonymous GID 1000 for second share, got %d",
			cfg.Shares[1].IdentityMapping.AnonymousGID)
	}
}

func TestApplyDefaults_NFSDisabled(t *testing.T) {
	cfg := &Config{
		Adapters: AdaptersConfig{
			NFS: nfs.NFSConfig{
				Enabled: false,
			},
		},
	}

	ApplyDefaults(cfg)

	// Even disabled, other defaults should still be applied
	if cfg.Adapters.NFS.Port != 2049 {
		t.Errorf("Expected default port even when disabled, got %d", cfg.Adapters.NFS.Port)
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
	if cfg.Content.Type == "" {
		t.Error("Default config missing content type")
	}
	if cfg.Metadata.Type == "" {
		t.Error("Default config missing metadata type")
	}
	if len(cfg.Shares) == 0 {
		t.Error("Default config has no shares")
	}
	if cfg.Shares[0].Name == "" {
		t.Error("Default config share has no name")
	}
}
