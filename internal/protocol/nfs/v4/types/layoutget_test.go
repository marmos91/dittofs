package types

import (
	"bytes"
	"testing"
)

func TestLayoutGetArgs_RoundTrip(t *testing.T) {
	original := LayoutGetArgs{
		Signal:     true,
		LayoutType: LAYOUT4_NFSV4_1_FILES,
		IOMode:     LAYOUTIOMODE4_RW,
		Offset:     4096,
		Length:     1048576,
		MinLength:  65536,
		Stateid:    ValidStateid(),
		MaxCount:   131072,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded LayoutGetArgs
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Signal != original.Signal {
		t.Errorf("Signal: got %t, want %t", decoded.Signal, original.Signal)
	}
	if decoded.LayoutType != original.LayoutType {
		t.Errorf("LayoutType: got %d, want %d", decoded.LayoutType, original.LayoutType)
	}
	if decoded.IOMode != original.IOMode {
		t.Errorf("IOMode: got %d, want %d", decoded.IOMode, original.IOMode)
	}
	if decoded.Offset != original.Offset {
		t.Errorf("Offset: got %d, want %d", decoded.Offset, original.Offset)
	}
	if decoded.Length != original.Length {
		t.Errorf("Length: got %d, want %d", decoded.Length, original.Length)
	}
	if decoded.MinLength != original.MinLength {
		t.Errorf("MinLength: got %d, want %d", decoded.MinLength, original.MinLength)
	}
	if decoded.Stateid.Seqid != original.Stateid.Seqid {
		t.Errorf("Stateid.Seqid: got %d, want %d", decoded.Stateid.Seqid, original.Stateid.Seqid)
	}
	if decoded.MaxCount != original.MaxCount {
		t.Errorf("MaxCount: got %d, want %d", decoded.MaxCount, original.MaxCount)
	}
}

func TestLayoutGetRes_RoundTrip_OK(t *testing.T) {
	original := LayoutGetRes{
		Status:        NFS4_OK,
		ReturnOnClose: true,
		Stateid:       ValidStateid(),
		Layouts: []Layout4{
			{
				Offset: 0,
				Length: 1048576,
				IOMode: LAYOUTIOMODE4_RW,
				Type:   LAYOUT4_NFSV4_1_FILES,
				Body:   []byte{0xaa, 0xbb, 0xcc},
			},
		},
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded LayoutGetRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4_OK {
		t.Fatalf("Status: got %d, want OK", decoded.Status)
	}
	if decoded.ReturnOnClose != true {
		t.Errorf("ReturnOnClose: got %t, want true", decoded.ReturnOnClose)
	}
	if len(decoded.Layouts) != 1 {
		t.Fatalf("Layouts length: got %d, want 1", len(decoded.Layouts))
	}
	if decoded.Layouts[0].Offset != 0 {
		t.Errorf("Layouts[0].Offset: got %d, want 0", decoded.Layouts[0].Offset)
	}
	if decoded.Layouts[0].Length != 1048576 {
		t.Errorf("Layouts[0].Length: got %d, want 1048576", decoded.Layouts[0].Length)
	}
	if !bytes.Equal(decoded.Layouts[0].Body, original.Layouts[0].Body) {
		t.Errorf("Layouts[0].Body: got %x, want %x", decoded.Layouts[0].Body, original.Layouts[0].Body)
	}
}

func TestLayoutGetRes_RoundTrip_TryLater(t *testing.T) {
	original := LayoutGetRes{
		Status:     NFS4ERR_LAYOUTTRYLATER,
		WillSignal: true,
	}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded LayoutGetRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4ERR_LAYOUTTRYLATER {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4ERR_LAYOUTTRYLATER)
	}
	if decoded.WillSignal != true {
		t.Errorf("WillSignal: got %t, want true", decoded.WillSignal)
	}
}

func TestLayoutGetRes_RoundTrip_Error(t *testing.T) {
	original := LayoutGetRes{Status: NFS4ERR_NOTSUPP}

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded LayoutGetRes
	if err := decoded.Decode(&buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Status != NFS4ERR_NOTSUPP {
		t.Errorf("Status: got %d, want %d", decoded.Status, NFS4ERR_NOTSUPP)
	}
}

func TestLayoutGetArgs_String(t *testing.T) {
	args := LayoutGetArgs{LayoutType: LAYOUT4_NFSV4_1_FILES, IOMode: LAYOUTIOMODE4_READ}
	s := args.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}
