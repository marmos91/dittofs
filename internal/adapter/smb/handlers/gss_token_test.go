package handlers

import (
	"bytes"
	"testing"
)

// krb5OID is the KRB5 mechanism OID in DER form (1.2.840.113554.1.2.2).
// Used in GSS-API initial context token framing (RFC 2743 Section 3.1).
var krb5OID = []byte{0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x12, 0x01, 0x02, 0x02}

// wrapInInitialContextToken wraps a raw AP-REQ in the GSS-API initial context
// token format ([APPLICATION 0] IMPLICIT SEQUENCE { OID, 0x01 0x00, AP-REQ }).
// This is the format that SPNEGO's MechToken field uses for the initial
// Kerberos token and what extractAPReqFromGSSToken is supposed to unwrap.
func wrapInInitialContextToken(apReq []byte) []byte {
	inner := make([]byte, 0, len(krb5OID)+2+len(apReq))
	inner = append(inner, krb5OID...)
	inner = append(inner, 0x01, 0x00) // AP-REQ Tok-ID
	inner = append(inner, apReq...)

	// [APPLICATION 0] tag + ASN.1 DER length
	out := []byte{0x60}
	switch {
	case len(inner) < 0x80:
		out = append(out, byte(len(inner)))
	case len(inner) <= 0xff:
		out = append(out, 0x81, byte(len(inner)))
	case len(inner) <= 0xffff:
		out = append(out, 0x82, byte(len(inner)>>8), byte(len(inner)))
	default:
		out = append(out, 0x83, byte(len(inner)>>16), byte(len(inner)>>8), byte(len(inner)))
	}
	return append(out, inner...)
}

func TestExtractAPReqFromGSSToken_Wrapped(t *testing.T) {
	apReq := []byte{0x6e, 0x82, 0x01, 0x23} // bogus but distinguishable prefix
	for i := 0; i < 200; i++ {
		apReq = append(apReq, byte(i))
	}

	token := wrapInInitialContextToken(apReq)
	got, err := extractAPReqFromGSSToken(token)
	if err != nil {
		t.Fatalf("extractAPReqFromGSSToken failed: %v", err)
	}
	if !bytes.Equal(got, apReq) {
		t.Fatalf("extractAPReqFromGSSToken returned %x, want %x", got, apReq)
	}
}

func TestExtractAPReqFromGSSToken_Raw(t *testing.T) {
	// A token that does not begin with 0x60 must be treated as already-raw
	// AP-REQ and returned unchanged (defensive behavior).
	raw := []byte{0x6e, 0x82, 0x01, 0x23, 0xaa, 0xbb, 0xcc}
	got, err := extractAPReqFromGSSToken(raw)
	if err != nil {
		t.Fatalf("extractAPReqFromGSSToken failed: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("expected raw passthrough, got %x", got)
	}
}

func TestExtractAPReqFromGSSToken_ShortLongFormLength(t *testing.T) {
	// Short inner so we exercise the single-byte length branch.
	apReq := []byte{0x01, 0x02, 0x03, 0x04}
	token := wrapInInitialContextToken(apReq)
	got, err := extractAPReqFromGSSToken(token)
	if err != nil {
		t.Fatalf("extractAPReqFromGSSToken failed: %v", err)
	}
	if !bytes.Equal(got, apReq) {
		t.Fatalf("short length: got %x, want %x", got, apReq)
	}

	// Long inner (>255 bytes) exercises the 0x82 two-byte length branch.
	big := make([]byte, 300)
	for i := range big {
		big[i] = byte(i)
	}
	token = wrapInInitialContextToken(big)
	got, err = extractAPReqFromGSSToken(token)
	if err != nil {
		t.Fatalf("extractAPReqFromGSSToken failed: %v", err)
	}
	if !bytes.Equal(got, big) {
		t.Fatalf("long length: got %x, want %x", got, big)
	}
}

func TestExtractAPReqFromGSSToken_Errors(t *testing.T) {
	tests := []struct {
		name  string
		token []byte
	}{
		{"empty", []byte{}},
		{"single byte", []byte{0x60}},
		{"truncated length", []byte{0x60, 0x82}},                                                     // claims 2-byte length, no bytes follow
		{"declared length exceeds buffer", []byte{0x60, 0x10, 0x06, 0x09}},                           // declares 16 bytes but only has 2
		{"missing OID tag", []byte{0x60, 0x02, 0xff, 0x00}},                                          // byte after length must be 0x06
		{"wrong tok-id", append([]byte{0x60, 0x0d, 0x06, 0x09}, append(krb5OID[2:], 0x02, 0x00)...)}, // Tok-ID 0x0200 (AP-REP) not 0x0100
		{"long-form OID length rejected", []byte{0x60, 0x05, 0x06, 0x81, 0x09, 0x01, 0x00}},          // OID length byte 0x81 (BER long-form) must be rejected
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := extractAPReqFromGSSToken(tt.token); err == nil {
				t.Errorf("expected error for %s, got nil", tt.name)
			}
		})
	}
}

func TestParseGSSASN1Length(t *testing.T) {
	tests := []struct {
		name    string
		buf     []byte
		want    uint32
		wantLen int
		wantErr bool
	}{
		{"short form 0", []byte{0x00}, 0, 1, false},
		{"short form 127", []byte{0x7f}, 127, 1, false},
		{"long form 1 byte", []byte{0x81, 0xff}, 255, 2, false},
		{"long form 2 bytes", []byte{0x82, 0x01, 0x00}, 256, 3, false},
		{"long form 3 bytes", []byte{0x83, 0x01, 0x00, 0x00}, 65536, 4, false},
		{"long form 4 bytes", []byte{0x84, 0x01, 0x00, 0x00, 0x00}, 16777216, 5, false},
		{"empty buf", []byte{}, 0, 0, true},
		{"truncated long form", []byte{0x82, 0x01}, 0, 0, true},
		{"unsupported length encoding", []byte{0x85, 0x01, 0x02, 0x03, 0x04, 0x05}, 0, 0, true},
		{"zero-length long form", []byte{0x80}, 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			length, n, err := parseGSSASN1Length(tt.buf)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if length != tt.want {
				t.Errorf("length=%d want=%d", length, tt.want)
			}
			if n != tt.wantLen {
				t.Errorf("consumed=%d want=%d", n, tt.wantLen)
			}
		})
	}
}
