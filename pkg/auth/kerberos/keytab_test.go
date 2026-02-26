package kerberos

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/keytab"
)

// ============================================================================
// resolveKeytabPath tests
// ============================================================================

func TestResolveKeytabPath_EnvVarOverride(t *testing.T) {
	t.Setenv("DITTOFS_KERBEROS_KEYTAB", "/env/override/keytab")

	result := resolveKeytabPath("/config/path/keytab")
	if result != "/env/override/keytab" {
		t.Fatalf("expected /env/override/keytab, got %s", result)
	}
}

func TestResolveKeytabPath_FallbackToConfig(t *testing.T) {
	// Ensure env var is not set
	t.Setenv("DITTOFS_KERBEROS_KEYTAB", "")

	result := resolveKeytabPath("/config/path/keytab")
	if result != "/config/path/keytab" {
		t.Fatalf("expected /config/path/keytab, got %s", result)
	}
}

func TestResolveKeytabPath_EmptyBoth(t *testing.T) {
	t.Setenv("DITTOFS_KERBEROS_KEYTAB", "")

	result := resolveKeytabPath("")
	if result != "" {
		t.Fatalf("expected empty string, got %s", result)
	}
}

// ============================================================================
// resolveServicePrincipal tests
// ============================================================================

func TestResolveServicePrincipal_EnvVarOverride(t *testing.T) {
	t.Setenv("DITTOFS_KERBEROS_PRINCIPAL", "nfs/env.example.com@EXAMPLE.COM")

	result := resolveServicePrincipal("nfs/config.example.com@EXAMPLE.COM")
	if result != "nfs/env.example.com@EXAMPLE.COM" {
		t.Fatalf("expected nfs/env.example.com@EXAMPLE.COM, got %s", result)
	}
}

func TestResolveServicePrincipal_FallbackToConfig(t *testing.T) {
	t.Setenv("DITTOFS_KERBEROS_PRINCIPAL", "")

	result := resolveServicePrincipal("nfs/config.example.com@EXAMPLE.COM")
	if result != "nfs/config.example.com@EXAMPLE.COM" {
		t.Fatalf("expected nfs/config.example.com@EXAMPLE.COM, got %s", result)
	}
}

// ============================================================================
// resolveKrb5ConfPath tests
// ============================================================================

func TestResolveKrb5ConfPath_EnvVarOverride(t *testing.T) {
	t.Setenv("DITTOFS_KERBEROS_KRB5CONF", "/env/override/krb5.conf")

	result := resolveKrb5ConfPath("/config/path/krb5.conf")
	if result != "/env/override/krb5.conf" {
		t.Fatalf("expected /env/override/krb5.conf, got %s", result)
	}
}

func TestResolveKrb5ConfPath_FallbackToConfig(t *testing.T) {
	t.Setenv("DITTOFS_KERBEROS_KRB5CONF", "")

	result := resolveKrb5ConfPath("/config/path/krb5.conf")
	if result != "/config/path/krb5.conf" {
		t.Fatalf("expected /config/path/krb5.conf, got %s", result)
	}
}

func TestResolveKrb5ConfPath_DefaultFallback(t *testing.T) {
	t.Setenv("DITTOFS_KERBEROS_KRB5CONF", "")

	result := resolveKrb5ConfPath("")
	if result != "/etc/krb5.conf" {
		t.Fatalf("expected /etc/krb5.conf, got %s", result)
	}
}

// ============================================================================
// loadKeytab tests
// ============================================================================

// createTestKeytab creates a minimal valid keytab file for testing with KVNO 1.
func createTestKeytab(t *testing.T, dir string) string {
	t.Helper()
	return createTestKeytabWithKVNO(t, dir, 1)
}

// createTestKeytabWithKVNO creates a keytab file with a specific KVNO for testing.
func createTestKeytabWithKVNO(t *testing.T, dir string, kvno uint8) string {
	t.Helper()

	kt := keytab.New()
	err := kt.AddEntry("nfs/server.example.com", "EXAMPLE.COM", "test-password", time.Now(), kvno, 17)
	if err != nil {
		t.Fatalf("add keytab entry: %v", err)
	}

	data, err := kt.Marshal()
	if err != nil {
		t.Fatalf("marshal test keytab: %v", err)
	}

	path := filepath.Join(dir, "test.keytab")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write test keytab: %v", err)
	}

	return path
}

func TestLoadKeytab_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := createTestKeytab(t, dir)

	kt, err := loadKeytab(path)
	if err != nil {
		t.Fatalf("loadKeytab failed: %v", err)
	}
	if kt == nil {
		t.Fatal("expected non-nil keytab")
	}
}

func TestLoadKeytab_NonexistentFile(t *testing.T) {
	_, err := loadKeytab("/nonexistent/path/keytab")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestLoadKeytab_InvalidData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.keytab")
	if err := os.WriteFile(path, []byte("not a keytab"), 0600); err != nil {
		t.Fatalf("write bad keytab: %v", err)
	}

	_, err := loadKeytab(path)
	if err == nil {
		t.Fatal("expected error for invalid keytab data")
	}
}

// ============================================================================
// ReloadKeytab tests
// ============================================================================

func TestReloadKeytab_AtomicSwap(t *testing.T) {
	dir := t.TempDir()
	path := createTestKeytabWithKVNO(t, dir, 1)

	// Create a provider with the initial keytab
	p := &Provider{
		keytabPath: path,
	}

	// Load initial keytab
	kt, err := loadKeytab(path)
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	p.keytab = kt

	oldKeytab := p.Keytab()

	// Create an updated keytab with KVNO 2
	kt2 := keytab.New()
	_ = kt2.AddEntry("nfs/updated.example.com", "EXAMPLE.COM", "updated-password", time.Now(), 2, 17)

	data, err := kt2.Marshal()
	if err != nil {
		t.Fatalf("marshal updated keytab: %v", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write updated keytab: %v", err)
	}

	// Reload
	if err := p.ReloadKeytab(); err != nil {
		t.Fatalf("ReloadKeytab failed: %v", err)
	}

	// Verify the keytab was swapped (pointers should be different)
	newKeytab := p.Keytab()
	if oldKeytab == newKeytab {
		t.Fatal("expected keytab to be swapped to a new instance")
	}
}

func TestReloadKeytab_KeepsOldOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := createTestKeytab(t, dir)

	// Create a provider with the initial keytab
	p := &Provider{
		keytabPath: path,
	}

	kt, err := loadKeytab(path)
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	p.keytab = kt

	oldKeytab := p.Keytab()

	// Overwrite with invalid data
	if err := os.WriteFile(path, []byte("invalid keytab data"), 0600); err != nil {
		t.Fatalf("write invalid keytab: %v", err)
	}

	// Reload should fail
	err = p.ReloadKeytab()
	if err == nil {
		t.Fatal("expected error for invalid keytab data during reload")
	}

	// Old keytab should still be active (same pointer)
	currentKeytab := p.Keytab()
	if currentKeytab != oldKeytab {
		t.Fatal("expected old keytab to be preserved after failed reload")
	}
}

// ============================================================================
// KeytabManager tests
// ============================================================================

func TestKeytabManager_StartStop(t *testing.T) {
	dir := t.TempDir()
	path := createTestKeytab(t, dir)

	p := &Provider{keytabPath: path}
	kt, _ := loadKeytab(path)
	p.keytab = kt

	km := NewKeytabManager(path, p)
	if err := km.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Stop should not panic or block
	km.Stop()

	// Double stop should be safe
	km.Stop()
}

func TestKeytabManager_StartFailsForMissingFile(t *testing.T) {
	p := &Provider{keytabPath: "/nonexistent"}

	km := NewKeytabManager("/nonexistent", p)
	err := km.Start()
	if err == nil {
		t.Fatal("expected error for nonexistent keytab file")
	}
}
