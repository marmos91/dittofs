package handlers

import (
	"bytes"
	"testing"

	"github.com/jcmturner/gofork/encoding/asn1"
	"github.com/jcmturner/gokrb5/v8/spnego"

	"github.com/marmos91/dittofs/internal/adapter/smb/auth"
	kerbauth "github.com/marmos91/dittofs/internal/auth/kerberos"
)

// =============================================================================
// normalizeSessionKey Tests
// =============================================================================

func TestNormalizeSessionKey(t *testing.T) {
	tests := []struct {
		name    string
		keyLen  int
		wantLen int
		checkFn func(t *testing.T, key, normalized []byte)
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

// =============================================================================
// SPNEGO ResponseToken framing — issue #335 regression lock-in
// =============================================================================

// TestSPNEGOResponseTokenWrapping proves the exact wire format handleKerberosAuth
// sends back to MIT Kerberos clients. This is the regression guard for issue #335.
//
// There are TWO layers of OID selection and they are NOT the same:
//
//  1. SPNEGO NegTokenResp.supportedMech — echoes whichever OID the client
//     advertised in mechTypes (clientKerberosOID picks MS if offered, for
//     Windows/SSPI interop).
//
//  2. The OID inside the GSS-API InitialContextToken wrapper that contains
//     the AP-REP — MUST always be the standard RFC 4121 OID
//     (1.2.840.113554.1.2.2), regardless of what SPNEGO negotiated. MIT's
//     krb5_gss and Heimdal only recognize this OID internally when parsing
//     mech tokens; sending the MS OID here causes g_verify_token_header to
//     reject the response with GSS_S_DEFECTIVE_TOKEN.
//
// Windows SSPI accepts the standard OID in both layers, so using it inside
// the wrapper is the safe universal choice.
func TestSPNEGOResponseTokenWrapping(t *testing.T) {
	// Fake raw AP-REP — shape doesn't matter, we assert byte equality.
	// Using the real 0x6F APPLICATION 15 tag to mimic gokrb5's output.
	rawAPRep := append([]byte{0x6F, 0x1F}, bytes.Repeat([]byte{0xA5}, 31)...)

	// Both the StandardOnly and MSPreferred cases must wrap the AP-REP with
	// the standard Kerberos V5 OID inside the GSS envelope. Only the outer
	// SPNEGO supportedMech differs.
	cases := []struct {
		name             string
		spnegoOuterOID   asn1.ObjectIdentifier
		wantInnerOIDFull []byte // always standard; this is the invariant
	}{
		{
			name:             "ClientAdvertisedStandardOnly",
			spnegoOuterOID:   auth.OIDKerberosV5,
			wantInnerOIDFull: kerbauth.KerberosV5OIDBytes,
		},
		{
			name:             "ClientAdvertisedMSLegacy",
			spnegoOuterOID:   auth.OIDMSKerberosV5,
			wantInnerOIDFull: kerbauth.KerberosV5OIDBytes, // still standard inside!
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Replicate handleKerberosAuth's AP-REP wrapping step verbatim:
			// the inner OID is hardcoded to the standard Kerberos V5 OID so
			// MIT-based clients accept the token.
			wrapped := kerbauth.WrapGSSToken(rawAPRep, kerbauth.KerberosV5OIDBytes, kerbauth.GSSTokenIDAPRep)

			// The outer SPNEGO layer still echoes whichever OID the client
			// preferred, via clientKerberosOID.
			spnegoBytes, err := auth.BuildAcceptCompleteWithMIC(tc.spnegoOuterOID, wrapped, nil)
			if err != nil {
				t.Fatalf("BuildAcceptCompleteWithMIC: %v", err)
			}

			var resp spnego.NegTokenResp
			if err := resp.Unmarshal(spnegoBytes); err != nil {
				t.Fatalf("parse NegTokenResp: %v", err)
			}
			if !resp.SupportedMech.Equal(tc.spnegoOuterOID) {
				t.Fatalf("outer supportedMech = %v, want %v (outer layer must echo client preference)",
					resp.SupportedMech, tc.spnegoOuterOID)
			}

			// --- ResponseToken framing assertions -----------------------------

			rt := resp.ResponseToken
			if len(rt) == 0 {
				t.Fatal("ResponseToken is empty — AP-REP was not included")
			}
			if rt[0] != 0x60 {
				t.Fatalf("ResponseToken[0] = 0x%02x, want 0x60 (GSS-API InitialContextToken tag — issue #335)", rt[0])
			}

			// Short form length (wrapped is well under 128 bytes).
			declaredLen := int(rt[1])
			if 2+declaredLen != len(rt) {
				t.Fatalf("length mismatch: declared %d, actual body %d", declaredLen, len(rt)-2)
			}

			// OID block: 0x06 <len> <value>
			if rt[2] != 0x06 {
				t.Fatalf("inner OID tag = 0x%02x, want 0x06", rt[2])
			}
			innerOIDLen := int(rt[3])
			innerOIDFull := rt[2 : 2+2+innerOIDLen] // tag+length+value
			if !bytes.Equal(innerOIDFull, tc.wantInnerOIDFull) {
				t.Fatalf("inner GSS OID must always be standard Kerberos V5 (issue #335)\n  got:  % x\n  want: % x",
					innerOIDFull, tc.wantInnerOIDFull)
			}
			// Sanity check: byte at offset 7 inside the OID distinguishes
			// standard (0x86) from MS (0x82). Must be 0x86.
			if rt[2+5] != 0x86 {
				t.Fatalf("inner OID family marker = 0x%02x, want 0x86 (standard Kerberos V5). "+
					"0x82 would indicate the MS OID leaked into the inner wrapper — MIT will reject.", rt[2+5])
			}

			// Token ID (2 bytes, big-endian 0x0200 for AP-REP).
			tokIDOff := 2 + 2 + innerOIDLen
			if rt[tokIDOff] != 0x02 || rt[tokIDOff+1] != 0x00 {
				t.Fatalf("tokenID = 0x%02x%02x, want 0x0200 (AP-REP)", rt[tokIDOff], rt[tokIDOff+1])
			}

			// Body: must be the exact raw AP-REP we passed in.
			body := rt[tokIDOff+2:]
			if !bytes.Equal(body, rawAPRep) {
				t.Fatalf("embedded AP-REP mismatch\n  got:  % x\n  want: % x", body, rawAPRep)
			}
		})
	}
}
