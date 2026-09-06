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
