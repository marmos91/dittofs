package types

import (
	"bytes"
	"testing"
)

// TestCreateSessionArgs_RoundTrip tests with channel attrs, cb_program, and sec parms.
func TestCreateSessionArgs_RoundTrip(t *testing.T) {
	original := &CreateSessionArgs{
		ClientID:         0x123456789abcdef0,
		SequenceID:       1,
		Flags:            CREATE_SESSION4_FLAG_PERSIST | CREATE_SESSION4_FLAG_CONN_BACK_CHAN,
		ForeChannelAttrs: ValidChannelAttrs(),
		BackChannelAttrs: ChannelAttrs{
			HeaderPadSize:         0,
			MaxRequestSize:        4096,
			MaxResponseSize:       4096,
			MaxResponseSizeCached: 4096,
			MaxOperations:         2,
			MaxRequests:           1,
			RdmaIrd:               nil,
		},
		CbProgram: 0x40000000,
		CbSecParms: []CallbackSecParms4{
			{CbSecFlavor: 0}, // AUTH_NONE
		},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &CreateSessionArgs{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.ClientID != original.ClientID {
		t.Errorf("ClientID = 0x%x, want 0x%x", decoded.ClientID, original.ClientID)
	}
	if decoded.SequenceID != original.SequenceID {
		t.Errorf("SequenceID = %d, want %d", decoded.SequenceID, original.SequenceID)
	}
	if decoded.Flags != original.Flags {
		t.Errorf("Flags = 0x%x, want 0x%x", decoded.Flags, original.Flags)
	}
	if decoded.ForeChannelAttrs.MaxRequestSize != original.ForeChannelAttrs.MaxRequestSize {
		t.Errorf("ForeChannel.MaxRequestSize = %d, want %d",
			decoded.ForeChannelAttrs.MaxRequestSize, original.ForeChannelAttrs.MaxRequestSize)
	}
	if decoded.ForeChannelAttrs.MaxOperations != original.ForeChannelAttrs.MaxOperations {
		t.Errorf("ForeChannel.MaxOperations = %d, want %d",
			decoded.ForeChannelAttrs.MaxOperations, original.ForeChannelAttrs.MaxOperations)
	}
	if decoded.BackChannelAttrs.MaxRequestSize != original.BackChannelAttrs.MaxRequestSize {
		t.Errorf("BackChannel.MaxRequestSize = %d, want %d",
			decoded.BackChannelAttrs.MaxRequestSize, original.BackChannelAttrs.MaxRequestSize)
	}
	if decoded.CbProgram != original.CbProgram {
		t.Errorf("CbProgram = 0x%x, want 0x%x", decoded.CbProgram, original.CbProgram)
	}
	if len(decoded.CbSecParms) != 1 {
		t.Fatalf("CbSecParms length = %d, want 1", len(decoded.CbSecParms))
	}
	if decoded.CbSecParms[0].CbSecFlavor != 0 {
		t.Errorf("CbSecParms[0].CbSecFlavor = %d, want 0", decoded.CbSecParms[0].CbSecFlavor)
	}

	// Verify String() doesn't panic
	s := original.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

// TestCreateSessionArgs_RoundTrip_MultipleSec tests with multiple security params.
func TestCreateSessionArgs_RoundTrip_MultipleSec(t *testing.T) {
	original := &CreateSessionArgs{
		ClientID:         0xdeadbeefcafebabe,
		SequenceID:       2,
		Flags:            0,
		ForeChannelAttrs: ValidChannelAttrs(),
		BackChannelAttrs: ValidChannelAttrs(),
		CbProgram:        0x40000001,
		CbSecParms: []CallbackSecParms4{
			{CbSecFlavor: 0}, // AUTH_NONE
			{CbSecFlavor: 1, AuthSysParms: &AuthSysParms{Stamp: 1, MachineName: "test", UID: 0, GID: 0}}, // AUTH_SYS
		},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &CreateSessionArgs{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if len(decoded.CbSecParms) != 2 {
		t.Fatalf("CbSecParms length = %d, want 2", len(decoded.CbSecParms))
	}
	if decoded.CbSecParms[0].CbSecFlavor != 0 {
		t.Errorf("CbSecParms[0].CbSecFlavor = %d, want 0", decoded.CbSecParms[0].CbSecFlavor)
	}
	if decoded.CbSecParms[1].CbSecFlavor != 1 {
		t.Errorf("CbSecParms[1].CbSecFlavor = %d, want 1", decoded.CbSecParms[1].CbSecFlavor)
	}
	if decoded.CbSecParms[1].AuthSysParms == nil || decoded.CbSecParms[1].AuthSysParms.MachineName != "test" {
		t.Errorf("CbSecParms[1].AuthSysParms roundtrip failed")
	}
}

// TestCreateSessionRes_RoundTrip tests a success response with negotiated attrs.
func TestCreateSessionRes_RoundTrip(t *testing.T) {
	original := &CreateSessionRes{
		Status:           NFS4_OK,
		SessionID:        ValidSessionId(),
		SequenceID:       1,
		Flags:            CREATE_SESSION4_FLAG_PERSIST | CREATE_SESSION4_FLAG_CONN_BACK_CHAN,
		ForeChannelAttrs: ValidChannelAttrs(),
		BackChannelAttrs: ChannelAttrs{
			HeaderPadSize:         0,
			MaxRequestSize:        4096,
			MaxResponseSize:       4096,
			MaxResponseSizeCached: 4096,
			MaxOperations:         2,
			MaxRequests:           1,
			RdmaIrd:               nil,
		},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &CreateSessionRes{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Errorf("Status = %d, want %d", decoded.Status, NFS4_OK)
	}
	if decoded.SessionID != original.SessionID {
		t.Errorf("SessionID = %s, want %s", decoded.SessionID.String(), original.SessionID.String())
	}
	if decoded.SequenceID != original.SequenceID {
		t.Errorf("SequenceID = %d, want %d", decoded.SequenceID, original.SequenceID)
	}
	if decoded.Flags != original.Flags {
		t.Errorf("Flags = 0x%x, want 0x%x", decoded.Flags, original.Flags)
	}
	if decoded.ForeChannelAttrs.MaxRequestSize != original.ForeChannelAttrs.MaxRequestSize {
		t.Errorf("ForeChannel.MaxRequestSize = %d, want %d",
			decoded.ForeChannelAttrs.MaxRequestSize, original.ForeChannelAttrs.MaxRequestSize)
	}
	if decoded.BackChannelAttrs.MaxRequests != original.BackChannelAttrs.MaxRequests {
		t.Errorf("BackChannel.MaxRequests = %d, want %d",
			decoded.BackChannelAttrs.MaxRequests, original.BackChannelAttrs.MaxRequests)
	}

	// Verify String() doesn't panic
	s := decoded.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

// TestCreateSessionRes_RoundTrip_Error tests an error response.
func TestCreateSessionRes_RoundTrip_Error(t *testing.T) {
	original := &CreateSessionRes{
		Status: NFS4ERR_STALE_CLIENTID,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &CreateSessionRes{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.Status != NFS4ERR_STALE_CLIENTID {
		t.Errorf("Status = %d, want %d", decoded.Status, NFS4ERR_STALE_CLIENTID)
	}
	// Session ID should be zero (not decoded)
	var zeroSession SessionId4
	if decoded.SessionID != zeroSession {
		t.Errorf("SessionID = %s, want zero (error case)", decoded.SessionID.String())
	}

	// Verify error String() format
	s := decoded.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
