package config

import (
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/adapter/nfs"
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

func TestValidate_InvalidContentType(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Content.Type = "s3"

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for unimplemented content type")
	}
}

func TestValidate_InvalidMetadataType(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Metadata.Type = "postgres"

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for unimplemented metadata type")
	}
}

func TestValidate_NoShares(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Shares = []ShareConfig{}

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for no shares")
	}
	if !strings.Contains(err.Error(), "at least one share") {
		t.Errorf("Expected 'at least one share' error, got: %v", err)
	}
}

func TestValidate_DuplicateShareNames(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Shares = append(cfg.Shares, cfg.Shares[0]) // Duplicate share

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for duplicate share names")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("Expected 'duplicate' error, got: %v", err)
	}
}

func TestValidate_ShareNameMustStartWithSlash(t *testing.T) {
	cfg := &Config{
		Logging: LoggingConfig{
			Level:  "INFO",
			Format: "text",
			Output: "stdout",
		},
		Server: ServerConfig{
			ShutdownTimeout: 30,
		},
		Content: ContentConfig{
			Type: "filesystem",
		},
		Metadata: MetadataConfig{
			Type: "memory",
		},
		Shares: []ShareConfig{
			{
				Name:               "export", // Missing leading slash
				AllowedAuthMethods: []string{"anonymous"},
				IdentityMapping: IdentityMappingConfig{
					AnonymousUID: 65534,
					AnonymousGID: 65534,
				},
				RootAttr: RootAttrConfig{
					Mode: 0755,
				},
			},
		},
		Adapters: AdaptersConfig{
			NFS: nfs.NFSConfig{
				Enabled:         true,
				Port:            2049,
				ShutdownTimeout: 30,
			},
		},
	}

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for share name without leading slash")
	}
	t.Logf("Got error: %v", err)
	// The error might mention shares[0] or similar
	errStr := err.Error()
	if !strings.Contains(errStr, "startswith") && !strings.Contains(errStr, "required") {
		t.Errorf("Expected 'startswith' or 'required' validation error, got: %v", err)
	}
}

func TestValidate_InvalidAuthMethod(t *testing.T) {
	cfg := &Config{
		Logging: LoggingConfig{
			Level:  "INFO",
			Format: "text",
			Output: "stdout",
		},
		Server: ServerConfig{
			ShutdownTimeout: 30,
		},
		Content: ContentConfig{
			Type: "filesystem",
		},
		Metadata: MetadataConfig{
			Type: "memory",
		},
		Shares: []ShareConfig{
			{
				Name:               "/export",
				AllowedAuthMethods: []string{"kerberos"}, // Invalid
				IdentityMapping: IdentityMappingConfig{
					AnonymousUID: 65534,
					AnonymousGID: 65534,
				},
				RootAttr: RootAttrConfig{
					Mode: 0755,
				},
			},
		},
		Adapters: AdaptersConfig{
			NFS: nfs.NFSConfig{
				Enabled:         true,
				Port:            2049,
				ShutdownTimeout: 30,
			},
		},
	}

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for invalid auth method")
	}
	// Just check we got an error, the exact validation tag doesn't matter
}

func TestValidate_InvalidNFSPort(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Adapters.NFS.Port = 70000 // Out of range

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
	cfg.Adapters.NFS.Port = -1

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for negative port")
	}
}

func TestValidate_NegativeMaxConnections(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Adapters.NFS.MaxConnections = -1

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for negative max_connections")
	}
}

func TestValidate_InvalidShutdownTimeout(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Server.ShutdownTimeout = 0

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for zero shutdown timeout")
	}
	// Either 'required' or 'gt' is acceptable
	if !strings.Contains(err.Error(), "required") && !strings.Contains(err.Error(), "gt") {
		t.Errorf("Expected 'required' or 'gt' validation error, got: %v", err)
	}
}

func TestValidate_NegativeTimeout(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Adapters.NFS.ReadTimeout = -1 * time.Second

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for negative timeout")
	}
}

func TestValidate_NoAdaptersEnabled(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Adapters.NFS.Enabled = false

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error when no adapters are enabled")
	}
	if !strings.Contains(err.Error(), "at least one adapter") {
		t.Errorf("Expected 'at least one adapter' error, got: %v", err)
	}
}

func TestValidate_DumpRestrictedWithoutClients(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Metadata.DumpRestricted = true
	cfg.Metadata.DumpAllowedClients = []string{}

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for dump_restricted without allowed clients")
	}
	if !strings.Contains(err.Error(), "dump_allowed_clients") {
		t.Errorf("Expected error about dump_allowed_clients, got: %v", err)
	}
}

func TestValidate_InvalidRootMode(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Shares[0].RootAttr.Mode = 0777 + 1 // 512 in decimal, > 511 (0777)

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for invalid mode")
	}
	if !strings.Contains(err.Error(), "lte") {
		t.Errorf("Expected 'lte' (less than or equal) validation error, got: %v", err)
	}
}

func TestValidate_LogLevelNormalization(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Logging.Level = "info" // Lowercase

	err := Validate(cfg)
	if err != nil {
		t.Fatalf("Validation failed: %v", err)
	}

	// Should be normalized to uppercase
	if cfg.Logging.Level != "INFO" {
		t.Errorf("Expected level to be normalized to 'INFO', got %q", cfg.Logging.Level)
	}
}

func TestValidate_MultipleValidShares(t *testing.T) {
	cfg := GetDefaultConfig()
	cfg.Shares = append(cfg.Shares, ShareConfig{
		Name:               "/data",
		ReadOnly:           true,
		Async:              false,
		AllowedAuthMethods: []string{"unix"},
		IdentityMapping: IdentityMappingConfig{
			MapAllToAnonymous: false,
			AnonymousUID:      65534,
			AnonymousGID:      65534,
		},
		RootAttr: RootAttrConfig{
			Mode: 0755,
			UID:  0,
			GID:  0,
		},
	})

	err := Validate(cfg)
	if err != nil {
		t.Errorf("Expected valid config with multiple shares, got error: %v", err)
	}
}

func TestValidate_EmptyShareName(t *testing.T) {
	cfg := &Config{
		Logging: LoggingConfig{
			Level:  "INFO",
			Format: "text",
			Output: "stdout",
		},
		Server: ServerConfig{
			ShutdownTimeout: 30,
		},
		Content: ContentConfig{
			Type: "filesystem",
		},
		Metadata: MetadataConfig{
			Type: "memory",
		},
		Shares: []ShareConfig{
			{
				Name:               "", // Empty name
				AllowedAuthMethods: []string{"anonymous"},
				IdentityMapping: IdentityMappingConfig{
					AnonymousUID: 65534,
					AnonymousGID: 65534,
				},
				RootAttr: RootAttrConfig{
					Mode: 0755,
				},
			},
		},
		Adapters: AdaptersConfig{
			NFS: nfs.NFSConfig{
				Enabled:         true,
				Port:            2049,
				ShutdownTimeout: 30,
			},
		},
	}

	err := Validate(cfg)
	if err == nil {
		t.Fatal("Expected validation error for empty share name")
	}
	// Just check we got an error for the empty share name
}
