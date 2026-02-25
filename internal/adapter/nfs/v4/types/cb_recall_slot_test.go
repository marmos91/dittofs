package types

import (
	"bytes"
	"testing"
)

func TestCbRecallSlotArgs_RoundTrip(t *testing.T) {
	original := CbRecallSlotArgs{TargetHighestSlotID: 7}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded CbRecallSlotArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.TargetHighestSlotID != 7 {
		t.Errorf("TargetHighestSlotID: got %d, want 7", decoded.TargetHighestSlotID)
	}
}

func TestCbRecallSlotRes_RoundTrip(t *testing.T) {
	for _, status := range []uint32{NFS4_OK, NFS4ERR_DELAY} {
		original := CbRecallSlotRes{Status: status}

		var buf bytes.Buffer
		if err := original.Encode(&buf); err != nil {
			t.Fatalf("Encode status=%d: %v", status, err)
		}

		var decoded CbRecallSlotRes
		if err := decoded.Decode(&buf); err != nil {
			t.Fatalf("Decode status=%d: %v", status, err)
		}

		if decoded.Status != status {
			t.Errorf("Status: got %d, want %d", decoded.Status, status)
		}
	}
}

func TestCbRecallSlotArgs_String(t *testing.T) {
	args := CbRecallSlotArgs{TargetHighestSlotID: 15}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
