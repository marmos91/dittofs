package types

import (
	"bytes"
	"testing"
)

func TestCbWantsCancelledArgs_RoundTrip(t *testing.T) {
	original := CbWantsCancelledArgs{
		ContendedWantsCancelled: true,
		ResourcedWantsCancelled: false,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded CbWantsCancelledArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.ContendedWantsCancelled != true {
		t.Errorf("ContendedWantsCancelled: got %t, want true", decoded.ContendedWantsCancelled)
	}
	if decoded.ResourcedWantsCancelled != false {
		t.Errorf("ResourcedWantsCancelled: got %t, want false", decoded.ResourcedWantsCancelled)
	}
}

func TestCbWantsCancelledArgs_RoundTrip_BothTrue(t *testing.T) {
	original := CbWantsCancelledArgs{
		ContendedWantsCancelled: true,
		ResourcedWantsCancelled: true,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded CbWantsCancelledArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.ContendedWantsCancelled != true {
		t.Errorf("ContendedWantsCancelled: got %t, want true", decoded.ContendedWantsCancelled)
	}
	if decoded.ResourcedWantsCancelled != true {
		t.Errorf("ResourcedWantsCancelled: got %t, want true", decoded.ResourcedWantsCancelled)
	}
}

func TestCbWantsCancelledRes_RoundTrip(t *testing.T) {
	for _, status := range []uint32{NFS4_OK, NFS4ERR_INVAL} {
		original := CbWantsCancelledRes{Status: status}

		var buf bytes.Buffer
		if err := original.Encode(&buf); err != nil {
			t.Fatalf("Encode status=%d: %v", status, err)
		}

		var decoded CbWantsCancelledRes
		if err := decoded.Decode(&buf); err != nil {
			t.Fatalf("Decode status=%d: %v", status, err)
		}

		if decoded.Status != status {
			t.Errorf("Status: got %d, want %d", decoded.Status, status)
		}
	}
}

func TestCbWantsCancelledArgs_String(t *testing.T) {
	args := CbWantsCancelledArgs{ContendedWantsCancelled: true, ResourcedWantsCancelled: false}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
