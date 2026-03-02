package kerberos

import (
	"testing"
)

// =============================================================================
// ResolvePrincipal Tests
// =============================================================================

func TestResolvePrincipal(t *testing.T) {
	t.Run("StripRealmByDefault", func(t *testing.T) {
		cfg := &IdentityConfig{StripRealm: true}
		username := ResolvePrincipal("alice", "EXAMPLE.COM", cfg)
		if username != "alice" {
			t.Errorf("ResolvePrincipal = %q, want %q", username, "alice")
		}
	})

	t.Run("StripRealmDefault_NilConfig", func(t *testing.T) {
		username := ResolvePrincipal("alice", "EXAMPLE.COM", nil)
		if username != "alice" {
			t.Errorf("ResolvePrincipal = %q, want %q", username, "alice")
		}
	})

	t.Run("ExplicitMappingTakesPrecedence", func(t *testing.T) {
		cfg := &IdentityConfig{
			StripRealm: true,
			ExplicitMappings: map[string]string{
				"admin@CORP.COM": "superadmin",
			},
		}
		username := ResolvePrincipal("admin", "CORP.COM", cfg)
		if username != "superadmin" {
			t.Errorf("ResolvePrincipal = %q, want %q", username, "superadmin")
		}
	})

	t.Run("ExplicitMappingCaseInsensitiveRealm", func(t *testing.T) {
		// Mapping key uses uppercase realm; principal comes in as-is
		cfg := &IdentityConfig{
			StripRealm: true,
			ExplicitMappings: map[string]string{
				"alice@EXAMPLE.COM": "alice-mapped",
			},
		}
		username := ResolvePrincipal("alice", "EXAMPLE.COM", cfg)
		if username != "alice-mapped" {
			t.Errorf("ResolvePrincipal = %q, want %q", username, "alice-mapped")
		}
	})

	t.Run("FallbackToStripRealmWhenNoMapping", func(t *testing.T) {
		cfg := &IdentityConfig{
			StripRealm: true,
			ExplicitMappings: map[string]string{
				"admin@CORP.COM": "superadmin",
			},
		}
		// "bob@CORP.COM" is not in the explicit mapping
		username := ResolvePrincipal("bob", "CORP.COM", cfg)
		if username != "bob" {
			t.Errorf("ResolvePrincipal = %q, want %q", username, "bob")
		}
	})

	t.Run("ServicePrincipalStripsPrefix", func(t *testing.T) {
		cfg := &IdentityConfig{StripRealm: true}
		// Service principal like "svc/host" -> "svc"
		username := ResolvePrincipal("svc/host.example.com", "EXAMPLE.COM", cfg)
		if username != "svc" {
			t.Errorf("ResolvePrincipal = %q, want %q", username, "svc")
		}
	})

	t.Run("ServicePrincipalExplicitMapping", func(t *testing.T) {
		cfg := &IdentityConfig{
			StripRealm: true,
			ExplicitMappings: map[string]string{
				"svc/host@CORP.COM": "service-account",
			},
		}
		username := ResolvePrincipal("svc/host", "CORP.COM", cfg)
		if username != "service-account" {
			t.Errorf("ResolvePrincipal = %q, want %q", username, "service-account")
		}
	})

	t.Run("StripRealmDisabled", func(t *testing.T) {
		cfg := &IdentityConfig{StripRealm: false}
		username := ResolvePrincipal("alice", "EXAMPLE.COM", cfg)
		// When StripRealm is false and no explicit mapping, return principal@realm
		if username != "alice@EXAMPLE.COM" {
			t.Errorf("ResolvePrincipal = %q, want %q", username, "alice@EXAMPLE.COM")
		}
	})

	t.Run("EmptyRealm", func(t *testing.T) {
		cfg := &IdentityConfig{StripRealm: true}
		username := ResolvePrincipal("alice", "", cfg)
		if username != "alice" {
			t.Errorf("ResolvePrincipal = %q, want %q", username, "alice")
		}
	})

	t.Run("EmptyPrincipal", func(t *testing.T) {
		cfg := &IdentityConfig{StripRealm: true}
		username := ResolvePrincipal("", "EXAMPLE.COM", cfg)
		if username != "" {
			t.Errorf("ResolvePrincipal = %q, want %q", username, "")
		}
	})
}

// =============================================================================
// DefaultIdentityConfig Tests
// =============================================================================

func TestDefaultIdentityConfig(t *testing.T) {
	cfg := DefaultIdentityConfig()
	if cfg == nil {
		t.Fatal("DefaultIdentityConfig returned nil")
	}
	if !cfg.StripRealm {
		t.Error("Default StripRealm should be true")
	}
	if cfg.ExplicitMappings != nil {
		t.Error("Default ExplicitMappings should be nil")
	}
}
