package types

import (
	"bytes"
	"testing"
)

func TestCbPushDelegArgs_RoundTrip(t *testing.T) {
	original := CbPushDelegArgs{
		FH:         []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		Delegation: []byte{0xde, 0xad, 0xbe, 0xef, 0xca, 0xfe},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded CbPushDelegArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if !bytes.Equal(decoded.FH, original.FH) {
		t.Errorf("FH: got %x, want %x", decoded.FH, original.FH)
	}
	if !bytes.Equal(decoded.Delegation, original.Delegation) {
		t.Errorf("Delegation: got %x, want %x", decoded.Delegation, original.Delegation)
	}
}

func TestCbPushDelegRes_RoundTrip(t *testing.T) {
	for _, status := range []uint32{NFS4_OK, NFS4ERR_REJECT_DELEG, NFS4ERR_INVAL} {
		original := CbPushDelegRes{Status: status}

		var buf bytes.Buffer
		if err := original.Encode(&buf); err != nil {
			t.Fatalf("Encode status=%d: %v", status, err)
		}

		var decoded CbPushDelegRes
		if err := decoded.Decode(&buf); err != nil {
			t.Fatalf("Decode status=%d: %v", status, err)
		}

		if decoded.Status != status {
			t.Errorf("Status: got %d, want %d", decoded.Status, status)
		}
	}
}

func TestCbPushDelegArgs_String(t *testing.T) {
	args := CbPushDelegArgs{FH: []byte{0x01}, Delegation: []byte{0x02}}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
