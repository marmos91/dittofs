package commands

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/adapter/smb"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func TestApplyParsedSMBConfig_ReadsEncryptionMode(t *testing.T) {
	var cfg smb.Config
	if err := applyParsedSMBConfig(&cfg, map[string]any{
		"encryption": map[string]any{"encryption_mode": "required"},
		"signing":    map[string]any{"required": true},
	}); err != nil {
		t.Fatal(err)
	}
	if cfg.Encryption.Mode != "required" {
		t.Fatalf("encryption mode = %q, want required", cfg.Encryption.Mode)
	}
	if !cfg.Signing.Required {
		t.Fatal("signing required was dropped")
	}
}

func TestApplyParsedSMBConfig_LeavesModeUnsetWhenAbsent(t *testing.T) {
	var cfg smb.Config
	if err := applyParsedSMBConfig(&cfg, map[string]any{
		"bind_address": "192.168.64.254",
	}); err != nil {
		t.Fatal(err)
	}
	if cfg.Encryption.Mode != "" {
		t.Fatalf("encryption mode = %q, want empty so the default can apply", cfg.Encryption.Mode)
	}
	if cfg.BindAddress != "192.168.64.254" {
		t.Fatalf("bind address = %q", cfg.BindAddress)
	}
}

func TestCreateSMBAdapter_RejectsUnknownEncryptionMode(t *testing.T) {
	cfg := &models.AdapterConfig{
		Type: "smb",
		ParsedConfig: map[string]any{
			"encryption": map[string]any{"encryption_mode": "require"},
		},
	}
	adapter, err := createSMBAdapter(cfg, nil, nil)
	if err == nil {
		t.Fatal("expected invalid encryption_mode to be rejected")
	}
	if adapter != nil {
		t.Fatalf("adapter = %v, want nil", adapter)
	}
}
