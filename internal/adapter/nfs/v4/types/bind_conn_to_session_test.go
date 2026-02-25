package types

import (
	"bytes"
	"testing"
)

// TestBindConnToSessionArgs_RoundTrip tests with CDFC4_FORE_OR_BOTH direction.
func TestBindConnToSessionArgs_RoundTrip(t *testing.T) {
	original := &BindConnToSessionArgs{
		SessionID:         ValidSessionId(),
		Dir:               CDFC4_FORE_OR_BOTH,
		UseConnInRDMAMode: false,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &BindConnToSessionArgs{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.SessionID != original.SessionID {
		t.Errorf("SessionID = %s, want %s", decoded.SessionID.String(), original.SessionID.String())
	}
	if decoded.Dir != CDFC4_FORE_OR_BOTH {
		t.Errorf("Dir = %d, want %d (CDFC4_FORE_OR_BOTH)", decoded.Dir, CDFC4_FORE_OR_BOTH)
	}
	if decoded.UseConnInRDMAMode != false {
		t.Errorf("UseConnInRDMAMode = %t, want false", decoded.UseConnInRDMAMode)
	}

	// Verify String() doesn't panic and includes direction name
	s := original.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

// TestBindConnToSessionArgs_RoundTrip_BackOrBoth tests with CDFC4_BACK_OR_BOTH direction.
func TestBindConnToSessionArgs_RoundTrip_BackOrBoth(t *testing.T) {
	original := &BindConnToSessionArgs{
		SessionID:         ValidSessionId(),
		Dir:               CDFC4_BACK_OR_BOTH,
		UseConnInRDMAMode: true,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &BindConnToSessionArgs{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.Dir != CDFC4_BACK_OR_BOTH {
		t.Errorf("Dir = %d, want %d (CDFC4_BACK_OR_BOTH)", decoded.Dir, CDFC4_BACK_OR_BOTH)
	}
	if decoded.UseConnInRDMAMode != true {
		t.Errorf("UseConnInRDMAMode = %t, want true", decoded.UseConnInRDMAMode)
	}
}

// TestBindConnToSessionRes_RoundTrip tests with CDFS4_BOTH direction.
func TestBindConnToSessionRes_RoundTrip(t *testing.T) {
	original := &BindConnToSessionRes{
		Status:            NFS4_OK,
		SessionID:         ValidSessionId(),
		Dir:               CDFS4_BOTH,
		UseConnInRDMAMode: false,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &BindConnToSessionRes{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Errorf("Status = %d, want %d", decoded.Status, NFS4_OK)
	}
	if decoded.SessionID != original.SessionID {
		t.Errorf("SessionID = %s, want %s", decoded.SessionID.String(), original.SessionID.String())
	}
	if decoded.Dir != CDFS4_BOTH {
		t.Errorf("Dir = %d, want %d (CDFS4_BOTH)", decoded.Dir, CDFS4_BOTH)
	}
	if decoded.UseConnInRDMAMode != false {
		t.Errorf("UseConnInRDMAMode = %t, want false", decoded.UseConnInRDMAMode)
	}

	// Verify String() doesn't panic
	s := decoded.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

// TestBindConnToSessionRes_RoundTrip_Error tests an error response.
func TestBindConnToSessionRes_RoundTrip_Error(t *testing.T) {
	original := &BindConnToSessionRes{
		Status: NFS4ERR_BADSESSION,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &BindConnToSessionRes{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.Status != NFS4ERR_BADSESSION {
		t.Errorf("Status = %d, want %d", decoded.Status, NFS4ERR_BADSESSION)
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
