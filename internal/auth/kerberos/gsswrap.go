package kerberos

// GSS-API initial context token framing helpers.
//
// Both the NFS RPCSEC_GSS path and the SMB SPNEGO path need to hand MIT
// Kerberos clients a Kerberos protocol message wrapped in the RFC 2743
// Section 3.1 InitialContextToken envelope:
//
//	0x60 [ASN.1 length] [OID tag] [OID length] [OID bytes] [tokenID] [inner]
//
// MIT's krb5_gss_init_sec_context (and gss_accept_sec_context) calls
// g_verify_token_header on the received context token, which requires this
// wrapper for both initial and subsequent context tokens — despite RFC 4121
// Section 4.1 suggesting subsequent tokens may skip the [APPLICATION 0]
// wrapper. Empirically, MIT clients reject bare "tokenID || message" bytes
// with GSS_S_DEFECTIVE_TOKEN (see issue #335), so every GSS-API context
// token we emit goes through this wrapper.

// Token identifiers for Kerberos context establishment tokens
// (RFC 4121 Section 4.1, expressed in big-endian order).
const (
	// GSSTokenIDAPReq is the 2-byte token identifier for KRB_AP_REQ.
	GSSTokenIDAPReq uint16 = 0x0100

	// GSSTokenIDAPRep is the 2-byte token identifier for KRB_AP_REP.
	GSSTokenIDAPRep uint16 = 0x0200

	// GSSTokenIDKRBError is the 2-byte token identifier for KRB_ERROR.
	GSSTokenIDKRBError uint16 = 0x0300
)

// Pre-encoded DER OID bytes (tag 0x06 + length + value) for the Kerberos V5
// mechanism OIDs. Callers pick the one that matches the mechanism negotiated
// at the outer layer (SPNEGO supportedMech, RPCSEC_GSS mechanism).
var (
	// KerberosV5OIDBytes is the DER-encoded OID 1.2.840.113554.1.2.2
	// (standard Kerberos V5 mechanism, RFC 4121).
	KerberosV5OIDBytes = []byte{
		0x06, 0x09,
		0x2a, 0x86, 0x48, 0x86, 0xf7, 0x12, 0x01, 0x02, 0x02,
	}

	// MSKerberosV5OIDBytes is the DER-encoded OID 1.2.840.48018.1.2.2
	// (Microsoft Kerberos V5 mechanism). Echoed back when a Windows / SSPI
	// client advertises this OID in SPNEGO negotiation.
	MSKerberosV5OIDBytes = []byte{
		0x06, 0x09,
		0x2a, 0x86, 0x48, 0x82, 0xf7, 0x12, 0x01, 0x02, 0x02,
	}
)

// WrapGSSToken wraps a Kerberos message (e.g. AP-REQ, AP-REP, KRB-ERROR) in a
// GSS-API InitialContextToken envelope per RFC 2743 Section 3.1.
//
// oidBytes must be the full DER encoding of the mechanism OID including the
// 0x06 tag and length prefix (see KerberosV5OIDBytes / MSKerberosV5OIDBytes).
// tokenID is the 2-byte Kerberos token identifier (GSSTokenIDAPReq / APRep /
// KRBError). innerToken is the already-encoded Kerberos message.
func WrapGSSToken(innerToken []byte, oidBytes []byte, tokenID uint16) []byte {
	innerLen := len(oidBytes) + 2 + len(innerToken) // +2 for tokenID
	lengthBytes := encodeASN1Length(innerLen)

	result := make([]byte, 0, 1+len(lengthBytes)+innerLen)
	result = append(result, 0x60) // [APPLICATION 0] IMPLICIT SEQUENCE
	result = append(result, lengthBytes...)
	result = append(result, oidBytes...)
	result = append(result, byte(tokenID>>8), byte(tokenID))
	result = append(result, innerToken...)
	return result
}

// encodeASN1Length encodes a non-negative length value in ASN.1 BER/DER form.
// Uses short form for lengths < 128, long form otherwise.
func encodeASN1Length(length int) []byte {
	if length < 128 {
		return []byte{byte(length)}
	}
	// Long form: count significant bytes, then emit big-endian.
	n := 0
	for v := length; v > 0; v >>= 8 {
		n++
	}
	out := make([]byte, 1+n)
	out[0] = 0x80 | byte(n)
	for i := n; i > 0; i-- {
		out[i] = byte(length)
		length >>= 8
	}
	return out
}
