package types

import (
	"bytes"
	"testing"
)

func TestDestroyClientidArgs_RoundTrip(t *testing.T) {
	original := DestroyClientidArgs{ClientID: 0xDEADBEEFCAFEBABE}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded DestroyClientidArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.ClientID != original.ClientID {
		t.Errorf("ClientID: got 0x%x, want 0x%x", decoded.ClientID, original.ClientID)
	}
}

func TestDestroyClientidRes_RoundTrip_OK(t *testing.T) {
	original := DestroyClientidRes{Status: NFS4_OK}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded DestroyClientidRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4_OK)
	}
}

func TestDestroyClientidRes_RoundTrip_Error(t *testing.T) {
	original := DestroyClientidRes{Status: NFS4ERR_CLIENTID_BUSY}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded DestroyClientidRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4ERR_CLIENTID_BUSY {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4ERR_CLIENTID_BUSY)
	}
}

func TestDestroyClientidArgs_String(t *testing.T) {
	args := DestroyClientidArgs{ClientID: 0x1234}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
