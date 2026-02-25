package types

import (
	"bytes"
	"testing"
)

func TestSetSsvArgs_RoundTrip(t *testing.T) {
	original := SetSsvArgs{
		SSV:    []byte{0x01, 0x02, 0x03, 0x04, 0x05},
		Digest: []byte{0xaa, 0xbb, 0xcc, 0xdd},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded SetSsvArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if !bytes.Equal(decoded.SSV, original.SSV) {
		t.Errorf("SSV: got %x, want %x", decoded.SSV, original.SSV)
	}
	if !bytes.Equal(decoded.Digest, original.Digest) {
		t.Errorf("Digest: got %x, want %x", decoded.Digest, original.Digest)
	}
}

func TestSetSsvRes_RoundTrip_OK(t *testing.T) {
	original := SetSsvRes{
		Status: NFS4_OK,
		Digest: []byte{0xde, 0xad, 0xbe, 0xef},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded SetSsvRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Fatalf("Status: got %d, want %d", decoded.Status, NFS4_OK)
	}
	if !bytes.Equal(decoded.Digest, original.Digest) {
		t.Errorf("Digest: got %x, want %x", decoded.Digest, original.Digest)
	}
}

func TestSetSsvRes_RoundTrip_Error(t *testing.T) {
	original := SetSsvRes{Status: NFS4ERR_INVAL}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded SetSsvRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4ERR_INVAL {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4ERR_INVAL)
	}
	if decoded.Digest != nil {
		t.Errorf("Digest should be nil on error, got %x", decoded.Digest)
	}
}

func TestSetSsvArgs_String(t *testing.T) {
	args := SetSsvArgs{SSV: []byte{1, 2, 3}, Digest: []byte{4, 5}}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
