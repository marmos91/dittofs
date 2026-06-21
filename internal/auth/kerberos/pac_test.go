package kerberos

import (
	"testing"

	"github.com/jcmturner/gofork/encoding/asn1"
	"github.com/jcmturner/gokrb5/v8/iana/adtype"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/types"
)

// Regression coverage for #1259: PAC decoding must be best-effort and must NOT
// fail Kerberos authentication for tickets without a (full) PAC. gokrb5's
// VerifyAPREQ couples PAC decoding to AP-REQ verification — a ticket from a
// plain MIT/Heimdal KDC carries a PAC with no KerbValidationInfo buffer, which
// hard-failed authentication. Authenticate now verifies the AP-REQ with PAC
// decoding disabled and decodes the PAC separately via decodePACBestEffort.

// TestDecodePACBestEffort_NoPAC: a ticket with no authorization data (the
// typical non-AD KDC case) must yield no AD credentials and no error, so the
// caller proceeds with a plain authenticated identity.
func TestDecodePACBestEffort_NoPAC(t *testing.T) {
	apReq := &messages.APReq{} // empty ticket => no AuthorizationData

	ad, err := decodePACBestEffort(apReq, &keytab.Keytab{})
	if err != nil {
		t.Fatalf("no-PAC ticket must not error, got: %v", err)
	}
	if ad != nil {
		t.Fatalf("no-PAC ticket must yield nil AD credentials, got: %+v", ad)
	}
}

// TestDecodePACBestEffort_MalformedPAC: a ticket that DOES carry a PAC the
// library cannot decode must return an error (so the caller skips AD attrs)
// rather than panicking. The key regression property is that the caller treats
// this as "no AD attributes", never as an authentication failure.
func TestDecodePACBestEffort_MalformedPAC(t *testing.T) {
	// Inner AD-WIN2K-PAC entry carrying bytes that fail pac.PACType.Unmarshal
	// (claims 16 buffers but provides none).
	innerAD := types.AuthorizationData{
		{ADType: adtype.ADWin2KPAC, ADData: []byte{0x10, 0, 0, 0, 0, 0, 0, 0}},
	}
	innerBytes, err := asn1.Marshal(innerAD)
	if err != nil {
		t.Fatalf("marshal inner AuthorizationData: %v", err)
	}

	apReq := &messages.APReq{}
	apReq.Ticket.DecryptedEncPart.AuthorizationData = types.AuthorizationData{
		{ADType: adtype.ADIfRelevant, ADData: innerBytes},
	}

	ad, err := decodePACBestEffort(apReq, &keytab.Keytab{})
	if err == nil {
		t.Fatal("malformed PAC must return an error so the caller skips AD attrs")
	}
	if ad != nil {
		t.Fatalf("malformed PAC must yield nil AD credentials, got: %+v", ad)
	}
}
