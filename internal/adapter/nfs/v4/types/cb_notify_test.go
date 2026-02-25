package types

import (
	"bytes"
	"testing"
)

func TestCbNotifyArgs_RoundTrip(t *testing.T) {
	fh := []byte{0xaa, 0xbb, 0xcc, 0xdd}
	original := CbNotifyArgs{
		Stateid: ValidStateid(),
		FH:      fh,
		Changes: []Notify4{
			{
				Mask:   Bitmap4{1 << NOTIFY4_ADD_ENTRY},
				Values: []NotifyEntry4{{Data: []byte{0x01, 0x02, 0x03}}},
			},
			{
				Mask:   Bitmap4{1 << NOTIFY4_REMOVE_ENTRY},
				Values: []NotifyEntry4{{Data: []byte{0x04, 0x05}}},
			},
		},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded CbNotifyArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Stateid.Seqid != original.Stateid.Seqid {
		t.Errorf("Stateid.Seqid: got %d, want %d", decoded.Stateid.Seqid, original.Stateid.Seqid)
	}
	if !bytes.Equal(decoded.FH, fh) {
		t.Errorf("FH: got %x, want %x", decoded.FH, fh)
	}
	if len(decoded.Changes) != 2 {
		t.Fatalf("Changes length: got %d, want 2", len(decoded.Changes))
	}
	// Verify first change
	if len(decoded.Changes[0].Mask) != 1 {
		t.Fatalf("Changes[0].Mask length: got %d, want 1", len(decoded.Changes[0].Mask))
	}
	if decoded.Changes[0].Mask[0] != (1 << NOTIFY4_ADD_ENTRY) {
		t.Errorf("Changes[0].Mask: got 0x%x, want 0x%x",
			decoded.Changes[0].Mask[0], 1<<NOTIFY4_ADD_ENTRY)
	}
	if len(decoded.Changes[0].Values) != 1 {
		t.Fatalf("Changes[0].Values length: got %d, want 1", len(decoded.Changes[0].Values))
	}
	if !bytes.Equal(decoded.Changes[0].Values[0].Data, []byte{0x01, 0x02, 0x03}) {
		t.Errorf("Changes[0].Values[0].Data: got %x, want 010203", decoded.Changes[0].Values[0].Data)
	}
	// Verify second change
	if len(decoded.Changes[1].Values) != 1 {
		t.Fatalf("Changes[1].Values length: got %d, want 1", len(decoded.Changes[1].Values))
	}
	if !bytes.Equal(decoded.Changes[1].Values[0].Data, []byte{0x04, 0x05}) {
		t.Errorf("Changes[1].Values[0].Data: got %x, want 0405", decoded.Changes[1].Values[0].Data)
	}
}

func TestCbNotifyArgs_RoundTrip_Empty(t *testing.T) {
	original := CbNotifyArgs{
		Stateid: ValidStateid(),
		FH:      []byte{0x01},
		Changes: nil,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded CbNotifyArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if len(decoded.Changes) != 0 {
		t.Errorf("Changes: got %d, want 0", len(decoded.Changes))
	}
}

func TestCbNotifyRes_RoundTrip(t *testing.T) {
	for _, status := range []uint32{NFS4_OK, NFS4ERR_BAD_STATEID, NFS4ERR_INVAL} {
		original := CbNotifyRes{Status: status}

		var buf bytes.Buffer
		if err := original.Encode(&buf); err != nil {
			t.Fatalf("Encode status=%d: %v", status, err)
		}

		var decoded CbNotifyRes
		if err := decoded.Decode(&buf); err != nil {
			t.Fatalf("Decode status=%d: %v", status, err)
		}

		if decoded.Status != status {
			t.Errorf("Status: got %d, want %d", decoded.Status, status)
		}
	}
}

func TestCbNotifyArgs_String(t *testing.T) {
	args := CbNotifyArgs{Stateid: ValidStateid(), FH: []byte{0x01}}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

func TestNotify4_String(t *testing.T) {
	n := Notify4{Mask: Bitmap4{0x01}, Values: []NotifyEntry4{{Data: []byte{0x01}}}}
	s := n.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
