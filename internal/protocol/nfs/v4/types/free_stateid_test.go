package types

import (
	"bytes"
	"testing"
)

func TestFreeStateidArgs_RoundTrip(t *testing.T) {
	original := FreeStateidArgs{
		Stateid: ValidStateid(),
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded FreeStateidArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Stateid.Seqid != original.Stateid.Seqid {
		t.Errorf("Seqid: got %d, want %d", decoded.Stateid.Seqid, original.Stateid.Seqid)
	}
	if decoded.Stateid.Other != original.Stateid.Other {
		t.Errorf("Other: got %x, want %x", decoded.Stateid.Other, original.Stateid.Other)
	}
}

func TestFreeStateidRes_RoundTrip_OK(t *testing.T) {
	original := FreeStateidRes{Status: NFS4_OK}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded FreeStateidRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4_OK)
	}
}

func TestFreeStateidRes_RoundTrip_Error(t *testing.T) {
	original := FreeStateidRes{Status: NFS4ERR_BAD_STATEID}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded FreeStateidRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4ERR_BAD_STATEID {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4ERR_BAD_STATEID)
	}
}

func TestFreeStateidArgs_String(t *testing.T) {
	args := FreeStateidArgs{Stateid: ValidStateid()}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
