package handlers

import (
	"testing"

	"github.com/jcmturner/gofork/encoding/asn1"
	"github.com/marmos91/dittofs/internal/adapter/smb/auth"
)

// =============================================================================
// normalizeSessionKey Tests
// =============================================================================

func TestNormalizeSessionKey(t *testing.T) {
	t.Run("AES256Key_32bytes_TruncatedTo16", func(t *testing.T) {
		key := make([]byte, 32)
		for i := range key {
			key[i] = byte(i + 1)
		}

		normalized := normalizeSessionKey(key)
		if len(normalized) != 16 {
			t.Fatalf("normalizeSessionKey length = %d, want 16", len(normalized))
		}

		// Should contain first 16 bytes
		for i := 0; i < 16; i++ {
			if normalized[i] != byte(i+1) {
				t.Errorf("normalizeSessionKey[%d] = %d, want %d", i, normalized[i], i+1)
			}
		}
	})

	t.Run("AES128Key_16bytes_PassThrough", func(t *testing.T) {
		key := make([]byte, 16)
		for i := range key {
			key[i] = byte(i + 0x10)
		}

		normalized := normalizeSessionKey(key)
		if len(normalized) != 16 {
			t.Fatalf("normalizeSessionKey length = %d, want 16", len(normalized))
		}

		for i := 0; i < 16; i++ {
			if normalized[i] != byte(i+0x10) {
				t.Errorf("normalizeSessionKey[%d] = %d, want %d", i, normalized[i], i+0x10)
			}
		}
	})

	t.Run("DESKey_8bytes_ZeroPadded", func(t *testing.T) {
		key := make([]byte, 8)
		for i := range key {
			key[i] = byte(i + 0x20)
		}

		normalized := normalizeSessionKey(key)
		if len(normalized) != 16 {
			t.Fatalf("normalizeSessionKey length = %d, want 16", len(normalized))
		}

		// First 8 bytes should match
		for i := 0; i < 8; i++ {
			if normalized[i] != byte(i+0x20) {
				t.Errorf("normalizeSessionKey[%d] = %d, want %d", i, normalized[i], i+0x20)
			}
		}

		// Remaining 8 bytes should be zero-padded
		for i := 8; i < 16; i++ {
			if normalized[i] != 0 {
				t.Errorf("normalizeSessionKey[%d] = %d, want 0 (zero-pad)", i, normalized[i])
			}
		}
	})

	t.Run("EmptyKey_ZeroPadded", func(t *testing.T) {
		normalized := normalizeSessionKey(nil)
		if len(normalized) != 16 {
			t.Fatalf("normalizeSessionKey length = %d, want 16", len(normalized))
		}

		for i := 0; i < 16; i++ {
			if normalized[i] != 0 {
				t.Errorf("normalizeSessionKey[%d] = %d, want 0", i, normalized[i])
			}
		}
	})
}

// =============================================================================
// deriveSMBPrincipal Tests
// =============================================================================

func TestDeriveSMBPrincipal(t *testing.T) {
	t.Run("NFS_to_CIFS", func(t *testing.T) {
		spn := deriveSMBPrincipal("nfs/server.example.com@EXAMPLE.COM", "")
		if spn != "cifs/server.example.com@EXAMPLE.COM" {
			t.Errorf("deriveSMBPrincipal = %q, want %q", spn, "cifs/server.example.com@EXAMPLE.COM")
		}
	})

	t.Run("OverrideTakesPrecedence", func(t *testing.T) {
		spn := deriveSMBPrincipal("nfs/server.example.com@EXAMPLE.COM", "custom/smb@REALM")
		if spn != "custom/smb@REALM" {
			t.Errorf("deriveSMBPrincipal = %q, want %q", spn, "custom/smb@REALM")
		}
	})

	t.Run("NonNFS_PassThrough", func(t *testing.T) {
		spn := deriveSMBPrincipal("cifs/server.example.com@EXAMPLE.COM", "")
		if spn != "cifs/server.example.com@EXAMPLE.COM" {
			t.Errorf("deriveSMBPrincipal = %q, want %q", spn, "cifs/server.example.com@EXAMPLE.COM")
		}
	})

	t.Run("EmptyOverride_UsesAutoDerive", func(t *testing.T) {
		spn := deriveSMBPrincipal("nfs/host@CORP.COM", "")
		if spn != "cifs/host@CORP.COM" {
			t.Errorf("deriveSMBPrincipal = %q, want %q", spn, "cifs/host@CORP.COM")
		}
	})
}

// =============================================================================
// clientKerberosOID Tests
// =============================================================================

func TestClientKerberosOID(t *testing.T) {
	t.Run("MSKerberosOIDUsed_ReturnsMSKerberosOID", func(t *testing.T) {
		parsed := &auth.ParsedToken{
			Type:      auth.TokenTypeInit,
			MechTypes: []asn1.ObjectIdentifier{auth.OIDMSKerberosV5, auth.OIDKerberosV5},
		}

		oid := clientKerberosOID(parsed)
		if !oid.Equal(auth.OIDMSKerberosV5) {
			t.Errorf("clientKerberosOID = %v, want OIDMSKerberosV5", oid)
		}
	})

	t.Run("StandardKerberosOIDUsed_ReturnsStandardOID", func(t *testing.T) {
		parsed := &auth.ParsedToken{
			Type:      auth.TokenTypeInit,
			MechTypes: []asn1.ObjectIdentifier{auth.OIDKerberosV5},
		}

		oid := clientKerberosOID(parsed)
		if !oid.Equal(auth.OIDKerberosV5) {
			t.Errorf("clientKerberosOID = %v, want OIDKerberosV5", oid)
		}
	})

	t.Run("BothOIDs_PrefersMS", func(t *testing.T) {
		// When both are present, MS OID takes precedence (Windows clients list it first)
		parsed := &auth.ParsedToken{
			Type:      auth.TokenTypeInit,
			MechTypes: []asn1.ObjectIdentifier{auth.OIDMSKerberosV5, auth.OIDKerberosV5},
		}

		oid := clientKerberosOID(parsed)
		if !oid.Equal(auth.OIDMSKerberosV5) {
			t.Errorf("clientKerberosOID = %v, want OIDMSKerberosV5 (preferred for Windows)", oid)
		}
	})

	t.Run("NilToken_DefaultsToStandard", func(t *testing.T) {
		oid := clientKerberosOID(nil)
		if !oid.Equal(auth.OIDKerberosV5) {
			t.Errorf("clientKerberosOID = %v, want OIDKerberosV5 (default)", oid)
		}
	})
}
