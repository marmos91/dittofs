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
	tests := []struct {
		name     string
		keyLen   int
		wantLen  int
		checkFn  func(t *testing.T, key, normalized []byte)
	}{
		{"AES256_32bytes_Truncated", 32, 16, func(t *testing.T, key, norm []byte) {
			for i := 0; i < 16; i++ {
				if norm[i] != key[i] {
					t.Errorf("[%d] = %d, want %d", i, norm[i], key[i])
				}
			}
		}},
		{"AES128_16bytes_PassThrough", 16, 16, func(t *testing.T, key, norm []byte) {
			for i := 0; i < 16; i++ {
				if norm[i] != key[i] {
					t.Errorf("[%d] = %d, want %d", i, norm[i], key[i])
				}
			}
		}},
		{"DES_8bytes_ZeroPadded", 8, 16, func(t *testing.T, key, norm []byte) {
			for i := 0; i < 8; i++ {
				if norm[i] != key[i] {
					t.Errorf("[%d] = %d, want %d", i, norm[i], key[i])
				}
			}
			for i := 8; i < 16; i++ {
				if norm[i] != 0 {
					t.Errorf("[%d] = %d, want 0", i, norm[i])
				}
			}
		}},
		{"Nil_AllZeros", 0, 16, func(t *testing.T, _, norm []byte) {
			for i := 0; i < 16; i++ {
				if norm[i] != 0 {
					t.Errorf("[%d] = %d, want 0", i, norm[i])
				}
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var key []byte
			if tt.keyLen > 0 {
				key = make([]byte, tt.keyLen)
				for i := range key {
					key[i] = byte(i + 1)
				}
			}

			normalized := normalizeSessionKey(key)
			if len(normalized) != tt.wantLen {
				t.Fatalf("length = %d, want %d", len(normalized), tt.wantLen)
			}
			tt.checkFn(t, key, normalized)
		})
	}
}

// =============================================================================
// deriveSMBPrincipal Tests
// =============================================================================

func TestDeriveSMBPrincipal(t *testing.T) {
	tests := []struct {
		name     string
		base     string
		override string
		want     string
	}{
		{"NFS_to_CIFS", "nfs/server.example.com@EXAMPLE.COM", "", "cifs/server.example.com@EXAMPLE.COM"},
		{"OverrideTakesPrecedence", "nfs/server.example.com@EXAMPLE.COM", "custom/smb@REALM", "custom/smb@REALM"},
		{"NonNFS_PassThrough", "cifs/server.example.com@EXAMPLE.COM", "", "cifs/server.example.com@EXAMPLE.COM"},
		{"EmptyOverride_UsesAutoDerive", "nfs/host@CORP.COM", "", "cifs/host@CORP.COM"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveSMBPrincipal(tt.base, tt.override); got != tt.want {
				t.Errorf("deriveSMBPrincipal(%q, %q) = %q, want %q", tt.base, tt.override, got, tt.want)
			}
		})
	}
}

// =============================================================================
// clientKerberosOID Tests
// =============================================================================

func TestClientKerberosOID(t *testing.T) {
	tests := []struct {
		name   string
		parsed *auth.ParsedToken
		want   asn1.ObjectIdentifier
	}{
		{"BothOIDs_PrefersMS", &auth.ParsedToken{
			Type:      auth.TokenTypeInit,
			MechTypes: []asn1.ObjectIdentifier{auth.OIDMSKerberosV5, auth.OIDKerberosV5},
		}, auth.OIDMSKerberosV5},
		{"StandardOnly_ReturnsStandard", &auth.ParsedToken{
			Type:      auth.TokenTypeInit,
			MechTypes: []asn1.ObjectIdentifier{auth.OIDKerberosV5},
		}, auth.OIDKerberosV5},
		{"NilToken_DefaultsToStandard", nil, auth.OIDKerberosV5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clientKerberosOID(tt.parsed); !got.Equal(tt.want) {
				t.Errorf("clientKerberosOID() = %v, want %v", got, tt.want)
			}
		})
	}
}
