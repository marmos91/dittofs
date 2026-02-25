package types

import (
	"bytes"
	"testing"
)

func TestCbRecallAnyArgs_RoundTrip(t *testing.T) {
	original := CbRecallAnyArgs{
		ObjectsToKeep: 5,
		TypeMask:      Bitmap4{(1 << RCA4_TYPE_MASK_RDATA_DLG) | (1 << RCA4_TYPE_MASK_WDATA_DLG)},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded CbRecallAnyArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.ObjectsToKeep != 5 {
		t.Errorf("ObjectsToKeep: got %d, want 5", decoded.ObjectsToKeep)
	}
	if len(decoded.TypeMask) != 1 {
		t.Fatalf("TypeMask length: got %d, want 1", len(decoded.TypeMask))
	}
	expected := uint32((1 << RCA4_TYPE_MASK_RDATA_DLG) | (1 << RCA4_TYPE_MASK_WDATA_DLG))
	if decoded.TypeMask[0] != expected {
		t.Errorf("TypeMask[0]: got 0x%x, want 0x%x", decoded.TypeMask[0], expected)
	}
}

func TestCbRecallAnyRes_RoundTrip(t *testing.T) {
	for _, status := range []uint32{NFS4_OK, NFS4ERR_DELAY} {
		original := CbRecallAnyRes{Status: status}

		var buf bytes.Buffer
		if err := original.Encode(&buf); err != nil {
			t.Fatalf("Encode status=%d: %v", status, err)
		}

		var decoded CbRecallAnyRes
		if err := decoded.Decode(&buf); err != nil {
			t.Fatalf("Decode status=%d: %v", status, err)
		}

		if decoded.Status != status {
			t.Errorf("Status: got %d, want %d", decoded.Status, status)
		}
	}
}

func TestCbRecallAnyArgs_String(t *testing.T) {
	args := CbRecallAnyArgs{ObjectsToKeep: 3, TypeMask: Bitmap4{0x01}}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

func TestCbRecallableObjAvailArgs_RoundTrip(t *testing.T) {
	original := CbRecallableObjAvailArgs{}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	// Void args should produce zero bytes
	if buf.Len() != 0 {
		t.Errorf("Encode produced %d bytes, want 0", buf.Len())
	}

	var decoded CbRecallableObjAvailArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
}

func TestCbRecallableObjAvailRes_RoundTrip(t *testing.T) {
	original := CbRecallableObjAvailRes{Status: NFS4_OK}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded CbRecallableObjAvailRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4_OK)
	}
}

func TestCbRecallableObjAvailArgs_String(t *testing.T) {
	args := CbRecallableObjAvailArgs{}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
