package types

import (
	"bytes"
	"testing"
)

func TestSecinfoNoNameArgs_RoundTrip_CurrentFH(t *testing.T) {
	original := SecinfoNoNameArgs{Style: SECINFO_STYLE4_CURRENT_FH}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded SecinfoNoNameArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Style != SECINFO_STYLE4_CURRENT_FH {
		t.Errorf("Style: got %d, want %d", decoded.Style, SECINFO_STYLE4_CURRENT_FH)
	}
}

func TestSecinfoNoNameArgs_RoundTrip_Parent(t *testing.T) {
	original := SecinfoNoNameArgs{Style: SECINFO_STYLE4_PARENT}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded SecinfoNoNameArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Style != SECINFO_STYLE4_PARENT {
		t.Errorf("Style: got %d, want %d", decoded.Style, SECINFO_STYLE4_PARENT)
	}
}

func TestSecinfoNoNameRes_RoundTrip_OK(t *testing.T) {
	original := SecinfoNoNameRes{Status: NFS4_OK}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded SecinfoNoNameRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4_OK)
	}
}

func TestSecinfoNoNameRes_RoundTrip_Error(t *testing.T) {
	original := SecinfoNoNameRes{Status: NFS4ERR_WRONGSEC}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded SecinfoNoNameRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4ERR_WRONGSEC {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4ERR_WRONGSEC)
	}
}

func TestSecinfoNoNameArgs_String(t *testing.T) {
	args := SecinfoNoNameArgs{Style: SECINFO_STYLE4_PARENT}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
	if !bytes.Contains([]byte(s), []byte("PARENT")) {
		t.Errorf("String() should contain PARENT: %s", s)
	}
}
