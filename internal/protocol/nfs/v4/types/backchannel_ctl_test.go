package types

import (
	"bytes"
	"testing"
)

// TestBackchannelCtlArgs_RoundTrip tests with cb_program and AUTH_NONE sec parms.
func TestBackchannelCtlArgs_RoundTrip(t *testing.T) {
	original := &BackchannelCtlArgs{
		CbProgram: 0x40000000,
		SecParms: []CallbackSecParms4{
			{CbSecFlavor: 0}, // AUTH_NONE
		},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &BackchannelCtlArgs{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.CbProgram != original.CbProgram {
		t.Errorf("CbProgram = 0x%x, want 0x%x", decoded.CbProgram, original.CbProgram)
	}
	if len(decoded.SecParms) != 1 {
		t.Fatalf("SecParms length = %d, want 1", len(decoded.SecParms))
	}
	if decoded.SecParms[0].CbSecFlavor != 0 {
		t.Errorf("SecParms[0].CbSecFlavor = %d, want 0 (AUTH_NONE)", decoded.SecParms[0].CbSecFlavor)
	}

	// Verify String() doesn't panic
	s := original.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

// TestBackchannelCtlArgs_RoundTrip_MultipleSecParms tests with multiple security param types.
func TestBackchannelCtlArgs_RoundTrip_MultipleSecParms(t *testing.T) {
	original := &BackchannelCtlArgs{
		CbProgram: 0x40000001,
		SecParms: []CallbackSecParms4{
			{CbSecFlavor: 0}, // AUTH_NONE
			{CbSecFlavor: 1, AuthSysData: []byte{0xca, 0xfe, 0xba, 0xbe}}, // AUTH_SYS
			{CbSecFlavor: 6, RpcGssData: []byte{0xde, 0xad}},               // RPCSEC_GSS
		},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &BackchannelCtlArgs{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if len(decoded.SecParms) != 3 {
		t.Fatalf("SecParms length = %d, want 3", len(decoded.SecParms))
	}
	if decoded.SecParms[0].CbSecFlavor != 0 {
		t.Errorf("SecParms[0].CbSecFlavor = %d, want 0", decoded.SecParms[0].CbSecFlavor)
	}
	if decoded.SecParms[1].CbSecFlavor != 1 {
		t.Errorf("SecParms[1].CbSecFlavor = %d, want 1", decoded.SecParms[1].CbSecFlavor)
	}
	if !bytes.Equal(decoded.SecParms[1].AuthSysData, original.SecParms[1].AuthSysData) {
		t.Errorf("SecParms[1].AuthSysData = %x, want %x",
			decoded.SecParms[1].AuthSysData, original.SecParms[1].AuthSysData)
	}
	if decoded.SecParms[2].CbSecFlavor != 6 {
		t.Errorf("SecParms[2].CbSecFlavor = %d, want 6", decoded.SecParms[2].CbSecFlavor)
	}
	if !bytes.Equal(decoded.SecParms[2].RpcGssData, original.SecParms[2].RpcGssData) {
		t.Errorf("SecParms[2].RpcGssData = %x, want %x",
			decoded.SecParms[2].RpcGssData, original.SecParms[2].RpcGssData)
	}
}

// TestBackchannelCtlRes_RoundTrip tests status-only response.
func TestBackchannelCtlRes_RoundTrip(t *testing.T) {
	// Test success
	original := &BackchannelCtlRes{Status: NFS4_OK}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &BackchannelCtlRes{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Errorf("Status = %d, want %d", decoded.Status, NFS4_OK)
	}

	// Test error case
	errorRes := &BackchannelCtlRes{Status: NFS4ERR_BACK_CHAN_BUSY}

	buf.Reset()
	if err := errorRes.Encode(&buf); err != nil {
		t.Fatalf("Encode error failed: %v", err)
	}

	decodedErr := &BackchannelCtlRes{}
	if err := decodedErr.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode error failed: %v", err)
	}

	if decodedErr.Status != NFS4ERR_BACK_CHAN_BUSY {
		t.Errorf("Status = %d, want %d", decodedErr.Status, NFS4ERR_BACK_CHAN_BUSY)
	}

	// Verify String() doesn't panic
	s := errorRes.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
