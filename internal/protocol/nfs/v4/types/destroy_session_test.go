package types

import (
	"bytes"
	"testing"
)

// TestDestroySessionArgs_RoundTrip tests encode/decode of DESTROY_SESSION args.
func TestDestroySessionArgs_RoundTrip(t *testing.T) {
	original := &DestroySessionArgs{
		SessionID: ValidSessionId(),
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	// SessionId4 is 16 bytes, no length prefix (fixed-size opaque)
	if buf.Len() != NFS4_SESSIONID_SIZE {
		t.Errorf("Encoded size = %d, want %d", buf.Len(), NFS4_SESSIONID_SIZE)
	}

	decoded := &DestroySessionArgs{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.SessionID != original.SessionID {
		t.Errorf("SessionID = %s, want %s", decoded.SessionID.String(), original.SessionID.String())
	}

	// Verify String() doesn't panic
	s := original.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

// TestDestroySessionRes_RoundTrip tests encode/decode of DESTROY_SESSION response.
func TestDestroySessionRes_RoundTrip(t *testing.T) {
	// Test success
	original := &DestroySessionRes{Status: NFS4_OK}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded := &DestroySessionRes{}
	if err := decoded.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Errorf("Status = %d, want %d", decoded.Status, NFS4_OK)
	}

	// Test error case
	errorRes := &DestroySessionRes{Status: NFS4ERR_BADSESSION}

	buf.Reset()
	if err := errorRes.Encode(&buf); err != nil {
		t.Fatalf("Encode error failed: %v", err)
	}

	decodedErr := &DestroySessionRes{}
	if err := decodedErr.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Decode error failed: %v", err)
	}

	if decodedErr.Status != NFS4ERR_BADSESSION {
		t.Errorf("Status = %d, want %d", decodedErr.Status, NFS4ERR_BADSESSION)
	}

	// Verify String() doesn't panic
	s := errorRes.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
