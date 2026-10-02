package commands

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/adapter/smb"
)

func TestApplyParsedSMBConfig_ReadsEncryptionMode(t *testing.T) {
	var cfg smb.Config
	applyParsedSMBConfig(&cfg, map[string]any{
		"encryption": map[string]any{"encryption_mode": "required"},
		"signing":    map[string]any{"required": true},
	})
	if cfg.Encryption.Mode != "required" {
		t.Fatalf("encryption mode = %q, want required", cfg.Encryption.Mode)
	}
	if !cfg.Signing.Required {
		t.Fatal("signing required was dropped")
	}
}

func TestApplyParsedSMBConfig_LeavesModeUnsetWhenAbsent(t *testing.T) {
	var cfg smb.Config
	applyParsedSMBConfig(&cfg, map[string]any{
		"bind_address": "192.168.64.254",
	})
	if cfg.Encryption.Mode != "" {
		t.Fatalf("encryption mode = %q, want empty so the default can apply", cfg.Encryption.Mode)
	}
	if cfg.BindAddress != "192.168.64.254" {
		t.Fatalf("bind address = %q", cfg.BindAddress)
	}
}
