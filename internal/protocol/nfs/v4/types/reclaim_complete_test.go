package types

import (
	"bytes"
	"testing"
)

func TestReclaimCompleteArgs_RoundTrip_AllFS(t *testing.T) {
	original := ReclaimCompleteArgs{OneFS: false}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded ReclaimCompleteArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.OneFS != false {
		t.Errorf("OneFS: got %t, want false", decoded.OneFS)
	}
}

func TestReclaimCompleteArgs_RoundTrip_OneFS(t *testing.T) {
	original := ReclaimCompleteArgs{OneFS: true}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded ReclaimCompleteArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.OneFS != true {
		t.Errorf("OneFS: got %t, want true", decoded.OneFS)
	}
}

func TestReclaimCompleteRes_RoundTrip_OK(t *testing.T) {
	original := ReclaimCompleteRes{Status: NFS4_OK}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded ReclaimCompleteRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4_OK)
	}
}

func TestReclaimCompleteRes_RoundTrip_Error(t *testing.T) {
	original := ReclaimCompleteRes{Status: NFS4ERR_COMPLETE_ALREADY}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded ReclaimCompleteRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4ERR_COMPLETE_ALREADY {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4ERR_COMPLETE_ALREADY)
	}
}

func TestReclaimCompleteArgs_String(t *testing.T) {
	args := ReclaimCompleteArgs{OneFS: true}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
