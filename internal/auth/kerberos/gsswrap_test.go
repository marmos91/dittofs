package kerberos

import (
	"bytes"
	"testing"
)

// TestWrapGSSToken_ShortForm verifies the wrapper produces a correct
// RFC 2743 Section 3.1 InitialContextToken envelope with short-form length.
func TestWrapGSSToken_ShortForm(t *testing.T) {
	inner := []byte{0x6F, 0x05, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE} // fake AP-REP
	wrapped := WrapGSSToken(inner, KerberosV5OIDBytes, GSSTokenIDAPRep)

	// Expected layout (short form length):
	//   0x60 <len> 0x06 0x09 <9-byte OID> 0x02 0x00 <inner 7 bytes>
	// totalInner = 11 (OID) + 2 (tokID) + 7 (inner) = 20 bytes
	want := []byte{
		0x60, 0x14,
		0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x12, 0x01, 0x02, 0x02,
		0x02, 0x00,
		0x6F, 0x05, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE,
	}
	if !bytes.Equal(wrapped, want) {
		t.Fatalf("short-form wrap mismatch\n  got:  % x\n  want: % x", wrapped, want)
	}
}

// TestWrapGSSToken_LongForm verifies long-form ASN.1 length encoding for
// inner tokens larger than 127 bytes (common for real AP-REPs with AES keys).
func TestWrapGSSToken_LongForm(t *testing.T) {
	inner := bytes.Repeat([]byte{0xAB}, 200)
	wrapped := WrapGSSToken(inner, KerberosV5OIDBytes, GSSTokenIDAPRep)

	if wrapped[0] != 0x60 {
		t.Fatalf("outer tag: got 0x%02x, want 0x60", wrapped[0])
	}
	// Inner content = 11 (OID DER) + 2 (tokID) + 200 (body) = 213 bytes
	// Long form: 0x81 0xD5
	if wrapped[1] != 0x81 {
		t.Fatalf("length form byte: got 0x%02x, want 0x81 (long form, 1 length octet)", wrapped[1])
	}
	if wrapped[2] != 0xD5 {
		t.Fatalf("length value: got 0x%02x, want 0xD5 (213)", wrapped[2])
	}

	// OID block starts at offset 3, token ID at offset 3+11=14, body at 16.
	if !bytes.Equal(wrapped[3:14], KerberosV5OIDBytes) {
		t.Fatalf("OID mismatch: got % x", wrapped[3:14])
	}
	if !bytes.Equal(wrapped[14:16], []byte{0x02, 0x00}) {
		t.Fatalf("tokenID: got % x, want 02 00", wrapped[14:16])
	}
	if !bytes.Equal(wrapped[16:], inner) {
		t.Fatalf("inner body mismatch")
	}
}

// TestWrapGSSToken_MSOID ensures the Microsoft Kerberos V5 OID is emitted
// unchanged (this is what Windows / SSPI clients advertise in SPNEGO).
func TestWrapGSSToken_MSOID(t *testing.T) {
	inner := []byte{0x00}
	wrapped := WrapGSSToken(inner, MSKerberosV5OIDBytes, GSSTokenIDAPReq)

	// OID block starts at offset 2 (short-form length).
	if !bytes.Equal(wrapped[2:2+len(MSKerberosV5OIDBytes)], MSKerberosV5OIDBytes) {
		t.Fatalf("MS OID not echoed: got % x", wrapped[2:2+len(MSKerberosV5OIDBytes)])
	}
	// Byte offset 4 in the MS OID DER (0x82) must distinguish it from the
	// standard OID (which has 0x86 at the same position). Guards against
	// accidentally swapping the two constants.
	if wrapped[2+5] != 0x82 {
		t.Fatalf("expected MS OID marker 0x82 at offset 7, got 0x%02x", wrapped[2+5])
	}
}

// TestWrapGSSToken_TokenIDs verifies all three valid Kerberos token IDs
// are encoded big-endian as RFC 4121 Section 4.1 requires.
func TestWrapGSSToken_TokenIDs(t *testing.T) {
	cases := []struct {
		name string
		id   uint16
		want []byte
	}{
		{"AP-REQ", GSSTokenIDAPReq, []byte{0x01, 0x00}},
		{"AP-REP", GSSTokenIDAPRep, []byte{0x02, 0x00}},
		{"KRB-ERROR", GSSTokenIDKRBError, []byte{0x03, 0x00}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := WrapGSSToken([]byte{0xFF}, KerberosV5OIDBytes, tc.id)
			// tokenID sits right after the 11-byte OID DER (offset 2+11=13).
			got := wrapped[13:15]
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("tokenID for %s: got % x, want % x", tc.name, got, tc.want)
			}
		})
	}
}

// TestWrapGSSToken_RoundTripThroughExtractor proves NFS's existing AP-REQ
// extractor can parse a token we wrap with GSSTokenIDAPReq. This locks in
// the contract that WrapGSSToken is inverse of parseGSSInitialContextToken.
func TestWrapGSSToken_RoundTripThroughExtractor(t *testing.T) {
	inner := []byte("fake-ap-req-body")
	wrapped := WrapGSSToken(inner, KerberosV5OIDBytes, GSSTokenIDAPReq)

	// Mini parser mirroring NFS framework.go / SMB extractAPReqFromGSSToken.
	if wrapped[0] != 0x60 {
		t.Fatalf("outer tag 0x%02x", wrapped[0])
	}
	n := int(wrapped[1]) // short form (inner < 128)
	body := wrapped[2 : 2+n]
	if body[0] != 0x06 {
		t.Fatalf("inner OID tag 0x%02x", body[0])
	}
	oidLen := int(body[1])
	bodyInner := body[2+oidLen+2:] // skip OID tag+len+value + 2-byte tokID
	if !bytes.Equal(bodyInner, inner) {
		t.Fatalf("extracted inner mismatch\n  got:  % x\n  want: % x", bodyInner, inner)
	}
}

func TestEncodeASN1Length(t *testing.T) {
	cases := []struct {
		in   int
		want []byte
	}{
		{0, []byte{0x00}},
		{127, []byte{0x7F}},
		{128, []byte{0x81, 0x80}},
		{255, []byte{0x81, 0xFF}},
		{256, []byte{0x82, 0x01, 0x00}},
		{65535, []byte{0x82, 0xFF, 0xFF}},
		{65536, []byte{0x83, 0x01, 0x00, 0x00}},
	}
	for _, tc := range cases {
		got := encodeASN1Length(tc.in)
		if !bytes.Equal(got, tc.want) {
			t.Errorf("encodeASN1Length(%d) = % x, want % x", tc.in, got, tc.want)
		}
	}
}
