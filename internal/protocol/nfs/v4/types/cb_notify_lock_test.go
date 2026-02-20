package types

import (
	"bytes"
	"testing"
)

func TestCbNotifyLockArgs_RoundTrip(t *testing.T) {
	original := CbNotifyLockArgs{
		FH: []byte{0x01, 0x02, 0x03, 0x04},
		LockOwner: LockOwner4{
			ClientID: 0xDEADBEEFCAFEBABE,
			Owner:    []byte("lock-owner-test"),
		},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded CbNotifyLockArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if !bytes.Equal(decoded.FH, original.FH) {
		t.Errorf("FH: got %x, want %x", decoded.FH, original.FH)
	}
	if decoded.LockOwner.ClientID != 0xDEADBEEFCAFEBABE {
		t.Errorf("ClientID: got 0x%x, want 0xDEADBEEFCAFEBABE", decoded.LockOwner.ClientID)
	}
	if !bytes.Equal(decoded.LockOwner.Owner, []byte("lock-owner-test")) {
		t.Errorf("Owner: got %q, want %q", decoded.LockOwner.Owner, "lock-owner-test")
	}
}

func TestCbNotifyLockRes_RoundTrip(t *testing.T) {
	for _, status := range []uint32{NFS4_OK, NFS4ERR_DELAY} {
		original := CbNotifyLockRes{Status: status}

		var buf bytes.Buffer
		if err := original.Encode(&buf); err != nil {
			t.Fatalf("Encode status=%d: %v", status, err)
		}

		var decoded CbNotifyLockRes
		if err := decoded.Decode(&buf); err != nil {
			t.Fatalf("Decode status=%d: %v", status, err)
		}

		if decoded.Status != status {
			t.Errorf("Status: got %d, want %d", decoded.Status, status)
		}
	}
}

func TestLockOwner4_RoundTrip(t *testing.T) {
	original := LockOwner4{
		ClientID: 12345,
		Owner:    []byte{0xaa, 0xbb, 0xcc},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded LockOwner4
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.ClientID != 12345 {
		t.Errorf("ClientID: got %d, want 12345", decoded.ClientID)
	}
	if !bytes.Equal(decoded.Owner, original.Owner) {
		t.Errorf("Owner: got %x, want %x", decoded.Owner, original.Owner)
	}
}

func TestCbNotifyLockArgs_String(t *testing.T) {
	args := CbNotifyLockArgs{
		FH:        []byte{0x01},
		LockOwner: LockOwner4{ClientID: 1, Owner: []byte("test")},
	}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
