package smb

import "testing"

// TestEncryptionConfig_DefaultModeIsPreferred locks in the secure-by-default
// posture: an unset encryption mode resolves to "preferred", which encrypts
// SMB 3.x sessions while still accepting SMB 2.x clients (wire-compatible).
func TestEncryptionConfig_DefaultModeIsPreferred(t *testing.T) {
	var c EncryptionConfig
	c.applyDefaults()
	if c.Mode != "preferred" {
		t.Fatalf("default encryption mode = %q, want %q", c.Mode, "preferred")
	}
	if len(c.AllowedCiphers) == 0 {
		t.Fatalf("default AllowedCiphers must be non-empty")
	}
}

// TestEncryptionConfig_ExplicitModePreserved ensures applyDefaults never
// overrides an admin-set mode (including an explicit "disabled" opt-out).
func TestEncryptionConfig_ExplicitModePreserved(t *testing.T) {
	for _, mode := range []string{"disabled", "preferred", "required"} {
		c := EncryptionConfig{Mode: mode}
		c.applyDefaults()
		if c.Mode != mode {
			t.Errorf("applyDefaults overrode mode %q -> %q", mode, c.Mode)
		}
	}
}
